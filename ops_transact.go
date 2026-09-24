package dynar

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// TransactWriteItems limits, mirroring the DynamoDB API.
const (
	maxTransactItems      = 100
	maxTransactTotalBytes = 4 * 1024 * 1024 // aggregate size of the items the transaction targets
	clientTokenMaxLen     = 36
	clientTokenTTLSeconds = 600 // a client token is valid for 10 minutes
)

type txKind int

const (
	txCheck txKind = iota
	txPut
	txUpdate
	txDelete
)

// txOp is one validated TransactWriteItem, ready to run inside the
// transaction. Conditions are evaluated against the pre-transaction
// item; DynamoDB forbids two operations on the same item, so no
// operation ever observes another operation's write.
type txOp struct {
	kind   txKind
	t      *tableMeta
	pk, sk []byte
	env    *exprEnv
	cond   condNode
	rvOCF  string // ReturnValuesOnConditionCheckFailure: "", "NONE", "ALL_OLD"

	// Put
	item map[string]types.AttributeValue

	// Update
	key     map[string]types.AttributeValue
	actions []updateAction
}

// cancelReason becomes one entry of CancellationReasons.
type cancelReason struct {
	code string
	msg  string
	item map[string]any // old item when ReturnValuesOnConditionCheckFailure=ALL_OLD
}

// txCancel is returned from the transaction body to roll back every
// write; it carries the per-item cancellation reasons.
type txCancel struct {
	reasons []*cancelReason
}

func (e *txCancel) Error() string { return "transaction cancelled" }

func (e *txCancel) apiError() *apiError {
	list := make([]any, len(e.reasons))
	codes := make([]string, len(e.reasons))
	for i, r := range e.reasons {
		if r == nil {
			codes[i] = "None"
			list[i] = map[string]any{"Code": "None"}
			continue
		}
		codes[i] = r.code
		m := map[string]any{"Code": r.code, "Message": r.msg}
		if r.item != nil {
			m["Item"] = r.item
		}
		list[i] = m
	}
	return &apiError{
		typ: "TransactionCanceledException",
		msg: "Transaction cancelled, please refer cancellation reasons for specific reasons [" +
			strings.Join(codes, ", ") + "]",
		extra: map[string]any{"CancellationReasons": list},
	}
}

// rejectUnknown fails when m holds a key outside allowed. Unsupported
// features must never be silently ignored.
func rejectUnknown(m map[string]any, allowed ...string) *apiError {
	for k := range m {
		ok := false
		for _, a := range allowed {
			if k == a {
				ok = true
				break
			}
		}
		if !ok {
			return errValidation("dynar: unsupported parameter %s", k)
		}
	}
	return nil
}

// ---- TransactWriteItems ----

