package dynar_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	smithy "github.com/aws/smithy-go"

	"github.com/toyamarinyon/dynar"
)

// seedState stores the initial work-state item used by every scenario:
// pk=SESSION#s1, sk=STATE, version=7, status=idle, currentRevisionId=rev-3.
func seedState(t *testing.T, ctx context.Context, c *dynamodb.Client) {
	t.Helper()
	_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String("work"),
		Item: map[string]types.AttributeValue{
			"pk": s("SESSION#s1"), "sk": s("STATE"),
			"version": n("7"), "status": s("idle"),
			"currentRevisionId": s("rev-3"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

// startAnalysis is use case 1: bump the state to running, save the user
// message, and enqueue a run — all in one transaction.
func startAnalysis(ctx context.Context, c *dynamodb.Client, token, runID, msgID, text string) error {
	_, err := c.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		ClientRequestToken: aws.String(token),
		TransactItems: []types.TransactWriteItem{
			{
				Update: &types.Update{
					TableName: aws.String("work"),
					Key: map[string]types.AttributeValue{
						"pk": s("SESSION#s1"), "sk": s("STATE"),
					},
					ConditionExpression: aws.String("#v = :expected AND #status = :idle"),
					UpdateExpression: aws.String(
						"SET #v = :next, #status = :running, activeRunId = :run"),
					ExpressionAttributeNames: map[string]string{
						"#v": "version", "#status": "status",
					},
					ExpressionAttributeValues: map[string]types.AttributeValue{
						":expected": n("7"), ":next": n("8"),
						":idle": s("idle"), ":running": s("running"),
						":run": s(runID),
					},
				},
			},
			{
				Put: &types.Put{
					TableName: aws.String("work"),
					Item: map[string]types.AttributeValue{
						"pk": s("SESSION#s1"), "sk": s("MESSAGE#" + msgID),
						"role": s("user"), "text": s(text),
					},
					ConditionExpression: aws.String("attribute_not_exists(pk)"),
				},
			},
			{
				Put: &types.Put{
					TableName: aws.String("work"),
					Item: map[string]types.AttributeValue{
						"pk": s("SESSION#s1"), "sk": s("RUN#" + runID),
						"status": s("queued"), "baseRevisionId": s("rev-3"),
						"inputMessageId": s(msgID),
					},
					ConditionExpression: aws.String("attribute_not_exists(pk)"),
				},
			},
		},
	})
	return err
}

func getItem(t *testing.T, ctx context.Context, c *dynamodb.Client, pk, sk string) map[string]types.AttributeValue {
	t.Helper()
	out, err := c.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String("work"),
		Key:       map[string]types.AttributeValue{"pk": s(pk), "sk": s(sk)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.Item
}

func TestTransactWriteItemsSuccess(t *testing.T) {
	ctx := context.Background()
	_, c := openMemory(t)
	makeTable(t, ctx, c, "work", true)
	seedState(t, ctx, c)

	if err := startAnalysis(ctx, c, "start-s1-operation-001", "run-4", "msg-4",
		"承認は部長ではなく課長が行います"); err != nil {
		t.Fatal(err)
	}

	st := getItem(t, ctx, c, "SESSION#s1", "STATE")
	if st["version"].(*types.AttributeValueMemberN).Value != "8" {
		t.Fatalf("version: %v", st["version"])
	}
	if st["status"].(*types.AttributeValueMemberS).Value != "running" {
		t.Fatalf("status: %v", st["status"])
	}
	if st["activeRunId"].(*types.AttributeValueMemberS).Value != "run-4" {
		t.Fatalf("activeRunId: %v", st["activeRunId"])
	}
	msg := getItem(t, ctx, c, "SESSION#s1", "MESSAGE#msg-4")
	if msg["text"].(*types.AttributeValueMemberS).Value != "承認は部長ではなく課長が行います" {
		t.Fatalf("message: %v", msg)
	}
	run := getItem(t, ctx, c, "SESSION#s1", "RUN#run-4")
	if run["status"].(*types.AttributeValueMemberS).Value != "queued" {
		t.Fatalf("run: %v", run)
	}
}

func TestTransactWriteItemsAtomicRollback(t *testing.T) {
	ctx := context.Background()
	_, c := openMemory(t)
	makeTable(t, ctx, c, "work", true)
	seedState(t, ctx, c)

	// RUN#run-4 already exists, so the last Put's condition fails.
	if _, err := c.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String("work"),
		Item: map[string]types.AttributeValue{
			"pk": s("SESSION#s1"), "sk": s("RUN#run-4"), "status": s("queued"),
		},
	}); err != nil {
		t.Fatal(err)
	}

	err := startAnalysis(ctx, c, "tok-rollback", "run-4", "msg-4", "x")
	var txErr *types.TransactionCanceledException
	if !errors.As(err, &txErr) {
		t.Fatalf("expected TransactionCanceledException, got %v", err)
	}
	if len(txErr.CancellationReasons) != 3 {
		t.Fatalf("expected 3 cancellation reasons, got %v", txErr.CancellationReasons)
	}
	if txErr.CancellationReasons[0].Code != nil &&
		*txErr.CancellationReasons[0].Code != "None" {
		t.Fatalf("reason[0]: %v", *txErr.CancellationReasons[0].Code)
	}
	if txErr.CancellationReasons[2].Code == nil ||
		*txErr.CancellationReasons[2].Code != "ConditionalCheckFailed" {
		t.Fatalf("reason[2] should be ConditionalCheckFailed: %v", txErr.CancellationReasons[2])
	}

	// Neither the state update nor the message may survive.
	st := getItem(t, ctx, c, "SESSION#s1", "STATE")
	if st["version"].(*types.AttributeValueMemberN).Value != "7" ||
		st["status"].(*types.AttributeValueMemberS).Value != "idle" {
		t.Fatalf("state changed despite rollback: %v", st)
	}
	if _, ok := st["activeRunId"]; ok {
		t.Fatalf("activeRunId written despite rollback: %v", st)
	}
	if it := getItem(t, ctx, c, "SESSION#s1", "MESSAGE#msg-4"); it != nil {
		t.Fatalf("message written despite rollback: %v", it)
	}
}

