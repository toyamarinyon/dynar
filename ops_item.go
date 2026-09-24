package dynar

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const maxItemSize = 400 * 1024

// parseCond parses an optional expression field; empty string → nil node.
func parseCond(in map[string]any, field string, env *exprEnv) (condNode, *apiError) {
	s := optString(in, field)
	if s == "" {
		return nil, nil
	}
	n, err := parseConditionExpr(s, env)
	if err != nil {
		return nil, errValidation("invalid %s: %v", field, err)
	}
	return n, nil
}

// tableKey converts a request Key map into encoded columns, enforcing
// that it contains exactly the table's key attributes.
func tableKey(t *tableMeta, key map[string]types.AttributeValue) (pk, sk []byte, aerr *apiError) {
	if len(key) == 0 {
		return nil, nil, errValidation("Key must not be empty")
	}
	for k := range key {
		if k != t.hashKey && k != t.rangeKey {
			return nil, nil, errValidation(
				"The provided key element does not match the schema")
		}
	}
	hv, ok := key[t.hashKey]
	if !ok {
		return nil, nil, errValidation(
			"The provided key element does not match the schema")
	}
	if keyTypeOf(hv) != t.hashType {
		return nil, nil, errValidation(
			"The provided key element does not match the schema")
	}
	pk, err := encodeKeyValue(hv)
	if err != nil {
		return nil, nil, errValidation("invalid key: %v", err)
	}
	if t.rangeKey == "" {
		return pk, []byte{}, nil
	}
	sv, ok := key[t.rangeKey]
	if !ok || keyTypeOf(sv) != t.rangeType {
		return nil, nil, errValidation(
			"The provided key element does not match the schema")
	}
	sk, err = encodeKeyValue(sv)
	if err != nil {
		return nil, nil, errValidation("invalid key: %v", err)
	}
	return pk, sk, nil
}

// conditionalFail builds the failure error honoring
// ReturnValuesOnConditionCheckFailure.
func conditionalFail(in map[string]any, old map[string]types.AttributeValue) *apiError {
	e := errConditionalCheck("The conditional request failed", nil)
	if optString(in, "ReturnValuesOnConditionCheckFailure") == "ALL_OLD" && old != nil {
		e.item = itemToWire(old)
	}
	return e
}

func consumedCapacity(in map[string]any, table string) map[string]any {
	switch optString(in, "ReturnConsumedCapacity") {
	case "TOTAL", "INDEXES":
		return map[string]any{
			"TableName":     table,
			"CapacityUnits": 1.0,
		}
	default:
		return nil
	}
}

// ---- PutItem ----

