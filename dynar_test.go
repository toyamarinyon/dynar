package dynar_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	smithy "github.com/aws/smithy-go"

	"github.com/toyamarinyon/dynar"
)

func newClient(t *testing.T, db *dynar.DB) *dynamodb.Client {
	t.Helper()
	return dynamodb.New(dynamodb.Options{
		Region:      "ap-northeast-1",
		Credentials: credentials.NewStaticCredentialsProvider("local", "local", ""),
		HTTPClient:  db.HTTPClient(),
	})
}

func openMemory(t *testing.T) (*dynar.DB, *dynamodb.Client) {
	t.Helper()
	db, err := dynar.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, newClient(t, db)
}

func s(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }
func n(v string) types.AttributeValue { return &types.AttributeValueMemberN{Value: v} }
func b(v []byte) types.AttributeValue { return &types.AttributeValueMemberB{Value: v} }
func boolean(v bool) types.AttributeValue {
	return &types.AttributeValueMemberBOOL{Value: v}
}
func null() types.AttributeValue { return &types.AttributeValueMemberNULL{Value: true} }

func makeTable(t *testing.T, ctx context.Context, c *dynamodb.Client, name string, withSort bool) {
	t.Helper()
	ks := []types.KeySchemaElement{
		{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash},
	}
	defs := []types.AttributeDefinition{
		{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
	}
	if withSort {
		ks = append(ks, types.KeySchemaElement{
			AttributeName: aws.String("sk"), KeyType: types.KeyTypeRange})
		defs = append(defs, types.AttributeDefinition{
			AttributeName: aws.String("sk"), AttributeType: types.ScalarAttributeTypeS})
	}
	_, err := c.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:            aws.String(name),
		KeySchema:            ks,
		AttributeDefinitions: defs,
		BillingMode:          types.BillingModePayPerRequest,
	})
	if err != nil {
		t.Fatalf("CreateTable %s: %v", name, err)
	}
}

// ---- file persistence ----

func TestFilePersistence(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "dynamo.db")

	db, err := dynar.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(t, db)
	makeTable(t, ctx, c, "notes", true)
	if _, err := c.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String("notes"),
		Item:      map[string]types.AttributeValue{"pk": s("a"), "sk": s("1"), "text": s("hello")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// file exists
	if st, err := os.Stat(path); err != nil || st.Size() == 0 {
		t.Fatalf("expected non-empty db file: %v", err)
	}

	db2, err := dynar.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	c2 := newClient(t, db2)
	out, err := c2.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String("notes"),
		Key:       map[string]types.AttributeValue{"pk": s("a"), "sk": s("1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Item["text"].(*types.AttributeValueMemberS).Value != "hello" {
		t.Fatalf("unexpected item: %v", out.Item)
	}
}

func TestOpenRejectsNonDynarFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "other.db")
	if err := os.WriteFile(path, []byte("not a database"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := dynar.Open(path); err == nil {
		t.Fatal("expected error for non-sqlite file")
	}
}

func TestOpenMissingParentDir(t *testing.T) {
	_, err := dynar.Open(filepath.Join(t.TempDir(), "nope", "dynamo.db"))
	if err == nil {
		t.Fatal("expected error for missing parent directory")
	}
}

// ---- in-memory isolation ----

func TestMemoryIsolation(t *testing.T) {
	ctx := context.Background()
	_, c1 := openMemory(t)
	_, c2 := openMemory(t)

	makeTable(t, ctx, c1, "notes", false)
	if _, err := c1.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String("notes"),
		Item:      map[string]types.AttributeValue{"pk": s("a")},
	}); err != nil {
		t.Fatal(err)
	}
	// second handle sees nothing
	lt, err := c2.ListTables(ctx, &dynamodb.ListTablesInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(lt.TableNames) != 0 {
		t.Fatalf("memory DBs must be independent, saw %v", lt.TableNames)
	}
}

