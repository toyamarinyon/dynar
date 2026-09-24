# dynar design notes

## Public API

The public surface is deliberately minimal.

```go
type DB struct{ ... }

func Open(path string) (*DB, error)
func (db *DB) Close() error
func (db *DB) HTTPClient() aws.HTTPClient
```

- `Open` takes a file path or `":memory:"`.
- `HTTPClient()` returns the AWS SDK v2 `aws.HTTPClient`
  (`Do(*http.Request)`); pass it directly to
  `dynamodb.Options.HTTPClient`.
- `Option` functions or `Must` helpers are intentionally absent from
  the first version; they can be added when actually needed.

## Boundary: the HTTP / DynamoDB protocol

The SDK speaks JSON RPC with `application/x-amz-json-1.0` to DynamoDB
(`X-Amz-Target: DynamoDB_20120810.<Op>`, `POST /`). dynar implements
only this request/response format. It does not touch the SDK's
middleware, retries, authentication, or endpoint resolution.

- Request URLs and Authorization headers are ignored (not validated).
- Errors are returned as
  `{ "__type": "com.amazonaws.dynamodb.v20120810#<Type>", "message": "..." }`
  with HTTP 400. The SDK decodes them into typed exceptions, so
  `errors.As` works.
- Unsupported operations fail immediately with `ValidationException`
  (400, non-retryable). 5xx or connection errors are never used because
  the SDK would retry them and stall the caller.
- Requests after Close also fail with 400 (returned as an HTTP error
  response rather than a Go error, to avoid SDK retries).
- Context cancellation is checked via `req.Context().Done()` at the
  entry of each handler and returned wrapped in `*url.Error`.

## SQLite driver choice: `modernc.org/sqlite`

Candidates:

| Driver | CGO | Notes |
|---|---|---|
| `github.com/mattn/go-sqlite3` | Required | Battle-tested, but demands a C toolchain from consumers |
| `modernc.org/sqlite` | Not required | SQLite transpiled to Go; `database/sql` compliant |
| `zombiezen.com/go/sqlite` | Not required | Feature-rich but centered on its own API; `database/sql` interop is a separate layer |

Chosen: **modernc.org/sqlite**, prioritizing the consumer install
experience (plain `go build` works, cross-compilation works). The
dependency is large (it contains all of SQLite), which is acceptable
for a development-focused library. Because access goes through
`database/sql`, the driver could be swapped later.

## Connection model

- `SetMaxOpenConns(1)` on the `*sql.DB` serializes all operations
  through a single connection.
  - Avoids the problem where `:memory:` creates a separate database
    per connection.
  - Structurally eliminates `SQLITE_BUSY` and keeps the
    condition-check-then-write sequence atomic inside a simple
    transaction.
  - Throughput is not a concern for local development use.
- `SetMaxIdleConns(1)` and unlimited `ConnMaxLifetime`/`IdleTime` keep
  the connection from closing mid-session and losing the in-memory
  database.

## Storage schema

```
dynar_meta(key TEXT PRIMARY KEY, value TEXT)     -- schema_version
dynar_tables(name TEXT PRIMARY KEY,              -- table catalog
             hash_key TEXT, hash_type TEXT,
             range_key TEXT, range_type TEXT,    -- NULL = no sort key
             status TEXT, created_at REAL,
             billing_mode TEXT, deletion_protection INT,
             provisioned_rcu INT, provisioned_wcu INT,
             tags TEXT)                          -- JSON
dynar_data_<sanitized>(                          -- one per table
    pk   BLOB NOT NULL,
    sk   BLOB NOT NULL DEFAULT '',
    item TEXT NOT NULL,                          -- canonical JSON of the AttributeValue map
    PRIMARY KEY (pk, sk))
```

- `pk`/`sk` are stored with an **order-preserving key encoding**, so
  `ORDER BY pk, sk` in Query directly produces DynamoDB sort order.
  - `S`: `0x01` + UTF-8 (`0x00` escaped as `0x00 0xFF`, terminated by
    `0x00 0x00`)
  - `N`: `0x02` + sign + exponent (2 bytes, biased) + decimal digit
    mantissa. Normalization happens on the string form without going
    through float64, so no precision is lost. Negative numbers have
    their payload bit-inverted to reverse order.
  - `B`: `0x03` + bytes (escaped + terminated the same way)
- The `item` column holds DynamoDB-format JSON (`{"attr":{"S":"v"}}`)
  verbatim. The attribute-value codec is implemented in-house so `N`
  round-trips as a string.
- Table name → SQLite table name uses the `dynar_data_` prefix plus
  double-quote escaping (DynamoDB table names `[a-zA-Z0-9_.-]` cannot
  produce injection, but quoting is applied defensively anyway).

## `Open` semantics

- New file: created, schema initialized, `schema_version=1` recorded.
- Existing file: `dynar_meta` presence and `schema_version` checked.
  - Files not managed by dynar (no meta table) → error (not
    initialized). An empty file (size 0) is treated as new and
    initialized.
  - Newer version → error.
- `":memory:"`: an independent database per `Open` (a
  single-connection :memory:).
- A missing parent directory propagates SQLite's open error (the
  directory is not created implicitly).
- Multiple handles on the same file: left to SQLite file locking.
  WAL is not used; the default journal mode applies.

## Expression engine

Implemented with a small hand-written tokenizer + recursive-descent
parser.

- Condition / Filter / KeyCondition: comparisons, `BETWEEN`, `IN`,
  `AND/OR/NOT`, `attribute_exists` / `attribute_not_exists` /
  `attribute_type` / `begins_with` / `contains` / `size`
- Update: `SET` (`=`, `+`/`-`, `if_not_exists`, `list_append`),
  `REMOVE`, `ADD`, `DELETE`
- Projection: `a`, `a.b`, `a[0]` paths
- `#name` → ExpressionAttributeNames, `:name` →
  ExpressionAttributeValues
- Carries the DynamoDB reserved-word list; a bare identifier that is
  reserved yields `ValidationException`.
- Unsupported syntax or functions fail at parse time with
  `ValidationException` — never ignored or approximated.

## Error mapping

| DynamoDB exception | Used for |
|---|---|
| ValidationException | Invalid input, unsupported API/expression/option |
| ResourceNotFoundException | Nonexistent table |
| ResourceInUseException | Creating an existing table, operating on a deleting table |
| ConditionalCheckFailedException | Condition failure (includes Item when ReturnValuesOnConditionCheckFailure=ALL_OLD) |
| InternalServerError | Unexpected failures inside dynar |

## Compatibility testing

`internal/compat` holds shared scenarios that take a
`*dynamodb.Client`:

- Normal tests: run against dynar (`:memory:` and file)
- `//go:build dynamodblocal`: run against a client pointed at
  `DYNAMODB_LOCAL_ENDPOINT`

If the comparison tests are run explicitly and the endpoint is
unreachable, they fail rather than skip. The DynamoDB Local version is
pinned in `docs/compat.md`.