func (db *DB) opPutItem(ctx context.Context, in map[string]any) (any, *apiError) {
	name, aerr := reqString(in, "TableName")
	if aerr != nil {
		return nil, aerr
	}
	t, aerr := db.loadTable(ctx, name)
	if aerr != nil {
		return nil, aerr
	}
	if aerr := rejectLegacy(in, "Expected", "ConditionalOperator"); aerr != nil {
		return nil, aerr
	}
	item, aerr := reqItem(in, "Item")
	if aerr != nil {
		return nil, aerr
	}
	if err := validateItem(item); err != nil {
		return nil, errValidation("One or more parameter values were invalid: %v", err)
	}
	pk, sk, err := itemKey(t, item)
	if err != nil {
		return nil, errValidation("One or more parameter values were invalid: %v", err)
	}
	if sk == nil {
		sk = []byte{}
	}

	wire := itemToWire(item)
	enc, _ := json.Marshal(wire)
	if len(enc) > maxItemSize {
		return nil, errValidation("Item size has exceeded the maximum allowed size")
	}

	env, aerr := getExprEnv(in)
	if aerr != nil {
		return nil, aerr
	}
	cond, aerr := parseCond(in, "ConditionExpression", env)
	if aerr != nil {
		return nil, aerr
	}
	rv := optString(in, "ReturnValues")
	switch rv {
	case "", "NONE":
	case "ALL_OLD":
	default:
		return nil, errValidation("invalid ReturnValues %q for PutItem", rv)
	}

	var old map[string]types.AttributeValue
	err = db.withTx(ctx, func(tx *sql.Tx) error {
		var e error
		old, e = getItem(ctx, tx, t, pk, sk)
		if e != nil {
			return e
		}
		if cond != nil {
			ok, e := evalCond(cond, old, env)
			if e != nil {
				return errValidation("invalid ConditionExpression: %v", e)
			}
			if !ok {
				return conditionalFail(in, old)
			}
		}
		_, e = tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO `+dataTableName(t.name)+` (pk, sk, item) VALUES (?,?,?)`,
			pk, sk, string(enc))
		return e
	})
	if err != nil {
		if ae, ok := err.(*apiError); ok {
			return nil, ae
		}
		return nil, errInternal(err)
	}

	out := map[string]any{}
	if rv == "ALL_OLD" && old != nil {
		out["Attributes"] = itemToWire(old)
	}
	if cc := consumedCapacity(in, name); cc != nil {
		out["ConsumedCapacity"] = cc
	}
	return out, nil
}

// ---- GetItem ----

func (db *DB) opGetItem(ctx context.Context, in map[string]any) (any, *apiError) {
	name, aerr := reqString(in, "TableName")
	if aerr != nil {
		return nil, aerr
	}
	t, aerr := db.loadTable(ctx, name)
	if aerr != nil {
		return nil, aerr
	}
	if aerr := rejectLegacy(in, "AttributesToGet"); aerr != nil {
		return nil, aerr
	}
	key, aerr := reqItem(in, "Key")
	if aerr != nil {
		return nil, aerr
	}
	pk, sk, aerr := tableKey(t, key)
	if aerr != nil {
		return nil, aerr
	}
	env, aerr := getExprEnv(in)
	if aerr != nil {
		return nil, aerr
	}
	proj, aerr := parseProjection(optString(in, "ProjectionExpression"), env)
	if aerr != nil {
		return nil, aerr
	}

	var raw string
	err := db.sql.QueryRowContext(ctx,
		`SELECT item FROM `+dataTableName(t.name)+` WHERE pk = ? AND sk = ?`,
		pk, sk).Scan(&raw)
	if err == sql.ErrNoRows {
		out := map[string]any{}
		if cc := consumedCapacity(in, name); cc != nil {
			out["ConsumedCapacity"] = cc
		}
		return out, nil
	}
	if err != nil {
		return nil, errInternal(err)
	}
	var wm map[string]any
	if err := json.Unmarshal([]byte(raw), &wm); err != nil {
		return nil, errInternal(err)
	}
	item, err := wireToItem(wm)
	if err != nil {
		return nil, errInternal(err)
	}
	if proj != nil {
		item = projectItem(item, proj)
	}
	out := map[string]any{"Item": itemToWire(item)}
	if cc := consumedCapacity(in, name); cc != nil {
		out["ConsumedCapacity"] = cc
	}
	return out, nil
}

// ---- UpdateItem ----

func (db *DB) opUpdateItem(ctx context.Context, in map[string]any) (any, *apiError) {
	name, aerr := reqString(in, "TableName")
	if aerr != nil {
		return nil, aerr
	}
	t, aerr := db.loadTable(ctx, name)
	if aerr != nil {
		return nil, aerr
	}
	key, aerr := reqItem(in, "Key")
	if aerr != nil {
		return nil, aerr
	}
	pk, sk, aerr := tableKey(t, key)
	if aerr != nil {
		return nil, aerr
	}
	if aerr := rejectLegacy(in, "AttributeUpdates", "Expected", "ConditionalOperator"); aerr != nil {
		return nil, aerr
	}
	env, aerr := getExprEnv(in)
	if aerr != nil {
		return nil, aerr
	}
	ue := optString(in, "UpdateExpression")
	if ue == "" {
		return nil, errValidation("UpdateExpression is required")
	}
	actions, err := parseUpdateExpr(ue, env)
	if err != nil {
		return nil, errValidation("invalid UpdateExpression: %v", err)
	}
	// key attributes cannot be updated
	for _, a := range actions {
		top := a.path[0]
		if !top.isIdx && (top.name == t.hashKey || top.name == t.rangeKey) {
			return nil, errValidation(
				"UpdateExpression cannot update key attribute %s", top.name)
		}
	}
	cond, aerr := parseCond(in, "ConditionExpression", env)
	if aerr != nil {
		return nil, aerr
	}
	rv := optString(in, "ReturnValues")
	switch rv {
	case "", "NONE", "ALL_OLD", "ALL_NEW", "UPDATED_OLD", "UPDATED_NEW":
	default:
		return nil, errValidation("invalid ReturnValues %q for UpdateItem", rv)
	}

	var old, newItem map[string]types.AttributeValue
	var updated map[string]bool
	execErr := db.withTx(ctx, func(tx *sql.Tx) error {
		var e error
		old, e = getItem(ctx, tx, t, pk, sk)
		if e != nil {
			return e
		}
		if cond != nil {
			ok, e := evalCond(cond, old, env)
			if e != nil {
				return errValidation("invalid ConditionExpression: %v", e)
			}
			if !ok {
				return conditionalFail(in, old)
			}
		}
		// start from existing item or just the key attributes
		base := old
		if base == nil {
			base = map[string]types.AttributeValue{}
			for k, v := range key {
				base[k] = v
			}
		}
		newItem, updated, e = applyUpdate(base, actions, env)
		if e != nil {
			return errValidation("invalid UpdateExpression: %v", e)
		}
		if e := validateItem(newItem); e != nil {
			return errValidation("One or more parameter values were invalid: %v", e)
		}
		wire := itemToWire(newItem)
		enc, _ := json.Marshal(wire)
		if len(enc) > maxItemSize {
			return errValidation("Item size has exceeded the maximum allowed size")
		}
		_, e = tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO `+dataTableName(t.name)+` (pk, sk, item) VALUES (?,?,?)`,
			pk, sk, string(enc))
		return e
	})
	if execErr != nil {
		if ae, ok := execErr.(*apiError); ok {
			return nil, ae
		}
		return nil, errInternal(execErr)
	}

	out := map[string]any{}
	switch rv {
	case "ALL_OLD":
		if old != nil {
			out["Attributes"] = itemToWire(old)
		}
	case "ALL_NEW":
		out["Attributes"] = itemToWire(newItem)
	case "UPDATED_OLD":
		out["Attributes"] = itemToWire(pickUpdated(old, updated))
	case "UPDATED_NEW":
		out["Attributes"] = itemToWire(pickUpdated(newItem, updated))
	}
	if cc := consumedCapacity(in, name); cc != nil {
		out["ConsumedCapacity"] = cc
	}
	return out, nil
}