func TestTransactWriteItemsConcurrent(t *testing.T) {
	ctx := context.Background()
	_, c := openMemory(t)
	makeTable(t, ctx, c, "work", true)
	seedState(t, ctx, c)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			run := "run-a"
			msg := "msg-a"
			if i == 1 {
				run, msg = "run-b", "msg-b"
			}
			errs[i] = startAnalysis(ctx, c, "tok-race-"+run, run, msg, "x")
		}(i)
	}
	wg.Wait()

	var txErr *types.TransactionCanceledException
	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.As(err, &txErr):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("expected exactly one winner, got %d (errs: %v)", wins, errs)
	}

	st := getItem(t, ctx, c, "SESSION#s1", "STATE")
	active := st["activeRunId"].(*types.AttributeValueMemberS).Value
	var winner, loser string
	if active == "run-a" {
		winner, loser = "a", "b"
	} else {
		winner, loser = "b", "a"
	}
	if it := getItem(t, ctx, c, "SESSION#s1", "MESSAGE#msg-"+loser); it != nil {
		t.Fatalf("loser message %q survived: %v", loser, it)
	}
	if it := getItem(t, ctx, c, "SESSION#s1", "RUN#run-"+loser); it != nil {
		t.Fatalf("loser run %q survived: %v", loser, it)
	}
	if it := getItem(t, ctx, c, "SESSION#s1", "MESSAGE#msg-"+winner); it == nil {
		t.Fatalf("winner message %q missing", winner)
	}
}

func TestTransactWriteItemsIdempotentRetry(t *testing.T) {
	ctx := context.Background()
	_, c := openMemory(t)
	makeTable(t, ctx, c, "work", true)
	seedState(t, ctx, c)

	if err := startAnalysis(ctx, c, "tok-idem", "run-4", "msg-4", "x"); err != nil {
		t.Fatal(err)
	}
	// Re-sending the identical request with the same token must succeed
	// without re-executing — re-executing would fail the version check.
	if err := startAnalysis(ctx, c, "tok-idem", "run-4", "msg-4", "x"); err != nil {
		t.Fatalf("idempotent retry failed: %v", err)
	}
	st := getItem(t, ctx, c, "SESSION#s1", "STATE")
	if st["version"].(*types.AttributeValueMemberN).Value != "8" {
		t.Fatalf("version after retry: %v", st["version"])
	}
}