func (db *DB) opTransactWriteItems(ctx context.Context, in map[string]any) (any, *apiError) {
	if aerr := rejectUnknown(in, "TransactItems", "ClientRequestToken",
		"ReturnConsumedCapacity", "ReturnItemCollectionMetrics"); aerr != nil {
		return nil, aerr
	}
	raw, _ := in["TransactItems"].([]any)
	if len(raw) == 0 {
		return nil, errValidation("TransactItems must contain at least one item")
	}
	if len(raw) > maxTransactItems {
		return nil, errValidation(
			"Member must have length less than or equal to %d", maxTransactItems)
	}
	if _, ok := in["ReturnConsumedCapacity"]; ok {
		switch optString(in, "ReturnConsumedCapacity") {
		case "NONE", "TOTAL", "INDEXES":
		default:
			return nil, errValidation("invalid ReturnConsumedCapacity")
		}
	}
	switch v := optString(in, "ReturnItemCollectionMetrics"); v {
	case "", "NONE":
	default:
		return nil, errValidation(
			"dynar: ReturnItemCollectionMetrics %s is not supported", v)
	}
	var token string
	if v, ok := in["ClientRequestToken"]; ok {
		s, ok := v.(string)
		if !ok {
			return nil, errValidation("parameter ClientRequestToken must be a string")
		}
		if len(s) < 1 || len(s) > clientTokenMaxLen {
			return nil, errValidation(
				"ClientRequestToken must be between 1 and %d characters", clientTokenMaxLen)
		}
		token = s
	}
	fingerprint, _ := json.Marshal(in)

	ops := make([]*txOp, len(raw))
	targets := map[string]bool{}
	for i, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			return nil, errValidation("TransactItems[%d] must be an object", i)
		}
		op, aerr := db.parseTxItem(ctx, i, m)
		if aerr != nil {
			return nil, aerr
		}
		id := op.t.name + "\x00" + string(op.pk) + "\x00" + string(op.sk)
		if targets[id] {
			return nil, errValidation(
				"Transaction request cannot include multiple operations on one item")
		}
		targets[id] = true
		ops[i] = op
	}

	now := float64(time.Now().Unix())
	execErr := db.withTx(ctx, func(tx *sql.Tx) error {
		if _, e := tx.ExecContext(ctx,
			`DELETE FROM dynar_tokens WHERE created_at < ?`,
			now-clientTokenTTLSeconds); e != nil {
			return e
		}
		if token != "" {
			var stored []byte
			e := tx.QueryRowContext(ctx,
				`SELECT request FROM dynar_tokens WHERE token = ?`, token).Scan(&stored)
			switch {
			case e == sql.ErrNoRows:
			case e != nil:
				return e
			case !bytes.Equal(stored, fingerprint):
				return &apiError{typ: "IdempotentParameterMismatchException",
					msg: "The request was rejected because the ClientRequestToken was already used with different request parameters"}
			default:
				// Same token and same request within the idempotency
				// window: report success without re-executing.
				return nil
			}
		}

		// Phase 1: read every target and evaluate every condition.
		// Any failure cancels the whole transaction.
		olds := make([]map[string]types.AttributeValue, len(ops))
		reasons := make([]*cancelReason, len(ops))
		failed := false
		for i, op := range ops {
			old, e := getItem(ctx, tx, op.t, op.pk, op.sk)
			if e != nil {
				return e
			}
			olds[i] = old
			if op.cond == nil {
				continue
			}
			ok, e := evalCond(op.cond, old, op.env)
			if e != nil {
				return errValidation("invalid ConditionExpression: %v", e)
			}
			if !ok {
				r := &cancelReason{
					code: "ConditionalCheckFailed",
					msg:  "The conditional request failed",
				}
				if op.rvOCF == "ALL_OLD" && old != nil {
					r.item = itemToWire(old)
				}
				reasons[i] = r
				failed = true
			}
		}
		if failed {
			return &txCancel{reasons: reasons}
		}

		// Phase 2: apply every write. Validation problems discovered
		// here cancel the transaction with a ValidationError reason,
		// as the real service does.
		total := int64(0)
		for i, op := range ops {
			// DynamoDB counts every item the transaction touches
			// toward the 4 MB aggregate: the written item for
			// Put/Update, the existing target for Delete/ConditionCheck.
			var size int64
			var item map[string]types.AttributeValue
			switch op.kind {
			case txCheck, txDelete:
				if olds[i] != nil {
					size = itemSize(olds[i])
				}
			case txPut:
				item = op.item
				size = itemSize(item)
			case txUpdate:
				base := olds[i]
				if base == nil {
					base = map[string]types.AttributeValue{}
					for k, v := range op.key {
						base[k] = v
					}
				}
				newItem, _, e := applyUpdate(base, op.actions, op.env)
				if e == nil {
					e = validateItem(newItem)
				}
				if e != nil {
					reasons[i] = &cancelReason{code: "ValidationError", msg: e.Error()}
					return &txCancel{reasons: reasons}
				}
				item = newItem
				size = itemSize(item)
			}
			total += size
			switch {
			case item != nil && size > maxItemSize:
				reasons[i] = &cancelReason{code: "ValidationError",
					msg: "Item size has exceeded the maximum allowed size"}
				return &txCancel{reasons: reasons}
			case total > maxTransactTotalBytes:
				reasons[i] = &cancelReason{code: "ValidationError",
					msg: "Item size has exceeded the maximum allowed size"}
				return &txCancel{reasons: reasons}
			}
			switch op.kind {
			case txDelete:
				if _, e := tx.ExecContext(ctx,
					`DELETE FROM `+dataTableName(op.t.name)+` WHERE pk = ? AND sk = ?`,
					op.pk, op.sk); e != nil {
					return e
				}
			case txPut, txUpdate:
				enc, _ := json.Marshal(itemToWire(item))
				if _, e := tx.ExecContext(ctx,
					`INSERT OR REPLACE INTO `+dataTableName(op.t.name)+` (pk, sk, item) VALUES (?,?,?)`,
					op.pk, op.sk, string(enc)); e != nil {
					return e
				}
			}
		}
		if token != "" {
			// The idempotency window is 10 minutes counted from when
			// the request completes, so stamp the completion time.
			if _, e := tx.ExecContext(ctx,
				`INSERT INTO dynar_tokens (token, request, created_at) VALUES (?,?,?)`,
				token, fingerprint, float64(time.Now().Unix())); e != nil {
				return e
			}
		}
		return nil
	})
	if execErr != nil {
		switch e := execErr.(type) {
		case *txCancel:
			return nil, e.apiError()
		case *apiError:
			return nil, e
		default:
			return nil, errInternal(execErr)
		}
	}

	out := map[string]any{}
	if cc := consumedCapacity(in, ops[0].t.name); cc != nil {
		out["ConsumedCapacity"] = []any{cc}
	}
	return out, nil
}