// pickUpdated returns the subset of item covering updated top-level attrs.
func pickUpdated(item map[string]types.AttributeValue, updated map[string]bool) map[string]types.AttributeValue {
	out := map[string]types.AttributeValue{}
	if item == nil {
		return out
	}
	for k := range updated {
		if v, ok := item[k]; ok {
			out[k] = v
		}
	}
	return out
}

// ---- DeleteItem ----

func (db *DB) opDeleteItem(ctx context.Context, in map[string]any) (any, *apiError) {
	name, aerr := reqString(in, "TableName")
	if aerr != nil {
		return nil, aerr
	}
	t, aerr := db.loadTable(ctx, name)
	if aerr != nil {
		return nil, aerr
	}
	key, aerr := reqItem(in, "Key")
	if aerr != nil {
		return nil, aerr
	}
	pk, sk, aerr := tableKey(t, key)
	if aerr != nil {
		return nil, aerr
	}
	if aerr := rejectLegacy(in, "Expected", "ConditionalOperator"); aerr != nil {
		return nil, aerr
	}
	env, aerr := getExprEnv(in)
	if aerr != nil {
		return nil, aerr
	}
	cond, aerr := parseCond(in, "ConditionExpression", env)
	if aerr != nil {
		return nil, aerr
	}
	rv := optString(in, "ReturnValues")
	switch rv {
	case "", "NONE":
	case "ALL_OLD":
	default:
		return nil, errValidation("invalid ReturnValues %q for DeleteItem", rv)
	}

	var old map[string]types.AttributeValue
	err := db.withTx(ctx, func(tx *sql.Tx) error {
		var e error
		old, e = getItem(ctx, tx, t, pk, sk)
		if e != nil {
			return e
		}
		if cond != nil {
			ok, e := evalCond(cond, old, env)
			if e != nil {
				return errValidation("invalid ConditionExpression: %v", e)
			}
			if !ok {
				return conditionalFail(in, old)
			}
		}
		_, e = tx.ExecContext(ctx,
			`DELETE FROM `+dataTableName(t.name)+` WHERE pk = ? AND sk = ?`, pk, sk)
		return e
	})
	if err != nil {
		if ae, ok := err.(*apiError); ok {
			return nil, ae
		}
		return nil, errInternal(err)
	}

	out := map[string]any{}
	if rv == "ALL_OLD" && old != nil {
		out["Attributes"] = itemToWire(old)
	}
	if cc := consumedCapacity(in, name); cc != nil {
		out["ConsumedCapacity"] = cc
	}
	return out, nil
}

