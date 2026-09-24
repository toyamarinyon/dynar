package dynar

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var tableNameRe = regexp.MustCompile(`^[a-zA-Z0-9_.\-]{3,255}$`)

func (db *DB) opCreateTable(ctx context.Context, in map[string]any) (any, *apiError) {
	name, aerr := reqString(in, "TableName")
	if aerr != nil {
		return nil, aerr
	}
	if !tableNameRe.MatchString(name) {
		return nil, errValidation("TableName must be between 3 and 255 characters and contain only a-z, A-Z, 0-9, '_', '-', '.'")
	}

	// Structural features outside the supported scope fail loudly.
	if v, ok := in["GlobalSecondaryIndexes"].([]any); ok && len(v) > 0 {
		return nil, errValidation("dynar: GlobalSecondaryIndexes are not supported")
	}
	if v, ok := in["LocalSecondaryIndexes"].([]any); ok && len(v) > 0 {
		return nil, errValidation("dynar: LocalSecondaryIndexes are not supported")
	}
	if v, ok := in["StreamSpecification"].(map[string]any); ok {
		if en, _ := v["StreamEnabled"].(bool); en {
			return nil, errValidation("dynar: StreamSpecification is not supported")
		}
	}
	if v, ok := in["SSESpecification"].(map[string]any); ok {
		if en, _ := v["Enabled"].(bool); en {
			return nil, errValidation("dynar: SSESpecification is not supported")
		}
	}

	keySchema, aerr := parseKeySchema(in["KeySchema"])
	if aerr != nil {
		return nil, aerr
	}
	attrDefs, aerr := parseAttrDefs(in["AttributeDefinitions"])
	if aerr != nil {
		return nil, aerr
	}

	defs := map[string]string{}
	for _, d := range attrDefs {
		if d.Type != "S" && d.Type != "N" && d.Type != "B" {
			return nil, errValidation("AttributeDefinitions has invalid type %s for %s", d.Type, d.Name)
		}
		defs[d.Name] = d.Type
	}
	hashKey, rangeKey := "", ""
	var hashType, rangeType string
	for _, k := range keySchema {
		t, ok := defs[k.Name]
		if !ok {
			return nil, errValidation("AttributeDefinitions does not define key attribute %s", k.Name)
		}
		switch k.KeyType {
		case "HASH":
			if hashKey != "" {
				return nil, errValidation("multiple HASH keys in KeySchema")
			}
			hashKey, hashType = k.Name, t
		case "RANGE":
			if rangeKey != "" {
				return nil, errValidation("multiple RANGE keys in KeySchema")
			}
			rangeKey, rangeType = k.Name, t
		default:
			return nil, errValidation("invalid KeyType %q", k.KeyType)
		}
	}
	if hashKey == "" {
		return nil, errValidation("KeySchema must contain a HASH key")
	}
	// With no indexes, every attribute definition must be a key.
	for _, d := range attrDefs {
		if d.Name != hashKey && d.Name != rangeKey {
			return nil, errValidation(
				"AttributeDefinitions defines %s which is not a key attribute", d.Name)
		}
	}

	billing := optString(in, "BillingMode")
	var provRCU, provWCU int64
	if pt, ok := in["ProvisionedThroughput"].(map[string]any); ok {
		var e *apiError
		if provRCU, e = optInt(pt, "ReadCapacityUnits"); e != nil {
			return nil, e
		}
		if provWCU, e = optInt(pt, "WriteCapacityUnits"); e != nil {
			return nil, e
		}
	}
	if billing == "" || billing == "PROVISIONED" {
		billing = "PROVISIONED"
		if _, ok := in["ProvisionedThroughput"]; !ok {
			return nil, errValidation(
				"ProvisionedThroughput is required when BillingMode is PROVISIONED")
		}
	} else if billing != "PAY_PER_REQUEST" {
		return nil, errValidation("invalid BillingMode %q", billing)
	}

	tags := map[string]string{}
	if v, ok := in["Tags"].([]any); ok {
		for _, e := range v {
			m, ok := e.(map[string]any)
			if !ok {
				return nil, errValidation("invalid Tags")
			}
			k, _ := m["Key"].(string)
			val, _ := m["Value"].(string)
			tags[k] = val
		}
	}
	tagsJSON, _ := json.Marshal(tags)
	defsJSON, _ := json.Marshal(attrDefs)

	err := db.withTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM dynar_tables WHERE name = ?`, name).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return errInUse(fmt.Sprintf("Table already exists: %s", name))
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO dynar_tables
			 (name, hash_key, hash_type, range_key, range_type, attr_defs,
			  status, created_at, billing_mode, deletion_protection,
			  provisioned_rcu, provisioned_wcu, tags)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			name, hashKey, hashType,
			nullStr(rangeKey), nullStr(rangeType), string(defsJSON),
			"ACTIVE", float64(time.Now().UnixMilli())/1000, billing,
			optBool(in, "DeletionProtectionEnabled"),
			nullInt(provRCU), nullInt(provWCU), string(tagsJSON)); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`CREATE TABLE `+dataTableName(name)+` (
				pk BLOB NOT NULL,
				sk BLOB NOT NULL DEFAULT X'',
				item TEXT NOT NULL,
				PRIMARY KEY (pk, sk))`)
		return err
	})
	if err != nil {
		if ae, ok := err.(*apiError); ok {
			return nil, ae
		}
		return nil, errInternal(err)
	}

	t, aerr := db.loadTable(ctx, name)
	if aerr != nil {
		return nil, aerr
	}
	return map[string]any{"Table": tableDescription(t, 0, 0)}, nil
}

func (db *DB) opDescribeTable(ctx context.Context, in map[string]any) (any, *apiError) {
	name, aerr := reqString(in, "TableName")
	if aerr != nil {
		return nil, aerr
	}
	t, aerr := db.loadTable(ctx, name)
	if aerr != nil {
		return nil, aerr
	}
	var count int64
	var size sql.NullInt64
	row := db.sql.QueryRowContext(ctx,
		`SELECT count(*), coalesce(sum(length(item)),0) FROM `+dataTableName(name))
	if err := row.Scan(&count, &size); err != nil {
		return nil, errInternal(err)
	}
	return map[string]any{"Table": tableDescription(t, count, size.Int64)}, nil
}

func (db *DB) opListTables(ctx context.Context, in map[string]any) (any, *apiError) {
	limit, aerr := optInt(in, "Limit")
	if aerr != nil {
		return nil, aerr
	}
	start := optString(in, "ExclusiveStartTableName")

	names, err := listTables(ctx, db)
	if err != nil {
		return nil, errInternal(err)
	}
	out := []string{}
	for _, n := range names {
		if n > start {
			out = append(out, n)
		}
	}
	resp := map[string]any{}
	if limit > 0 && int64(len(out)) > limit {
		resp["LastEvaluatedTableName"] = out[limit-1]
		out = out[:limit]
	}
	if out == nil {
		out = []string{}
	}
	resp["TableNames"] = out
	return resp, nil
}

func (db *DB) opDeleteTable(ctx context.Context, in map[string]any) (any, *apiError) {
	name, aerr := reqString(in, "TableName")
	if aerr != nil {
		return nil, aerr
	}
	err := db.withTx(ctx, func(tx *sql.Tx) error {
		var dp int
		err := tx.QueryRowContext(ctx,
			`SELECT deletion_protection FROM dynar_tables WHERE name = ?`, name).Scan(&dp)
		if err == sql.ErrNoRows {
			return errNotFound("Cannot do operations on a non-existent table")
		}
		if err != nil {
			return err
		}
		if dp != 0 {
			return errValidation("table cannot be deleted as DeletionProtection is enabled")
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM dynar_tables WHERE name = ?`, name); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DROP TABLE `+dataTableName(name))
		return err
	})
	if err != nil {
		if ae, ok := err.(*apiError); ok {
			return nil, ae
		}
		return nil, errInternal(err)
	}
	return map[string]any{"Table": map[string]any{
		"TableName":   name,
		"TableStatus": "DELETING",
	}}, nil
}

func tableDescription(t *tableMeta, itemCount, sizeBytes int64) map[string]any {
	ks := []any{
		map[string]any{"AttributeName": t.hashKey, "KeyType": "HASH"},
	}
	if t.rangeKey != "" {
		ks = append(ks, map[string]any{"AttributeName": t.rangeKey, "KeyType": "RANGE"})
	}
	defs := make([]any, 0, len(t.attrDefs))
	for _, d := range t.attrDefs {
		defs = append(defs, map[string]any{"AttributeName": d.Name, "AttributeType": d.Type})
	}
	return map[string]any{
		"TableName":            t.name,
		"TableStatus":          t.status,
		"KeySchema":            ks,
		"AttributeDefinitions": defs,
		"CreationDateTime":     t.createdAt,
		"TableArn":             fmt.Sprintf("arn:aws:dynamodb:dynar-local:000000000000:table/%s", t.name),
		"TableId":              tableID(t.name),
		"ItemCount":            itemCount,
		"TableSizeBytes":       sizeBytes,
		"BillingModeSummary":   map[string]any{"BillingMode": t.billingMode},
		"ProvisionedThroughput": map[string]any{
			"ReadCapacityUnits":      t.provRCU,
			"WriteCapacityUnits":     t.provWCU,
			"NumberOfDecreasesToday": 0,
		},
		"DeletionProtectionEnabled": t.deletionProtection,
	}
}

// tableID derives a stable id-shaped value from the table name.
func tableID(name string) string {
	h := make([]byte, 16)
	// deterministic per name + creation time is unnecessary; keep it
	// stable within a run by hashing the name
	sum := stableHash(name)
	copy(h, sum)
	h[6] = (h[6] & 0x0f) | 0x40
	h[8] = (h[8] & 0x3f) | 0x80
	var b strings.Builder
	b.WriteString(hex.EncodeToString(h[:4]))
	b.WriteByte('-')
	b.WriteString(hex.EncodeToString(h[4:6]))
	b.WriteByte('-')
	b.WriteString(hex.EncodeToString(h[6:8]))
	b.WriteByte('-')
	b.WriteString(hex.EncodeToString(h[8:10]))
	b.WriteByte('-')
	b.WriteString(hex.EncodeToString(h[10:16]))
	return b.String()
}

func stableHash(s string) []byte {
	h := make([]byte, 0, 20)
	// tiny fnv-like mixer; the value only needs to be stable and uuid-shaped
	var a, b uint64 = 0xcbf29ce484222325, 0x9e3779b97f4a7c15
	for i := 0; i < len(s); i++ {
		a = (a ^ uint64(s[i])) * 0x100000001b3
		b = (b + uint64(s[i])) * 0x9e3779b97f4a7c15
	}
	for i := 0; i < 8; i++ {
		h = append(h, byte(a>>(8*i)))
	}
	for i := 0; i < 8; i++ {
		h = append(h, byte(b>>(8*i)))
	}
	r := make([]byte, 4)
	rand.Read(r)
	h = append(h, r...)
	return h
}

// tags ops -----------------------------------------------------------------

func (db *DB) tableFromARN(ctx context.Context, arn string) (string, *apiError) {
	const marker = ":table/"
	i := strings.LastIndex(arn, marker)
	if i < 0 {
		return "", errValidation("invalid ResourceArn %q", arn)
	}
	name := arn[i+len(marker):]
	if _, aerr := db.loadTable(ctx, name); aerr != nil {
		return "", errNotFound(fmt.Sprintf("Table not found: %s", name))
	}
	return name, nil
}

func (db *DB) opListTagsOfResource(ctx context.Context, in map[string]any) (any, *apiError) {
	arn, aerr := reqString(in, "ResourceArn")
	if aerr != nil {
		return nil, aerr
	}
	name, aerr := db.tableFromARN(ctx, arn)
	if aerr != nil {
		return nil, aerr
	}
	t, _ := db.loadTable(ctx, name)
	tags := make([]any, 0, len(t.tags))
	for k, v := range t.tags {
		tags = append(tags, map[string]any{"Key": k, "Value": v})
	}
	return map[string]any{"Tags": tags}, nil
}

func (db *DB) opTagResource(ctx context.Context, in map[string]any) (any, *apiError) {
	arn, aerr := reqString(in, "ResourceArn")
	if aerr != nil {
		return nil, aerr
	}
	name, aerr := db.tableFromARN(ctx, arn)
	if aerr != nil {
		return nil, aerr
	}
	t, _ := db.loadTable(ctx, name)
	if t.tags == nil {
		t.tags = map[string]string{}
	}
	if v, ok := in["Tags"].([]any); ok {
		for _, e := range v {
			m, ok := e.(map[string]any)
			if !ok {
				return nil, errValidation("invalid Tags")
			}
			k, _ := m["Key"].(string)
			val, _ := m["Value"].(string)
			t.tags[k] = val
		}
	}
	b, _ := json.Marshal(t.tags)
	if _, err := db.sql.ExecContext(ctx,
		`UPDATE dynar_tables SET tags = ? WHERE name = ?`, string(b), name); err != nil {
		return nil, errInternal(err)
	}
	return map[string]any{}, nil
}

func (db *DB) opUntagResource(ctx context.Context, in map[string]any) (any, *apiError) {
	arn, aerr := reqString(in, "ResourceArn")
	if aerr != nil {
		return nil, aerr
	}
	name, aerr := db.tableFromARN(ctx, arn)
	if aerr != nil {
		return nil, aerr
	}
	t, _ := db.loadTable(ctx, name)
	if v, ok := in["TagKeys"].([]any); ok {
		for _, e := range v {
			if k, ok := e.(string); ok {
				delete(t.tags, k)
			}
		}
	}
	b, _ := json.Marshal(t.tags)
	if _, err := db.sql.ExecContext(ctx,
		`UPDATE dynar_tables SET tags = ? WHERE name = ?`, string(b), name); err != nil {
		return nil, errInternal(err)
	}
	return map[string]any{}, nil
}

// helpers -------------------------------------------------------------------

type keySchemaElem struct {
	Name    string
	KeyType string
}

func parseKeySchema(v any) ([]keySchemaElem, *apiError) {
	l, ok := v.([]any)
	if !ok || len(l) == 0 {
		return nil, errValidation("KeySchema is required")
	}
	out := make([]keySchemaElem, 0, len(l))
	for _, e := range l {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, errValidation("invalid KeySchema element")
		}
		name, _ := m["AttributeName"].(string)
		kt, _ := m["KeyType"].(string)
		if name == "" || kt == "" {
			return nil, errValidation("invalid KeySchema element")
		}
		out = append(out, keySchemaElem{Name: name, KeyType: kt})
	}
	return out, nil
}

func parseAttrDefs(v any) ([]attrDef, *apiError) {
	l, ok := v.([]any)
	if !ok || len(l) == 0 {
		return nil, errValidation("AttributeDefinitions is required")
	}
	out := make([]attrDef, 0, len(l))
	for _, e := range l {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, errValidation("invalid AttributeDefinitions element")
		}
		name, _ := m["AttributeName"].(string)
		typ, _ := m["AttributeType"].(string)
		if name == "" || typ == "" {
			return nil, errValidation("invalid AttributeDefinitions element")
		}
		out = append(out, attrDef{Name: name, Type: typ})
	}
	return out, nil
}

func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func nullInt(v int64) sql.NullInt64 {
	if v == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: v, Valid: true}
}