func TestMemoryParallel(t *testing.T) {
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			db, err := dynar.Open(":memory:")
			if err != nil {
				t.Error(err)
				return
			}
			defer db.Close()
			c := dynamodb.New(dynamodb.Options{
				Region:      "ap-northeast-1",
				Credentials: credentials.NewStaticCredentialsProvider("l", "l", ""),
				HTTPClient:  db.HTTPClient(),
			})
			name := fmt.Sprintf("tbl-%d", i)
			if _, err := c.CreateTable(ctx, &dynamodb.CreateTableInput{
				TableName: aws.String(name),
				KeySchema: []types.KeySchemaElement{
					{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
				AttributeDefinitions: []types.AttributeDefinition{
					{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS}},
				BillingMode: types.BillingModePayPerRequest,
			}); err != nil {
				t.Error(err)
				return
			}
			if _, err := c.PutItem(ctx, &dynamodb.PutItemInput{
				TableName: aws.String(name),
				Item:      map[string]types.AttributeValue{"pk": s(fmt.Sprint(i))},
			}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
}

// ---- attribute round trip ----

func TestAttributeRoundTrip(t *testing.T) {
	ctx := context.Background()
	_, c := openMemory(t)
	makeTable(t, ctx, c, "tbl", false)

	item := map[string]types.AttributeValue{
		"pk":       s("key"),
		"str":      s("hello 世界"),
		"empty_s":  s(""),
		"num":      n("9007199254740993123.14159"), // beyond float64 precision
		"negsmall": n("-1.5e-20"),
		"zero":     n("0"),
		"bin":      b([]byte{0, 1, 2, 255}),
		"flag":     boolean(true),
		"nothing":  null(),
		"ss":       &types.AttributeValueMemberSS{Value: []string{"a", "b"}},
		"ns":       &types.AttributeValueMemberNS{Value: []string{"1", "2.5"}},
		"bs":       &types.AttributeValueMemberBS{Value: [][]byte{{1}, {2}}},
		"list": &types.AttributeValueMemberL{Value: []types.AttributeValue{
			s("x"), n("42"), null(),
		}},
		"nested": &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{
			"deep": &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{
				"v": n("1.2300"),
			}},
		}},
	}
	if _, err := c.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String("tbl"), Item: item}); err != nil {
		t.Fatal(err)
	}
	out, err := c.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String("tbl"), Key: map[string]types.AttributeValue{"pk": s("key")}})
	if err != nil {
		t.Fatal(err)
	}
	// N round-trips exactly, no float64 mangling
	if got := out.Item["num"].(*types.AttributeValueMemberN).Value; got != "9007199254740993123.14159" {
		t.Fatalf("number precision lost: %s", got)
	}
	if got := out.Item["nested"].(*types.AttributeValueMemberM).Value["deep"].(*types.AttributeValueMemberM).Value["v"].(*types.AttributeValueMemberN).Value; got != "1.2300" {
		t.Fatalf("nested number lost: %s", got)
	}
	if got := out.Item["empty_s"].(*types.AttributeValueMemberS).Value; got != "" {
		t.Fatalf("empty string lost: %q", got)
	}
	if got := out.Item["bin"].(*types.AttributeValueMemberB).Value; len(got) != 4 || got[3] != 255 {
		t.Fatalf("binary lost: %v", got)
	}
}

// ---- conditional writes ----