// ---- Query ----

// keyCondition holds a parsed KeyConditionExpression.
type keyCondition struct {
	skOp string // "", "=", "<", "<=", ">", ">=", "between", "begins_with"
	skLo types.AttributeValue
	skHi types.AttributeValue
}

func (db *DB) opQuery(ctx context.Context, in map[string]any) (any, *apiError) {
	name, aerr := reqString(in, "TableName")
	if aerr != nil {
		return nil, aerr
	}
	t, aerr := db.loadTable(ctx, name)
	if aerr != nil {
		return nil, aerr
	}
	if _, ok := in["IndexName"]; ok {
		return nil, errValidation("dynar: IndexName (GSI/LSI) is not supported")
	}
	if aerr := rejectLegacy(in, "KeyConditions", "QueryFilter", "ConditionalOperator", "AttributesToGet"); aerr != nil {
		return nil, aerr
	}
	env, aerr := getExprEnv(in)
	if aerr != nil {
		return nil, aerr
	}

	kce := optString(in, "KeyConditionExpression")
	if kce == "" {
		return nil, errValidation("KeyConditionExpression is required for Query")
	}
	pkVal, kc, aerr := db.parseKeyCondition(kce, t, env)
	if aerr != nil {
		return nil, aerr
	}
	pkEnc, err := encodeKeyValue(pkVal)
	if err != nil {
		return nil, errValidation("invalid partition key value: %v", err)
	}

	filter, aerr := parseCond(in, "FilterExpression", env)
	if aerr != nil {
		return nil, aerr
	}
	proj, aerr := parseProjection(optString(in, "ProjectionExpression"), env)
	if aerr != nil {
		return nil, aerr
	}
	sel := optString(in, "Select")
	switch sel {
	case "", "ALL_ATTRIBUTES":
	case "COUNT", "SPECIFIC_ATTRIBUTES":
	default:
		return nil, errValidation("invalid Select %q", sel)
	}
	if sel == "SPECIFIC_ATTRIBUTES" && len(proj) == 0 {
		return nil, errValidation("ProjectionExpression is required when Select is SPECIFIC_ATTRIBUTES")
	}

	limit, aerr := optInt(in, "Limit")
	if aerr != nil {
		return nil, aerr
	}
	forward := true
	if v, ok := in["ScanIndexForward"].(bool); ok {
		forward = v
	}

	// Build the sk predicate SQL.
	var conds []string
	var args []any
	conds = append(conds, "pk = ?")
	args = append(args, pkEnc)
	addSk := func(op string, av types.AttributeValue) *apiError {
		if t.rangeKey == "" {
			return errValidation("table has no sort key")
		}
		if keyTypeOf(av) != t.rangeType {
			return errValidation("sort key condition value type must be %s", t.rangeType)
		}
		enc, err := encodeKeyValue(av)
		if err != nil {
			return errValidation("invalid sort key value: %v", err)
		}
		conds = append(conds, "sk "+op+" ?")
		args = append(args, enc)
		return nil
	}
	switch kc.skOp {
	case "=":
		if e := addSk("=", kc.skLo); e != nil {
			return nil, e
		}
	case "<", "<=", ">", ">=":
		if e := addSk(kc.skOp, kc.skLo); e != nil {
			return nil, e
		}
	case "between":
		if e := addSk(">=", kc.skLo); e != nil {
			return nil, e
		}
		if e := addSk("<=", kc.skHi); e != nil {
			return nil, e
		}
	case "begins_with":
		pref, err := keyPrefix(kc.skLo)
		if err != nil {
			return nil, errValidation("begins_with: %v", err)
		}
		conds = append(conds, "sk >= ?")
		args = append(args, pref)
		if s := successor(pref); s != nil {
			conds = append(conds, "sk < ?")
			args = append(args, s)
		}
	case "":
	default:
		return nil, errValidation("unsupported sort key condition")
	}

	// ExclusiveStartKey: continue strictly after that key.
	if esk, aerr := optItem(in, "ExclusiveStartKey"); aerr != nil {
		return nil, aerr
	} else if esk != nil {
		pk0, sk0, aerr := tableKey(t, esk)
		if aerr != nil {
			return nil, aerr
		}
		if !keyEqual(pk0, pkEnc) {
			return nil, errValidation("ExclusiveStartKey does not match the query partition key")
		}
		if t.rangeKey != "" {
			if forward {
				conds = append(conds, "sk > ?")
			} else {
				conds = append(conds, "sk < ?")
			}
			args = append(args, sk0)
		}
	}

	order := "ASC"
	if !forward {
		order = "DESC"
	}
	sqlq := `SELECT pk, sk, item FROM ` + dataTableName(t.name) +
		` WHERE ` + strings.Join(conds, " AND ") + ` ORDER BY sk ` + order
	fetch := limit + 1
	if limit <= 0 {
		fetch = 0
	}
	if fetch > 0 {
		sqlq += fmt.Sprintf(" LIMIT %d", fetch)
	}

	rows, err := db.sql.QueryContext(ctx, sqlq, args...)
	if err != nil {
		return nil, errInternal(err)
	}
	defer rows.Close()

	type row struct {
		item map[string]types.AttributeValue
	}
	var scanned []row
	for rows.Next() {
		var pkB, skB []byte
		var raw string
		if err := rows.Scan(&pkB, &skB, &raw); err != nil {
			return nil, errInternal(err)
		}
		var wm map[string]any
		if err := json.Unmarshal([]byte(raw), &wm); err != nil {
			return nil, errInternal(err)
		}
		it, err := wireToItem(wm)
		if err != nil {
			return nil, errInternal(err)
		}
		scanned = append(scanned, row{item: it})
	}
	if err := rows.Err(); err != nil {
		return nil, errInternal(err)
	}

	hasMore := false
	if limit > 0 && int64(len(scanned)) > limit {
		scanned = scanned[:limit]
		hasMore = true
	}

	var items []any
	var lastKey map[string]any
	for _, r := range scanned {
		it := r.item
		lastKey = itemToWire(extractKey(t, it))
		if filter != nil {
			ok, err := evalCond(filter, it, env)
			if err != nil {
				return nil, errValidation("invalid FilterExpression: %v", err)
			}
			if !ok {
				continue
			}
		}
		if proj != nil && sel != "COUNT" {
			it = projectItem(it, proj)
		}
		items = append(items, itemToWire(it))
	}

	out := map[string]any{
		"Count":        len(items),
		"ScannedCount": len(scanned),
	}
	if sel != "COUNT" {
		if items == nil {
			items = []any{}
		}
		out["Items"] = items
	}
	if hasMore && lastKey != nil {
		out["LastEvaluatedKey"] = lastKey
	}
	if cc := consumedCapacity(in, name); cc != nil {
		out["ConsumedCapacity"] = cc
	}
	return out, nil
}

