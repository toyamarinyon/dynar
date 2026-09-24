package dynar

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	_ "modernc.org/sqlite"
)

const schemaVersion = "1"

// DB is a DynamoDB-compatible database backed by a single SQLite file
// (or an in-memory database when opened with ":memory:").
//
// The zero value is not usable; use Open. A DB is safe for concurrent
// use; operations are serialized internally on a single SQLite
// connection, which also gives each write operation atomicity.
type DB struct {
	sql    *sql.DB
	closed atomic.Bool
	path   string
}

// Open opens (or creates) a dynar database at path.
//
//   - ":memory:" creates an independent in-memory database per call.
//   - A nonexistent file is created and initialized.
//   - An existing dynar file is reopened after a schema-version check.
//   - A non-empty file that dynar did not create, or a file with a newer
//     schema version, is rejected rather than initialized or destroyed.
//
// The parent directory must already exist; Open does not create it.
func Open(path string) (*DB, error) {
	dsn := path
	if path != ":memory:" {
		dsn = "file:" + path
	}
	sdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single connection serializes all operations, keeps :memory:
	// databases from splitting across a pool, and makes SQLITE_BUSY
	// unreachable inside this process.
	sdb.SetMaxOpenConns(1)
	sdb.SetMaxIdleConns(1)

	if err := initSchema(sdb); err != nil {
		sdb.Close()
		return nil, err
	}
	return &DB{sql: sdb, path: path}, nil
}

// initSchema creates the dynar schema on an empty database and validates
// it on an existing one.
func initSchema(sdb *sql.DB) error {
	var n int
	if err := sdb.QueryRow(`SELECT count(*) FROM sqlite_master`).Scan(&n); err != nil {
		return fmt.Errorf("dynar: not a readable sqlite database: %w", err)
	}
	if n == 0 {
		stmts := []string{
			`CREATE TABLE dynar_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
			`CREATE TABLE dynar_tables (
				name TEXT PRIMARY KEY,
				hash_key TEXT NOT NULL, hash_type TEXT NOT NULL,
				range_key TEXT, range_type TEXT,
				attr_defs TEXT NOT NULL,
				status TEXT NOT NULL,
				created_at REAL NOT NULL,
				billing_mode TEXT,
				deletion_protection INTEGER NOT NULL DEFAULT 0,
				provisioned_rcu INTEGER, provisioned_wcu INTEGER,
				tags TEXT)`,
			`CREATE TABLE dynar_tokens (
				token TEXT PRIMARY KEY,
				request BLOB NOT NULL,
				created_at REAL NOT NULL)`,
			`INSERT INTO dynar_meta (key, value) VALUES ('schema_version', '` + schemaVersion + `')`,
			`PRAGMA busy_timeout = 5000`,
		}
		for _, s := range stmts {
			if _, err := sdb.Exec(s); err != nil {
				return fmt.Errorf("dynar: failed to initialize database: %w", err)
			}
		}
		return nil
	}

	var v string
	err := sdb.QueryRow(`SELECT value FROM dynar_meta WHERE key = 'schema_version'`).Scan(&v)
	if err == sql.ErrNoRows {
		return fmt.Errorf("dynar: file was not created by dynar; refusing to initialize it")
	}
	if err != nil {
		return fmt.Errorf("dynar: cannot read database schema: %w", err)
	}
	if v != schemaVersion {
		return fmt.Errorf("dynar: unsupported schema version %q (this build supports %q)", v, schemaVersion)
	}
	if _, err := sdb.Exec(`CREATE TABLE IF NOT EXISTS dynar_tokens (
		token TEXT PRIMARY KEY,
		request BLOB NOT NULL,
		created_at REAL NOT NULL)`); err != nil {
		return fmt.Errorf("dynar: failed to initialize database: %w", err)
	}
	if _, err := sdb.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		return fmt.Errorf("dynar: %w", err)
	}
	return nil
}

// Close releases the database. Requests received after Close fail with
// an error response rather than touching the file.
func (db *DB) Close() error {
	db.closed.Store(true)
	return db.sql.Close()
}

func (db *DB) isClosed() bool { return db.closed.Load() }

// tableMeta is the catalog record for one DynamoDB table.
type tableMeta struct {
	name                string
	hashKey, hashType   string
	rangeKey, rangeType string // empty when the table has no sort key
	attrDefs            []attrDef
	status              string
	createdAt           float64
	billingMode         string
	deletionProtection  bool
	provRCU, provWCU    int64
	tags                map[string]string
}

type attrDef struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// dataTableName maps a DynamoDB table name to its backing SQLite table.
// DynamoDB names are [a-zA-Z0-9_.-]{3,255}; quoting defends in depth.
func dataTableName(name string) string {
	return `"dynar_data_` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// withTx runs fn inside a transaction on the single connection.
func (db *DB) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (db *DB) loadTable(ctx context.Context, name string) (*tableMeta, *apiError) {
	t := &tableMeta{}
	var rangeKey, rangeType, billing, tags sql.NullString
	var provRCU, provWCU sql.NullInt64
	var attrDefs string
	var dp int
	err := db.sql.QueryRowContext(ctx,
		`SELECT hash_key, hash_type, range_key, range_type, attr_defs, status,
		        created_at, billing_mode, deletion_protection,
		        provisioned_rcu, provisioned_wcu, tags
		 FROM dynar_tables WHERE name = ?`, name).
		Scan(&t.hashKey, &t.hashType, &rangeKey, &rangeType, &attrDefs, &t.status,
			&t.createdAt, &billing, &dp, &provRCU, &provWCU, &tags)
	if err == sql.ErrNoRows {
		return nil, errNotFound("Cannot do operations on a non-existent table")
	}
	if err != nil {
		return nil, errInternal(err)
	}
	t.name = name
	t.deletionProtection = dp != 0
	t.rangeKey, t.rangeType = rangeKey.String, rangeType.String
	t.billingMode = billing.String
	t.provRCU, t.provWCU = provRCU.Int64, provWCU.Int64
	if err := json.Unmarshal([]byte(attrDefs), &t.attrDefs); err != nil {
		return nil, errInternal(err)
	}
	if tags.Valid {
		json.Unmarshal([]byte(tags.String), &t.tags)
	}
	return t, nil
}

func listTables(ctx context.Context, db *DB) ([]string, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT name FROM dynar_tables ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// getItem reads one item by encoded key.
func getItem(ctx context.Context, q sqlQueryer, t *tableMeta, pk, sk []byte) (map[string]types.AttributeValue, error) {
	var raw string
	err := q.QueryRowContext(ctx,
		`SELECT item FROM `+dataTableName(t.name)+` WHERE pk = ? AND sk = ?`, pk, sk).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var wire map[string]any
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		return nil, err
	}
	item, err := wireToItem(wire)
	if err != nil {
		return nil, err
	}
	return item, nil
}

type sqlQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}