func TestConditionalWrites(t *testing.T) {
	ctx := context.Background()
	_, c := openMemory(t)
	makeTable(t, ctx, c, "tbl", true)

	put := func() error {
		_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
			TableName:           aws.String("tbl"),
			ConditionExpression: aws.String("attribute_not_exists(sk)"),
			Item: map[string]types.AttributeValue{
				"pk": s("a"), "sk": s("1"), "text": s("v"),
			},
		})
		return err
	}
	if err := put(); err != nil {
		t.Fatal(err)
	}
	err := put()
	var cond *types.ConditionalCheckFailedException
	if !errors.As(err, &cond) {
		t.Fatalf("expected ConditionalCheckFailedException, got %v", err)
	}

	// update guarded by attribute_exists
	_, err = c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String("tbl"),
		Key:                       map[string]types.AttributeValue{"pk": s("a"), "sk": s("1")},
		ConditionExpression:       aws.String("attribute_exists(sk)"),
		UpdateExpression:          aws.String("SET #t = :t"),
		ExpressionAttributeNames:  map[string]string{"#t": "text"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":t": s("new")},
	})
	if err != nil {
		t.Fatal(err)
	}

	// update missing item with exists condition fails
	_, err = c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 aws.String("tbl"),
		Key:                       map[string]types.AttributeValue{"pk": s("a"), "sk": s("zzz")},
		ConditionExpression:       aws.String("attribute_exists(sk)"),
		UpdateExpression:          aws.String("SET #t = :t"),
		ExpressionAttributeNames:  map[string]string{"#t": "text"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":t": s("new")},
	})
	if !errors.As(err, &cond) {
		t.Fatalf("expected ConditionalCheckFailedException, got %v", err)
	}

	// conditional delete
	_, err = c.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName:                 aws.String("tbl"),
		Key:                       map[string]types.AttributeValue{"pk": s("a"), "sk": s("1")},
		ConditionExpression:       aws.String("attribute_exists(sk) AND #t = :old"),
		ExpressionAttributeNames:  map[string]string{"#t": "text"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":old": s("wrong")},
	})
	if !errors.As(err, &cond) {
		t.Fatalf("expected ConditionalCheckFailedException, got %v", err)
	}
	// still there
	out, _ := c.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String("tbl"),
		Key:       map[string]types.AttributeValue{"pk": s("a"), "sk": s("1")}})
	if out.Item == nil {
		t.Fatal("item should still exist after failed conditional delete")
	}
}

func TestConditionalCheckFailedReturnsItem(t *testing.T) {
	ctx := context.Background()
	_, c := openMemory(t)
	makeTable(t, ctx, c, "tbl", false)
	if _, err := c.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String("tbl"),
		Item:      map[string]types.AttributeValue{"pk": s("a"), "v": n("7")},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:                           aws.String("tbl"),
		ConditionExpression:                 aws.String("#v < :n"),
		Item:                                map[string]types.AttributeValue{"pk": s("a"), "v": n("9")},
		ExpressionAttributeNames:            map[string]string{"#v": "v"},
		ExpressionAttributeValues:           map[string]types.AttributeValue{":n": n("5")},
		ReturnValuesOnConditionCheckFailure: types.ReturnValuesOnConditionCheckFailureAllOld,
	})
	var cond *types.ConditionalCheckFailedException
	if !errors.As(err, &cond) {
		t.Fatalf("expected ConditionalCheckFailedException, got %v", err)
	}
	if cond.Item == nil {
		t.Fatal("expected Item in ConditionalCheckFailedException")
	}
	if got := cond.Item["v"].(*types.AttributeValueMemberN).Value; got != "7" {
		t.Fatalf("expected old value 7, got %s", got)
	}
	// and the write did not happen
	out, _ := c.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String("tbl"), Key: map[string]types.AttributeValue{"pk": s("a")}})
	if got := out.Item["v"].(*types.AttributeValueMemberN).Value; got != "7" {
		t.Fatalf("failed condition must not write; got %s", got)
	}
}

// ---- concurrent conditional writes ----

func TestConcurrentConditionalPut(t *testing.T) {
	ctx := context.Background()
	_, c := openMemory(t)
	makeTable(t, ctx, c, "tbl", false)

	const goroutines = 20
	var wg sync.WaitGroup
	wins := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
				TableName:           aws.String("tbl"),
				ConditionExpression: aws.String("attribute_not_exists(pk)"),
				Item:                map[string]types.AttributeValue{"pk": s("race"), "v": n("1")},
			})
			wins <- err
		}()
	}
	wg.Wait()
	close(wins)
	okCount, failCount := 0, 0
	for err := range wins {
		if err == nil {
			okCount++
		} else {
			var cond *types.ConditionalCheckFailedException
			if errors.As(err, &cond) {
				failCount++
			} else {
				t.Errorf("unexpected error: %v", err)
			}
		}
	}
	if okCount != 1 || failCount != goroutines-1 {
		t.Fatalf("expected exactly 1 winner, got %d wins %d fails", okCount, failCount)
	}
}