// parseKeyCondition splits a KeyConditionExpression into the partition
// key equality value and an optional sort key condition.
func (db *DB) parseKeyCondition(s string, t *tableMeta, env *exprEnv) (types.AttributeValue, keyCondition, *apiError) {
	n, err := parseConditionExpr(s, env)
	if err != nil {
		return nil, keyCondition{}, errValidation("invalid KeyConditionExpression: %v", err)
	}
	// flatten top-level ANDs
	var terms []condNode
	switch c := n.(type) {
	case andNode:
		terms = c.terms
	default:
		terms = []condNode{n}
	}
	if len(terms) > 2 {
		return nil, keyCondition{}, errValidation(
			"KeyConditionExpression must only contain one partition key and one sort key condition")
	}

	var pkVal types.AttributeValue
	var kc keyCondition
	pkSeen := false

	for _, term := range terms {
		// which key does this term reference?
		isPk := func(o operand) bool {
			return o.kind == opndPath && len(o.path) == 1 && !o.path[0].isIdx && o.path[0].name == t.hashKey
		}
		isSk := func(o operand) bool {
			return t.rangeKey != "" && o.kind == opndPath && len(o.path) == 1 && !o.path[0].isIdx && o.path[0].name == t.rangeKey
		}
		switch c := term.(type) {
		case cmpNode:
			if c.op != "=" {
				if isSk(c.left) {
					if kc.skOp != "" {
						return nil, keyCondition{}, errValidation("multiple sort key conditions")
					}
					v, ok, err := resolveOperand(c.right, nil, env)
					if err != nil {
						return nil, keyCondition{}, errValidation("invalid KeyConditionExpression: %v", err)
					}
					if !ok {
						return nil, keyCondition{}, errValidation("sort key condition must compare to a value")
					}
					kc.skOp, kc.skLo = c.op, v
					continue
				}
				return nil, keyCondition{}, errValidation(
					"KeyConditionExpression only supports = for the partition key")
			}
			if isPk(c.left) {
				if pkSeen {
					return nil, keyCondition{}, errValidation("multiple partition key conditions")
				}
				v, ok, err := resolveOperand(c.right, nil, env)
				if err != nil {
					return nil, keyCondition{}, errValidation("invalid KeyConditionExpression: %v", err)
				}
				if !ok {
					return nil, keyCondition{}, errValidation("partition key must compare to a value")
				}
				if keyTypeOf(v) != t.hashType {
					return nil, keyCondition{}, errValidation(
						"partition key condition value type must be %s", t.hashType)
				}
				pkVal, pkSeen = v, true
				continue
			}
			if isSk(c.left) {
				if kc.skOp != "" {
					return nil, keyCondition{}, errValidation("multiple sort key conditions")
				}
				v, ok, err := resolveOperand(c.right, nil, env)
				if err != nil {
					return nil, keyCondition{}, errValidation("invalid KeyConditionExpression: %v", err)
				}
				if !ok {
					return nil, keyCondition{}, errValidation("sort key condition must compare to a value")
				}
				kc.skOp, kc.skLo = "=", v
				continue
			}
			return nil, keyCondition{}, errValidation(
				"KeyConditionExpression must reference the key attributes")
		case betweenNode:
			if !isSk(c.v) {
				return nil, keyCondition{}, errValidation(
					"BETWEEN is only allowed on the sort key")
			}
			if kc.skOp != "" {
				return nil, keyCondition{}, errValidation("multiple sort key conditions")
			}
			lo, okL, err := resolveOperand(c.lo, nil, env)
			if err != nil {
				return nil, keyCondition{}, errValidation("invalid KeyConditionExpression: %v", err)
			}
			hi, okH, err := resolveOperand(c.hi, nil, env)
			if err != nil {
				return nil, keyCondition{}, errValidation("invalid KeyConditionExpression: %v", err)
			}
			if !okL || !okH {
				return nil, keyCondition{}, errValidation("BETWEEN requires values")
			}
			kc.skOp, kc.skLo, kc.skHi = "between", lo, hi
		case funcNode:
			if c.name != "begins_with" || len(c.args) != 2 || !isSk(c.args[0]) {
				return nil, keyCondition{}, errValidation(
					"only begins_with is allowed as a sort key function")
			}
			if kc.skOp != "" {
				return nil, keyCondition{}, errValidation("multiple sort key conditions")
			}
			v, ok, err := resolveOperand(c.args[1], nil, env)
			if err != nil {
				return nil, keyCondition{}, errValidation("invalid KeyConditionExpression: %v", err)
			}
			if !ok {
				return nil, keyCondition{}, errValidation("begins_with requires a value")
			}
			kc.skOp, kc.skLo = "begins_with", v
		default:
			return nil, keyCondition{}, errValidation(
				"invalid term in KeyConditionExpression")
		}
	}
	if !pkSeen {
		return nil, keyCondition{}, errValidation(
			"KeyConditionExpression must contain an equality condition on the partition key")
	}
	return pkVal, kc, nil
}