func TestTransactWriteItemsTokenMismatch(t *testing.T) {
	ctx := context.Background()
	_, c := openMemory(t)
	makeTable(t, ctx, c, "work", true)
	seedState(t, ctx, c)

	if err := startAnalysis(ctx, c, "tok-misuse", "run-4", "msg-4", "original"); err != nil {
		t.Fatal(err)
	}
	err := startAnalysis(ctx, c, "tok-misuse", "run-4", "msg-4", "changed body")
	var mism *types.IdempotentParameterMismatchException
	if !errors.As(err, &mism) {
		t.Fatalf("expected IdempotentParameterMismatchException, got %v", err)
	}
}

func TestTransactWriteItemsStaleCompletion(t *testing.T) {
	ctx := context.Background()
	_, c := openMemory(t)
	makeTable(t, ctx, c, "work", true)
	seedState(t, ctx, c)

	// run-4 is in flight with attempt-2, and the state points at it, so
	// the state-side condition of the completion transaction holds and
	// only the stale attemptId can fail.
	if _, err := c.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String("work"),
		Item: map[string]types.AttributeValue{
			"pk": s("SESSION#s1"), "sk": s("RUN#run-4"),
			"status": s("running"), "attemptId": s("attempt-2"),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String("work"),
		Key:       map[string]types.AttributeValue{"pk": s("SESSION#s1"), "sk": s("STATE")},
		UpdateExpression: aws.String(
			"SET activeRunId = :r, #status = :running"),
		ExpressionAttributeNames: map[string]string{"#status": "status"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":r": s("run-4"), ":running": s("running"),
		},
	}); err != nil {
		t.Fatal(err)
	}
	// A retry superseded attempt-2 before the old worker's completion arrived.
	if _, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:        aws.String("work"),
		Key:              map[string]types.AttributeValue{"pk": s("SESSION#s1"), "sk": s("RUN#run-4")},
		UpdateExpression: aws.String("SET attemptId = :a"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":a": s("attempt-3"),
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Stale completion: every op's condition must hold for the tx to commit.
	_, err := c.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: []types.TransactWriteItem{
			{
				Update: &types.Update{
					TableName: aws.String("work"),
					Key:       map[string]types.AttributeValue{"pk": s("SESSION#s1"), "sk": s("STATE")},
					ConditionExpression: aws.String(
						"activeRunId = :run AND currentRevisionId = :rev"),
					UpdateExpression: aws.String("SET currentRevisionId = :new"),
					ExpressionAttributeValues: map[string]types.AttributeValue{
						":run": s("run-4"), ":rev": s("rev-3"), ":new": s("rev-4"),
					},
				},
			},
			{
				Update: &types.Update{
					TableName: aws.String("work"),
					Key:       map[string]types.AttributeValue{"pk": s("SESSION#s1"), "sk": s("RUN#run-4")},
					ConditionExpression: aws.String(
						"#status = :running AND attemptId = :attempt"),
					UpdateExpression:         aws.String("SET #status = :succeeded"),
					ExpressionAttributeNames: map[string]string{"#status": "status"},
					ExpressionAttributeValues: map[string]types.AttributeValue{
						":running": s("running"), ":attempt": s("attempt-2"),
						":succeeded": s("succeeded"),
					},
				},
			},
			{
				Put: &types.Put{
					TableName: aws.String("work"),
					Item: map[string]types.AttributeValue{
						"pk": s("SESSION#s1"), "sk": s("REV#rev-4"),
						"summary": s("new understanding"),
					},
					ConditionExpression: aws.String("attribute_not_exists(pk)"),
				},
			},
			{
				Put: &types.Put{
					TableName: aws.String("work"),
					Item: map[string]types.AttributeValue{
						"pk": s("SESSION#s1"), "sk": s("MESSAGE#ai-1"),
						"role": s("assistant"), "text": s("理解案を生成しました"),
					},
				},
			},
		},
	})
	var txErr *types.TransactionCanceledException
	if !errors.As(err, &txErr) {
		t.Fatalf("expected TransactionCanceledException, got %v", err)
	}
	if txErr.CancellationReasons[0].Code == nil ||
		*txErr.CancellationReasons[0].Code != "None" {
		t.Fatalf("state update should pass its condition: %v", txErr.CancellationReasons)
	}
	if txErr.CancellationReasons[1].Code == nil ||
		*txErr.CancellationReasons[1].Code != "ConditionalCheckFailed" {
		t.Fatalf("run update should be the failing op: %v", txErr.CancellationReasons)
	}

	// Nothing may have changed.
	st := getItem(t, ctx, c, "SESSION#s1", "STATE")
	if st["currentRevisionId"].(*types.AttributeValueMemberS).Value != "rev-3" {
		t.Fatalf("stale completion rewrote state: %v", st)
	}
	run := getItem(t, ctx, c, "SESSION#s1", "RUN#run-4")
	if run["status"].(*types.AttributeValueMemberS).Value != "running" ||
		run["attemptId"].(*types.AttributeValueMemberS).Value != "attempt-3" {
		t.Fatalf("run changed: %v", run)
	}
	if it := getItem(t, ctx, c, "SESSION#s1", "REV#rev-4"); it != nil {
		t.Fatalf("rev-4 written: %v", it)
	}
	if it := getItem(t, ctx, c, "SESSION#s1", "MESSAGE#ai-1"); it != nil {
		t.Fatalf("ai message written: %v", it)
	}
}

func TestTransactWriteItemsFilePersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dynamo.db")

	db, err := dynar.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(t, db)
	makeTable(t, ctx, c, "work", true)
	seedState(t, ctx, c)
	if err := startAnalysis(ctx, c, "tok-persist", "run-4", "msg-4", "x"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := dynar.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	c2 := newClient(t, db2)
	st := getItem(t, ctx, c2, "SESSION#s1", "STATE")
	if st["version"].(*types.AttributeValueMemberN).Value != "8" {
		t.Fatalf("state lost after reopen: %v", st)
	}
	if getItem(t, ctx, c2, "SESSION#s1", "MESSAGE#msg-4") == nil {
		t.Fatal("message lost after reopen")
	}
	if getItem(t, ctx, c2, "SESSION#s1", "RUN#run-4") == nil {
		t.Fatal("run lost after reopen")
	}
	// The client token survives reopen: an identical resend is a
	// successful no-op, not a re-execution (version would fail).
	if err := startAnalysis(ctx, c2, "tok-persist", "run-4", "msg-4", "x"); err != nil {
		t.Fatalf("idempotent retry after reopen failed: %v", err)
	}
}

func TestTransactWriteItemsDeleteAndConditionCheck(t *testing.T) {
	ctx := context.Background()
	_, c := openMemory(t)
	makeTable(t, ctx, c, "work", true)
	seedState(t, ctx, c)
	if _, err := c.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String("work"),
		Item:      map[string]types.AttributeValue{"pk": s("SESSION#s1"), "sk": s("MESSAGE#old")},
	}); err != nil {
		t.Fatal(err)
	}

	tx := func(expectedVersion string) error {
		_, err := c.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
			TransactItems: []types.TransactWriteItem{
				{
					ConditionCheck: &types.ConditionCheck{
						TableName:                aws.String("work"),
						Key:                      map[string]types.AttributeValue{"pk": s("SESSION#s1"), "sk": s("STATE")},
						ConditionExpression:      aws.String("#v = :v"),
						ExpressionAttributeNames: map[string]string{"#v": "version"},
						ExpressionAttributeValues: map[string]types.AttributeValue{
							":v": n(expectedVersion),
						},
					},
				},
				{
					Delete: &types.Delete{
						TableName:           aws.String("work"),
						Key:                 map[string]types.AttributeValue{"pk": s("SESSION#s1"), "sk": s("MESSAGE#old")},
						ConditionExpression: aws.String("attribute_exists(pk)"),
					},
				},
				{
					Put: &types.Put{
						TableName: aws.String("work"),
						Item:      map[string]types.AttributeValue{"pk": s("SESSION#s1"), "sk": s("MESSAGE#new")},
					},
				},
			},
		})
		return err
	}

	// Wrong expected version: the ConditionCheck cancels everything.
	err := tx("999")
	var txErr *types.TransactionCanceledException
	if !errors.As(err, &txErr) {
		t.Fatalf("expected TransactionCanceledException, got %v", err)
	}
	if txErr.CancellationReasons[0].Code == nil ||
		*txErr.CancellationReasons[0].Code != "ConditionalCheckFailed" {
		t.Fatalf("reason[0]: %v", txErr.CancellationReasons)
	}
	if it := getItem(t, ctx, c, "SESSION#s1", "MESSAGE#old"); it == nil {
		t.Fatal("delete applied despite rollback")
	}
	if it := getItem(t, ctx, c, "SESSION#s1", "MESSAGE#new"); it != nil {
		t.Fatal("put applied despite rollback")
	}

	// Correct version: everything applies.
	if err := tx("7"); err != nil {
		t.Fatal(err)
	}
	if it := getItem(t, ctx, c, "SESSION#s1", "MESSAGE#old"); it != nil {
		t.Fatal("delete did not apply")
	}
	if it := getItem(t, ctx, c, "SESSION#s1", "MESSAGE#new"); it == nil {
		t.Fatal("put did not apply")
	}
}