// ---- query ordering + paging ----

func TestQueryOrderingAndPaging(t *testing.T) {
	ctx := context.Background()
	_, c := openMemory(t)
	makeTable(t, ctx, c, "tbl", true)

	// sort keys designed to exercise byte ordering
	ids := []string{"a1", "a10", "a2", "b", "b0"}
	for _, id := range ids {
		if _, err := c.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: aws.String("tbl"),
			Item:      map[string]types.AttributeValue{"pk": s("o"), "sk": s(id)},
		}); err != nil {
			t.Fatal(err)
		}
	}

	query := func(in *dynamodb.QueryInput) *dynamodb.QueryOutput {
		out, err := c.Query(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	keyExpr := "#p = :p"
	names := map[string]string{"#p": "pk"}
	vals := map[string]types.AttributeValue{":p": s("o")}

	out := query(&dynamodb.QueryInput{
		TableName: aws.String("tbl"), KeyConditionExpression: &keyExpr,
		ExpressionAttributeNames: names, ExpressionAttributeValues: vals,
	})
	got := []string{}
	for _, it := range out.Items {
		got = append(got, it["sk"].(*types.AttributeValueMemberS).Value)
	}
	want := []string{"a1", "a10", "a2", "b", "b0"} // UTF-8 byte order
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order mismatch: %v", got)
	}

	// descending
	out = query(&dynamodb.QueryInput{
		TableName: aws.String("tbl"), KeyConditionExpression: &keyExpr,
		ExpressionAttributeNames: names, ExpressionAttributeValues: vals,
		ScanIndexForward: aws.Bool(false),
	})
	if out.Items[0]["sk"].(*types.AttributeValueMemberS).Value != "b0" {
		t.Fatalf("desc order wrong: %v", out.Items[0])
	}

	// begins_with
	expr := "#p = :p AND begins_with(sk, :pre)"
	vals2 := map[string]types.AttributeValue{":p": s("o"), ":pre": s("a")}
	out = query(&dynamodb.QueryInput{
		TableName: aws.String("tbl"), KeyConditionExpression: &expr,
		ExpressionAttributeNames: names, ExpressionAttributeValues: vals2,
	})
	if len(out.Items) != 3 {
		t.Fatalf("begins_with expected 3, got %d", len(out.Items))
	}

	// paginator walks all pages
	p := dynamodb.NewQueryPaginator(c, &dynamodb.QueryInput{
		TableName: aws.String("tbl"), KeyConditionExpression: &keyExpr,
		ExpressionAttributeNames: names, ExpressionAttributeValues: vals,
		Limit: aws.Int32(2),
	})
	count := 0
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		count += len(page.Items)
		if len(page.Items) > 2 {
			t.Fatalf("page exceeded limit: %d", len(page.Items))
		}
	}
	if count != 5 {
		t.Fatalf("paginator saw %d items", count)
	}
}