// extractKey returns only the key attributes of an item.
func extractKey(t *tableMeta, item map[string]types.AttributeValue) map[string]types.AttributeValue {
	out := map[string]types.AttributeValue{}
	if v, ok := item[t.hashKey]; ok {
		out[t.hashKey] = v
	}
	if t.rangeKey != "" {
		if v, ok := item[t.rangeKey]; ok {
			out[t.rangeKey] = v
		}
	}
	return out
}

// ---- Scan ----

func (db *DB) opScan(ctx context.Context, in map[string]any) (any, *apiError) {
	name, aerr := reqString(in, "TableName")
	if aerr != nil {
		return nil, aerr
	}
	t, aerr := db.loadTable(ctx, name)
	if aerr != nil {
		return nil, aerr
	}
	if _, ok := in["IndexName"]; ok {
		return nil, errValidation("dynar: IndexName (GSI/LSI) is not supported")
	}
	if aerr := rejectLegacy(in, "ScanFilter", "ConditionalOperator", "AttributesToGet"); aerr != nil {
		return nil, aerr
	}
	env, aerr := getExprEnv(in)
	if aerr != nil {
		return nil, aerr
	}
	filter, aerr := parseCond(in, "FilterExpression", env)
	if aerr != nil {
		return nil, aerr
	}
	proj, aerr := parseProjection(optString(in, "ProjectionExpression"), env)
	if aerr != nil {
		return nil, aerr
	}
	sel := optString(in, "Select")
	switch sel {
	case "", "ALL_ATTRIBUTES":
	case "COUNT", "SPECIFIC_ATTRIBUTES":
	default:
		return nil, errValidation("invalid Select %q", sel)
	}
	if sel == "SPECIFIC_ATTRIBUTES" && len(proj) == 0 {
		return nil, errValidation("ProjectionExpression is required when Select is SPECIFIC_ATTRIBUTES")
	}
	if _, ok := in["Segment"]; ok {
		return nil, errValidation("dynar: parallel Scan (Segment/TotalSegments) is not supported")
	}
	limit, aerr := optInt(in, "Limit")
	if aerr != nil {
		return nil, aerr
	}

	var conds []string
	var args []any
	if esk, aerr := optItem(in, "ExclusiveStartKey"); aerr != nil {
		return nil, aerr
	} else if esk != nil {
		pk0, sk0, aerr := tableKey(t, esk)
		if aerr != nil {
			return nil, aerr
		}
		conds = append(conds, "(pk > ? OR (pk = ? AND sk > ?))")
		args = append(args, pk0, pk0, sk0)
	}

	sqlq := `SELECT pk, sk, item FROM ` + dataTableName(t.name)
	if len(conds) > 0 {
		sqlq += ` WHERE ` + strings.Join(conds, " AND ")
	}
	sqlq += ` ORDER BY pk, sk`
	fetch := limit + 1
	if limit <= 0 {
		fetch = 0
	}
	if fetch > 0 {
		sqlq += fmt.Sprintf(" LIMIT %d", fetch)
	}

	rows, err := db.sql.QueryContext(ctx, sqlq, args...)
	if err != nil {
		return nil, errInternal(err)
	}
	defer rows.Close()

	var scanned []map[string]types.AttributeValue
	for rows.Next() {
		var pkB, skB []byte
		var raw string
		if err := rows.Scan(&pkB, &skB, &raw); err != nil {
			return nil, errInternal(err)
		}
		var wm map[string]any
		if err := json.Unmarshal([]byte(raw), &wm); err != nil {
			return nil, errInternal(err)
		}
		it, err := wireToItem(wm)
		if err != nil {
			return nil, errInternal(err)
		}
		scanned = append(scanned, it)
	}
	if err := rows.Err(); err != nil {
		return nil, errInternal(err)
	}

	hasMore := false
	if limit > 0 && int64(len(scanned)) > limit {
		scanned = scanned[:limit]
		hasMore = true
	}

	var items []any
	var lastKey map[string]any
	for _, it := range scanned {
		lastKey = itemToWire(extractKey(t, it))
		if filter != nil {
			ok, err := evalCond(filter, it, env)
			if err != nil {
				return nil, errValidation("invalid FilterExpression: %v", err)
			}
			if !ok {
				continue
			}
		}
		out := it
		if proj != nil && sel != "COUNT" {
			out = projectItem(it, proj)
		}
		items = append(items, itemToWire(out))
	}

	out := map[string]any{
		"Count":        len(items),
		"ScannedCount": len(scanned),
	}
	if sel != "COUNT" {
		if items == nil {
			items = []any{}
		}
		out["Items"] = items
	}
	if hasMore && lastKey != nil {
		out["LastEvaluatedKey"] = lastKey
	}
	if cc := consumedCapacity(in, name); cc != nil {
		out["ConsumedCapacity"] = cc
	}
	return out, nil
}