func TestTransactWriteItemsRollbackAfterWrite(t *testing.T) {
	ctx := context.Background()
	_, c := openMemory(t)
	makeTable(t, ctx, c, "work", true)
	seedState(t, ctx, c)

	// The first Put has already been applied when the Update fails at
	// apply time (adding a number to a string), and must be rolled back.
	_, err := c.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: []types.TransactWriteItem{
			{
				Put: &types.Put{
					TableName: aws.String("work"),
					Item:      map[string]types.AttributeValue{"pk": s("SESSION#s1"), "sk": s("MESSAGE#w")},
				},
			},
			{
				Update: &types.Update{
					TableName:                aws.String("work"),
					Key:                      map[string]types.AttributeValue{"pk": s("SESSION#s1"), "sk": s("STATE")},
					UpdateExpression:         aws.String("SET #status = #status + :inc"),
					ExpressionAttributeNames: map[string]string{"#status": "status"},
					ExpressionAttributeValues: map[string]types.AttributeValue{
						":inc": n("1"),
					},
				},
			},
		},
	})
	var txErr *types.TransactionCanceledException
	if !errors.As(err, &txErr) {
		t.Fatalf("expected TransactionCanceledException, got %v", err)
	}
	if txErr.CancellationReasons[1].Code == nil ||
		*txErr.CancellationReasons[1].Code != "ValidationError" {
		t.Fatalf("reason[1] should be ValidationError: %v", txErr.CancellationReasons)
	}
	if it := getItem(t, ctx, c, "SESSION#s1", "MESSAGE#w"); it != nil {
		t.Fatalf("write survived rollback: %v", it)
	}
	st := getItem(t, ctx, c, "SESSION#s1", "STATE")
	if st["status"].(*types.AttributeValueMemberS).Value != "idle" {
		t.Fatalf("state changed despite rollback: %v", st)
	}
}

func TestItemSizeBoundary(t *testing.T) {
	ctx := context.Background()
	_, c := openMemory(t)
	makeTable(t, ctx, c, "work", true)

	// 310 KiB of raw binary is within DynamoDB's 400 KiB item limit even
	// though its base64 wire encoding exceeds 400 KiB.
	big := make([]byte, 310*1024)
	if _, err := c.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String("work"),
		Item: map[string]types.AttributeValue{
			"pk": s("p"), "sk": s("bin"), "blob": b(big),
		},
	}); err != nil {
		t.Fatalf("310 KiB binary item must fit: %v", err)
	}
	// 420 KiB does not fit.
	_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String("work"),
		Item: map[string]types.AttributeValue{
			"pk": s("p"), "sk": s("huge"), "blob": b(make([]byte, 420*1024)),
		},
	})
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "ValidationException" {
		t.Fatalf("oversized item: %v", err)
	}

	// Same accounting inside a transaction: the large-but-legal binary
	// commits, and the oversized one cancels with ValidationError.
	_, err = c.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: []types.TransactWriteItem{
			{Put: &types.Put{
				TableName: aws.String("work"),
				Item: map[string]types.AttributeValue{
					"pk": s("p"), "sk": s("tx-bin"), "blob": b(big),
				},
			}},
		},
	})
	if err != nil {
		t.Fatalf("310 KiB binary in transaction must fit: %v", err)
	}
	_, err = c.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: []types.TransactWriteItem{
			{Put: &types.Put{
				TableName: aws.String("work"),
				Item:      map[string]types.AttributeValue{"pk": s("p"), "sk": s("tx-ok")},
			}},
			{Put: &types.Put{
				TableName: aws.String("work"),
				Item: map[string]types.AttributeValue{
					"pk": s("p"), "sk": s("tx-huge"), "blob": b(make([]byte, 420*1024)),
				},
			}},
		},
	})
	var txErr *types.TransactionCanceledException
	if !errors.As(err, &txErr) {
		t.Fatalf("expected TransactionCanceledException, got %v", err)
	}
	if txErr.CancellationReasons[1].Code == nil ||
		*txErr.CancellationReasons[1].Code != "ValidationError" {
		t.Fatalf("reason[1]: %v", txErr.CancellationReasons)
	}
	if it := getItem(t, ctx, c, "p", "tx-ok"); it != nil {
		t.Fatalf("earlier put survived rollback: %v", it)
	}
}