func TestQueryNumericSortKey(t *testing.T) {
	ctx := context.Background()
	_, c := openMemory(t)

	_, err := c.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String("nums"),
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("sk"), KeyType: types.KeyTypeRange},
		},
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("sk"), AttributeType: types.ScalarAttributeTypeN},
		},
		BillingMode: types.BillingModePayPerRequest,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"2", "10", "1", "-3", "0.5", "1e2"} {
		if _, err := c.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: aws.String("nums"),
			Item:      map[string]types.AttributeValue{"pk": s("p"), "sk": n(v)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	expr := "pk = :p"
	out, err := c.Query(ctx, &dynamodb.QueryInput{
		TableName: aws.String("nums"), KeyConditionExpression: &expr,
		ExpressionAttributeValues: map[string]types.AttributeValue{":p": s("p")},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range out.Items {
		got = append(got, it["sk"].(*types.AttributeValueMemberN).Value)
	}
	want := []string{"-3", "0.5", "1", "2", "10", "1e2"} // numeric order
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("numeric order mismatch: %v", got)
	}

	// range condition
	expr = "pk = :p AND sk BETWEEN :lo AND :hi"
	out, err = c.Query(ctx, &dynamodb.QueryInput{
		TableName: aws.String("nums"), KeyConditionExpression: &expr,
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":p": s("p"), ":lo": n("0"), ":hi": n("10"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 4 {
		t.Fatalf("BETWEEN expected 4, got %d", len(out.Items))
	}
}

// ---- UpdateItem ----

func TestUpdateItemExpressions(t *testing.T) {
	ctx := context.Background()
	_, c := openMemory(t)
	makeTable(t, ctx, c, "tbl", false)
	key := map[string]types.AttributeValue{"pk": s("k")}

	if _, err := c.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String("tbl"),
		Item: map[string]types.AttributeValue{
			"pk": s("k"), "count": n("1"), "text": s("a"),
			"tags": &types.AttributeValueMemberSS{Value: []string{"x"}},
			"list": &types.AttributeValueMemberL{Value: []types.AttributeValue{s("a")}},
			"m":    &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{"a": n("1"), "b": n("2")}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// SET arithmetic, REMOVE, ADD on number and set, nested set, list_append, if_not_exists
	_, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String("tbl"),
		Key:       key,
		UpdateExpression: aws.String(
			"SET #c = #c + :inc, #m.#b = :nb, #l = list_append(#l, :elem), #new = if_not_exists(#new, :d) " +
				"REMOVE #t2 ADD #c2 :one, #tags :more"),
		ExpressionAttributeNames: map[string]string{
			"#c": "count", "#m": "m", "#b": "b", "#l": "list",
			"#new": "newattr", "#t2": "text", "#c2": "counter2", "#tags": "tags",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":inc":  n("4"),
			":nb":   n("99"),
			":elem": &types.AttributeValueMemberL{Value: []types.AttributeValue{n("5")}},
			":d":    s("created"),
			":one":  n("1"),
			":more": &types.AttributeValueMemberSS{Value: []string{"y"}},
		},
		ReturnValues: types.ReturnValueAllNew,
	})
	if err != nil {
		t.Fatal(err)
	}
	// DELETE from the set (separate call: overlapping paths are rejected)
	_, err = c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                aws.String("tbl"),
		Key:                      key,
		UpdateExpression:         aws.String("DELETE #tags :gone"),
		ExpressionAttributeNames: map[string]string{"#tags": "tags"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":gone": &types.AttributeValueMemberSS{Value: []string{"x"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String("tbl"), Key: key})
	if err != nil {
		t.Fatal(err)
	}
	it := out.Item
	if it["count"].(*types.AttributeValueMemberN).Value != "5" {
		t.Fatalf("count: %v", it["count"])
	}
	if _, ok := it["text"]; ok {
		t.Fatal("text should be removed")
	}
	if it["m"].(*types.AttributeValueMemberM).Value["b"].(*types.AttributeValueMemberN).Value != "99" {
		t.Fatalf("m.b: %v", it["m"])
	}
	if l := it["list"].(*types.AttributeValueMemberL).Value; len(l) != 2 {
		t.Fatalf("list: %v", l)
	}
	if it["newattr"].(*types.AttributeValueMemberS).Value != "created" {
		t.Fatalf("if_not_exists: %v", it["newattr"])
	}
	if it["counter2"].(*types.AttributeValueMemberN).Value != "1" {
		t.Fatalf("ADD number: %v", it["counter2"])
	}
	if got := it["tags"].(*types.AttributeValueMemberSS).Value; len(got) != 1 || got[0] != "y" {
		t.Fatalf("set ops: %v", got)
	}
}

// ---- errors ----

func TestErrorTypes(t *testing.T) {
	ctx := context.Background()
	_, c := openMemory(t)
	makeTable(t, ctx, c, "tbl", false)

	// duplicate create -> ResourceInUseException
	err := func() error {
		ks := []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}}
		defs := []types.AttributeDefinition{{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS}}
		_, e := c.CreateTable(ctx, &dynamodb.CreateTableInput{
			TableName: aws.String("tbl"), KeySchema: ks, AttributeDefinitions: defs,
			BillingMode: types.BillingModePayPerRequest})
		return e
	}()
	var inUse *types.ResourceInUseException
	if !errors.As(err, &inUse) {
		t.Fatalf("expected ResourceInUseException, got %v", err)
	}

	// describe missing -> ResourceNotFoundException
	_, err = c.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String("nope")})
	var notFound *types.ResourceNotFoundException
	if !errors.As(err, &notFound) {
		t.Fatalf("expected ResourceNotFoundException, got %v", err)
	}

	// get from missing table
	_, err = c.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String("nope"), Key: map[string]types.AttributeValue{"pk": s("x")}})
	if !errors.As(err, &notFound) {
		t.Fatalf("expected ResourceNotFoundException, got %v", err)
	}

	// unsupported operation -> ValidationException, not a retry storm
	_, err = c.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{
		TransactItems: []types.TransactWriteItem{
			{Put: &types.Put{
				TableName: aws.String("tbl"),
				Item:      map[string]types.AttributeValue{"pk": s("x")},
			}},
		},
	})
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "ValidationException" {
		t.Fatalf("expected ValidationException, got %v", err)
	}
}