// ---- projection ----

func parseProjection(s string, env *exprEnv) ([][]pathElem, *apiError) {
	if s == "" {
		return nil, nil
	}
	toks, err := lexExpr(s)
	if err != nil {
		return nil, errValidation("invalid ProjectionExpression: %v", err)
	}
	p := &parser{toks: toks, env: env}
	var paths [][]pathElem
	for !p.atEnd() {
		path, err := p.parsePath()
		if err != nil {
			return nil, errValidation("invalid ProjectionExpression: %v", err)
		}
		paths = append(paths, path)
		if p.peek().kind == tkComma {
			p.next()
			continue
		}
		break
	}
	if !p.atEnd() {
		return nil, errValidation("invalid ProjectionExpression: unexpected token %q", p.peek().text)
	}
	return paths, nil
}

func projectItem(item map[string]types.AttributeValue, paths [][]pathElem) map[string]types.AttributeValue {
	out := map[string]types.AttributeValue{}
	for _, p := range paths {
		v, ok := resolvePath(item, p)
		if !ok {
			continue
		}
		wrapped := wrapPath(v, p[1:])
		mergeAttr(out, p[0].name, wrapped)
	}
	return out
}

// wrapPath re-nests v along the remaining path elements.
func wrapPath(v types.AttributeValue, rest []pathElem) types.AttributeValue {
	if len(rest) == 0 {
		return v
	}
	e := rest[0]
	inner := wrapPath(v, rest[1:])
	if e.isIdx {
		return &types.AttributeValueMemberL{Value: []types.AttributeValue{inner}}
	}
	return &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{e.name: inner}}
}

// mergeAttr merges v into dst[name], combining M trees and appending Ls.
func mergeAttr(dst map[string]types.AttributeValue, name string, v types.AttributeValue) {
	cur, ok := dst[name]
	if !ok {
		dst[name] = v
		return
	}
	dst[name] = mergeAV(cur, v)
}

func mergeAV(a, b types.AttributeValue) types.AttributeValue {
	am, aok := a.(*types.AttributeValueMemberM)
	bm, bok := b.(*types.AttributeValueMemberM)
	if aok && bok {
		out := make(map[string]types.AttributeValue, len(am.Value)+len(bm.Value))
		for k, v := range am.Value {
			out[k] = v
		}
		for k, v := range bm.Value {
			if cur, ok := out[k]; ok {
				out[k] = mergeAV(cur, v)
			} else {
				out[k] = v
			}
		}
		return &types.AttributeValueMemberM{Value: out}
	}
	al, aok := a.(*types.AttributeValueMemberL)
	bl, bok := b.(*types.AttributeValueMemberL)
	if aok && bok {
		return &types.AttributeValueMemberL{Value: append(append([]types.AttributeValue{}, al.Value...), bl.Value...)}
	}
	return b
}