// parseTxItem validates one TransactWriteItem and pre-computes
// everything that does not depend on stored data.
func (db *DB) parseTxItem(ctx context.Context, idx int, m map[string]any) (*txOp, *apiError) {
	var kind txKind
	var sub map[string]any
	found := 0
	for k, v := range m {
		var isOp bool
		switch k {
		case "ConditionCheck":
			kind, isOp = txCheck, true
		case "Put":
			kind, isOp = txPut, true
		case "Update":
			kind, isOp = txUpdate, true
		case "Delete":
			kind, isOp = txDelete, true
		}
		if !isOp {
			return nil, errValidation("dynar: unsupported parameter %s in TransactItems[%d]", k, idx)
		}
		found++
		sm, ok := v.(map[string]any)
		if !ok {
			return nil, errValidation("TransactItems[%d].%s must be an object", idx, k)
		}
		sub = sm
	}
	if found != 1 {
		return nil, errValidation(
			"TransactItems[%d] must contain exactly one of ConditionCheck, Put, Update, or Delete", idx)
	}

	allowed := []string{"TableName", "ConditionExpression",
		"ExpressionAttributeNames", "ExpressionAttributeValues",
		"ReturnValuesOnConditionCheckFailure"}
	switch kind {
	case txCheck:
		allowed = append(allowed, "Key")
	case txDelete:
		allowed = append(allowed, "Key")
	case txPut:
		allowed = append(allowed, "Item")
	case txUpdate:
		allowed = append(allowed, "Key", "UpdateExpression")
	}
	if aerr := rejectUnknown(sub, allowed...); aerr != nil {
		return nil, aerr
	}
	name, aerr := reqString(sub, "TableName")
	if aerr != nil {
		return nil, aerr
	}
	t, aerr := db.loadTable(ctx, name)
	if aerr != nil {
		return nil, aerr
	}
	env, aerr := getExprEnv(sub)
	if aerr != nil {
		return nil, aerr
	}
	rvocf := optString(sub, "ReturnValuesOnConditionCheckFailure")
	switch rvocf {
	case "", "NONE", "ALL_OLD":
	default:
		return nil, errValidation("invalid ReturnValuesOnConditionCheckFailure %q", rvocf)
	}

	op := &txOp{kind: kind, t: t, env: env, rvOCF: rvocf}

	if kind != txPut {
		key, aerr := reqItem(sub, "Key")
		if aerr != nil {
			return nil, aerr
		}
		op.pk, op.sk, aerr = tableKey(t, key)
		if aerr != nil {
			return nil, aerr
		}
		op.key = key
	}

	switch kind {
	case txCheck:
		if optString(sub, "ConditionExpression") == "" {
			return nil, errValidation("ConditionCheck requires a ConditionExpression")
		}
	case txDelete:
		// key handled above
	case txPut:
		item, aerr := reqItem(sub, "Item")
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
		op.pk, op.sk = pk, sk
		op.item = item
	case txUpdate:
		ue := optString(sub, "UpdateExpression")
		if ue == "" {
			return nil, errValidation("UpdateExpression is required")
		}
		actions, err := parseUpdateExpr(ue, env)
		if err != nil {
			return nil, errValidation("invalid UpdateExpression: %v", err)
		}
		for _, a := range actions {
			top := a.path[0]
			if !top.isIdx && (top.name == t.hashKey || top.name == t.rangeKey) {
				return nil, errValidation(
					"UpdateExpression cannot update key attribute %s", top.name)
			}
		}
		op.actions = actions
	}

	cond, aerr := parseCond(sub, "ConditionExpression", env)
	if aerr != nil {
		return nil, aerr
	}
	op.cond = cond
	return op, nil
}