// ---- waiter ----

func TestTableExistsWaiter(t *testing.T) {
	ctx := context.Background()
	_, c := openMemory(t)
	makeTable(t, ctx, c, "tbl", false)

	waiter := dynamodb.NewTableExistsWaiter(c)
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := waiter.Wait(wctx, &dynamodb.DescribeTableInput{TableName: aws.String("tbl")}, time.Minute); err != nil {
		t.Fatalf("waiter: %v", err)
	}
}

// ---- list tables paging ----

func TestListTablesPaging(t *testing.T) {
	ctx := context.Background()
	_, c := openMemory(t)
	for _, name := range []string{"tt1", "tt2", "tt3"} {
		makeTable(t, ctx, c, name, false)
	}
	p := dynamodb.NewListTablesPaginator(c, &dynamodb.ListTablesInput{Limit: aws.Int32(2)})
	var all []string
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page.TableNames...)
	}
	if strings.Join(all, ",") != "tt1,tt2,tt3" {
		t.Fatalf("got %v", all)
	}
}

// ---- context + close ----

func TestContextCancel(t *testing.T) {
	db, err := dynar.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := newClient(t, db)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.ListTables(ctx, &dynamodb.ListTablesInput{})
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
	var canceled interface{ CancelError() string }
	_ = canceled
}

func TestUseAfterClose(t *testing.T) {
	ctx := context.Background()
	db, err := dynar.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = c.ListTables(ctx, &dynamodb.ListTablesInput{})
	if err == nil {
		t.Fatal("expected error after Close")
	}
	// must be an API-style error, not a retryable transport error
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected API error, got %T %v", err, err)
	}
}

// ---- delete table ----

func TestDeleteTable(t *testing.T) {
	ctx := context.Background()
	_, c := openMemory(t)
	makeTable(t, ctx, c, "tbl", false)
	if _, err := c.DeleteTable(ctx, &dynamodb.DeleteTableInput{TableName: aws.String("tbl")}); err != nil {
		t.Fatal(err)
	}
	_, err := c.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String("tbl"), Key: map[string]types.AttributeValue{"pk": s("x")}})
	var notFound *types.ResourceNotFoundException
	if !errors.As(err, &notFound) {
		t.Fatalf("expected ResourceNotFoundException, got %v", err)
	}
}

// ---- describe table ----