func TestTransactWriteItemsAggregateSizeLimit(t *testing.T) {
	ctx := context.Background()
	_, c := openMemory(t)
	makeTable(t, ctx, c, "work", true)

	// 11 existing items of ~390 KiB each: deletes write nothing, but the
	// touched items still exceed the 4 MB transaction aggregate.
	const items = 11
	blob := b(make([]byte, 390*1024))
	for i := 0; i < items; i++ {
		if _, err := c.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: aws.String("work"),
			Item: map[string]types.AttributeValue{
				"pk": s("p"), "sk": s(fmt.Sprintf("%02d", i)), "blob": blob,
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	var txItems []types.TransactWriteItem
	for i := 0; i < items; i++ {
		txItems = append(txItems, types.TransactWriteItem{
			Delete: &types.Delete{
				TableName: aws.String("work"),
				Key: map[string]types.AttributeValue{
					"pk": s("p"), "sk": s(fmt.Sprintf("%02d", i)),
				},
			},
		})
	}
	_, err := c.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: txItems,
	})
	var txErr *types.TransactionCanceledException
	if !errors.As(err, &txErr) {
		t.Fatalf("expected TransactionCanceledException, got %v", err)
	}
	if txErr.CancellationReasons[items-1].Code == nil ||
		*txErr.CancellationReasons[items-1].Code != "ValidationError" {
		t.Fatalf("reason[%d]: %v", items-1, txErr.CancellationReasons[items-1])
	}
	// Nothing was deleted.
	for i := 0; i < items; i++ {
		if it := getItem(t, ctx, c, "p", fmt.Sprintf("%02d", i)); it == nil {
			t.Fatalf("item %d deleted despite rollback", i)
		}
	}
}

func TestTransactWriteItemsValidation(t *testing.T) {
	ctx := context.Background()
	_, c := openMemory(t)
	makeTable(t, ctx, c, "work", true)

	var apiErr smithy.APIError

	// Two operations on the same item are rejected.
	_, err := c.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: []types.TransactWriteItem{
			{Put: &types.Put{
				TableName: aws.String("work"),
				Item:      map[string]types.AttributeValue{"pk": s("a"), "sk": s("1")},
			}},
			{Delete: &types.Delete{
				TableName: aws.String("work"),
				Key:       map[string]types.AttributeValue{"pk": s("a"), "sk": s("1")},
			}},
		},
	})
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "ValidationException" {
		t.Fatalf("duplicate item: %v", err)
	}

	// Token over 36 characters.
	_, err = c.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		ClientRequestToken: aws.String("0123456789012345678901234567890123456"),
		TransactItems: []types.TransactWriteItem{
			{Put: &types.Put{
				TableName: aws.String("work"),
				Item:      map[string]types.AttributeValue{"pk": s("a"), "sk": s("1")},
			}},
		},
	})
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "ValidationException" {
		t.Fatalf("long token: %v", err)
	}

	// ReturnItemCollectionMetrics is out of scope and must be rejected,
	// not silently ignored.
	_, err = c.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		ReturnItemCollectionMetrics: types.ReturnItemCollectionMetricsSize,
		TransactItems: []types.TransactWriteItem{
			{Put: &types.Put{
				TableName: aws.String("work"),
				Item:      map[string]types.AttributeValue{"pk": s("a"), "sk": s("1")},
			}},
		},
	})
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "ValidationException" {
		t.Fatalf("item collection metrics: %v", err)
	}
}