func TestDescribeTable(t *testing.T) {
	ctx := context.Background()
	_, c := openMemory(t)
	makeTable(t, ctx, c, "tbl", true)
	out, err := c.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String("tbl")})
	if err != nil {
		t.Fatal(err)
	}
	if out.Table.TableStatus != types.TableStatusActive {
		t.Fatalf("status: %v", out.Table.TableStatus)
	}
	if len(out.Table.KeySchema) != 2 {
		t.Fatalf("key schema: %v", out.Table.KeySchema)
	}
	if aws.ToString(out.Table.TableArn) == "" {
		t.Fatal("missing arn")
	}
}

// ---- filter + projection ----

func TestQueryFilterAndProjection(t *testing.T) {
	ctx := context.Background()
	_, c := openMemory(t)
	makeTable(t, ctx, c, "tbl", true)
	for i, txt := range []string{"keep", "drop", "keep2"} {
		id := fmt.Sprintf("i%d", i)
		if _, err := c.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: aws.String("tbl"),
			Item: map[string]types.AttributeValue{
				"pk": s("o"), "sk": s(id), "text": s(txt), "junk": s("x"),
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	kce := "pk = :p"
	fe := "begins_with(#txt, :pre)"
	pe := "pk, sk, #txt"
	out, err := c.Query(ctx, &dynamodb.QueryInput{
		TableName: aws.String("tbl"), KeyConditionExpression: &kce,
		FilterExpression: &fe, ProjectionExpression: &pe,
		ExpressionAttributeNames:  map[string]string{"#txt": "text"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":p": s("o"), ":pre": s("keep")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 2 {
		t.Fatalf("expected 2, got %d", len(out.Items))
	}
	if _, ok := out.Items[0]["junk"]; ok {
		t.Fatal("projection should drop junk")
	}
	if out.ScannedCount != 3 || out.Count != 2 {
		t.Fatalf("counts: %d/%d", out.Count, out.ScannedCount)
	}
}

// ---- deletion protection ----

func TestDeletionProtection(t *testing.T) {
	ctx := context.Background()
	_, c := openMemory(t)

	_, err := c.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String("protected"),
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS}},
		BillingMode:               types.BillingModePayPerRequest,
		DeletionProtectionEnabled: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.DeleteTable(ctx, &dynamodb.DeleteTableInput{TableName: aws.String("protected")})
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "ValidationException" {
		t.Fatalf("expected ValidationException for protected table, got %v", err)
	}
}

// ---- scan ----

func TestScan(t *testing.T) {
	ctx := context.Background()
	_, c := openMemory(t)
	makeTable(t, ctx, c, "tbl", true)
	for _, kv := range [][2]string{{"a", "1"}, {"a", "2"}, {"b", "1"}} {
		if _, err := c.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: aws.String("tbl"),
			Item:      map[string]types.AttributeValue{"pk": s(kv[0]), "sk": s(kv[1])},
		}); err != nil {
			t.Fatal(err)
		}
	}
	out, err := c.Scan(ctx, &dynamodb.ScanInput{TableName: aws.String("tbl")})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 3 {
		t.Fatalf("scan: %d", len(out.Items))
	}
	// filter + count select
	fe := "pk = :p"
	out, err = c.Scan(ctx, &dynamodb.ScanInput{
		TableName:                 aws.String("tbl"),
		FilterExpression:          &fe,
		ExpressionAttributeValues: map[string]types.AttributeValue{":p": s("a")},
		Select:                    types.SelectCount,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Count != 2 {
		t.Fatalf("count: %d", out.Count)
	}
}

// ---- empty key rejected ----

func TestEmptyKeyRejected(t *testing.T) {
	ctx := context.Background()
	_, c := openMemory(t)
	makeTable(t, ctx, c, "tbl", false)
	_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String("tbl"),
		Item:      map[string]types.AttributeValue{"pk": s("")},
	})
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "ValidationException" {
		t.Fatalf("expected ValidationException, got %v", err)
	}
}
