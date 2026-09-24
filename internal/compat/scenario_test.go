// Package compat runs one shared DynamoDB scenario suite against both
// dynar and the official DynamoDB Local. Scenarios take a
// *dynamodb.Client so the same code exercises both backends.
package compat

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	smithy "github.com/aws/smithy-go"
)

// RunAll executes the shared compatibility scenarios. prefix must make
// table names unique per backend run.
func RunAll(t *testing.T, c *dynamodb.Client, prefix string) {
	t.Run("CRUDAndTypes", func(t *testing.T) { scenarioCRUD(t, c, prefix) })
	t.Run("ConditionalWrites", func(t *testing.T) { scenarioConditional(t, c, prefix) })
	t.Run("UpdateExpressions", func(t *testing.T) { scenarioUpdate(t, c, prefix) })
	t.Run("QueryOrdering", func(t *testing.T) { scenarioQuery(t, c, prefix) })
	t.Run("QueryPaging", func(t *testing.T) { scenarioPaging(t, c, prefix) })
	t.Run("Errors", func(t *testing.T) { scenarioErrors(t, c, prefix) })
	t.Run("ReturnValues", func(t *testing.T) { scenarioReturnValues(t, c, prefix) })
}

// helper: create table with pk(S) + optional sk(S)
func createTable(t *testing.T, ctx context.Context, c *dynamodb.Client, name string, sortKey bool) {
	t.Helper()
	ks := []types.KeySchemaElement{
		{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash},
	}
	defs := []types.AttributeDefinition{
		{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS},
	}
	if sortKey {
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
	waitActive(t, ctx, c, name)
	t.Cleanup(func() {
		// best-effort cleanup for the real backend
		c.DeleteTable(context.Background(), &dynamodb.DeleteTableInput{TableName: aws.String(name)})
	})
}

func waitActive(t *testing.T, ctx context.Context, c *dynamodb.Client, name string) {
	t.Helper()
	w := dynamodb.NewTableExistsWaiter(c)
	if err := w.Wait(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(name)}, 30_000_000_000); err != nil {
		t.Fatalf("wait for %s: %v", name, err)
	}
}

func s(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }
func n(v string) types.AttributeValue { return &types.AttributeValueMemberN{Value: v} }

func scenarioCRUD(t *testing.T, c *dynamodb.Client, prefix string) {
	ctx := context.Background()
	tbl := prefix + "-crud"
	createTable(t, ctx, c, tbl, true)

	item := map[string]types.AttributeValue{
		"pk":   s("o1"),
		"sk":   s("i1"),
		"text": s("hello"),
		"num":  n("9007199254740993123.14159"),
		"bin":  &types.AttributeValueMemberB{Value: []byte{0, 1, 255}},
		"flag": &types.AttributeValueMemberBOOL{Value: true},
		"none": &types.AttributeValueMemberNULL{Value: true},
		"lst":  &types.AttributeValueMemberL{Value: []types.AttributeValue{s("a"), n("1")}},
		"mp": &types.AttributeValueMemberM{Value: map[string]types.AttributeValue{
			"k": s("v"), "deep": &types.AttributeValueMemberL{Value: []types.AttributeValue{n("2")}},
		}},
		"ss": &types.AttributeValueMemberSS{Value: []string{"a", "b"}},
		"ns": &types.AttributeValueMemberNS{Value: []string{"1", "2.5"}},
		"bs": &types.AttributeValueMemberBS{Value: [][]byte{{1}, {2, 3}}},
	}
	if _, err := c.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: &tbl, Item: item}); err != nil {
		t.Fatal(err)
	}

	out, err := c.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &tbl,
		Key:       map[string]types.AttributeValue{"pk": s("o1"), "sk": s("i1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertItemEqual(t, item, out.Item)

	// missing item -> empty result
	out, err = c.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &tbl,
		Key:       map[string]types.AttributeValue{"pk": s("o1"), "sk": s("nope")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Item) != 0 {
		t.Fatalf("expected empty item, got %v", out.Item)
	}
}

func scenarioConditional(t *testing.T, c *dynamodb.Client, prefix string) {
	ctx := context.Background()
	tbl := prefix + "-cond"
	createTable(t, ctx, c, tbl, true)

	put := func(condExpr *string) error {
		_, err := c.PutItem(ctx, &dynamodb.PutItemInput{
			TableName:           &tbl,
			ConditionExpression: condExpr,
			Item: map[string]types.AttributeValue{
				"pk": s("a"), "sk": s("1"), "v": n("1"),
			},
		})
		return err
	}
	if err := put(aws.String("attribute_not_exists(sk)")); err != nil {
		t.Fatal(err)
	}
	// second conditional put must fail with the typed error and not write
	err := put(aws.String("attribute_not_exists(sk)"))
	var cond *types.ConditionalCheckFailedException
	if !errors.As(err, &cond) {
		t.Fatalf("expected ConditionalCheckFailedException, got %v", err)
	}
	out, _ := c.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &tbl,
		Key:       map[string]types.AttributeValue{"pk": s("a"), "sk": s("1")}})
	if out.Item["v"].(*types.AttributeValueMemberN).Value != "1" {
		t.Fatal("failed conditional write modified data")
	}

	// conditional delete: condition true
	_, err = c.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName:                 &tbl,
		Key:                       map[string]types.AttributeValue{"pk": s("a"), "sk": s("1")},
		ConditionExpression:       aws.String("attribute_exists(sk) AND #v = :one"),
		ExpressionAttributeNames:  map[string]string{"#v": "v"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":one": n("1")},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, _ = c.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &tbl,
		Key:       map[string]types.AttributeValue{"pk": s("a"), "sk": s("1")}})
	if len(out.Item) != 0 {
		t.Fatal("item should be deleted")
	}
}

func scenarioUpdate(t *testing.T, c *dynamodb.Client, prefix string) {
	ctx := context.Background()
	tbl := prefix + "-upd"
	createTable(t, ctx, c, tbl, false)
	key := map[string]types.AttributeValue{"pk": s("k")}

	// update creates the item when absent
	_, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 &tbl,
		Key:                       key,
		UpdateExpression:          aws.String("SET #c = :z, #t = :t"),
		ExpressionAttributeNames:  map[string]string{"#c": "count", "#t": "text"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":z": n("0"), ":t": s("hi")},
	})
	if err != nil {
		t.Fatal(err)
	}

	// arithmetic + if_not_exists + list_append + REMOVE + ADD set
	// (list_append(if_not_exists(...)) is the documented append-or-create idiom)
	_, err = c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: &tbl,
		Key:       key,
		UpdateExpression: aws.String(
			"SET #c = #c + :i, #e = if_not_exists(#e, :d), " +
				"#l = list_append(if_not_exists(#l, :empty), :x) " +
				"REMOVE #t ADD #tags :a"),
		ExpressionAttributeNames: map[string]string{
			"#c": "count", "#e": "extra", "#l": "lst", "#t": "text", "#tags": "tags",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":i":     n("5"),
			":d":     s("dflt"),
			":x":     &types.AttributeValueMemberL{Value: []types.AttributeValue{n("7")}},
			":empty": &types.AttributeValueMemberL{Value: []types.AttributeValue{}},
			":a":     &types.AttributeValueMemberSS{Value: []string{"p", "q"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// DELETE set members in a separate call: DynamoDB rejects overlapping
	// document paths within one UpdateExpression.
	_, err = c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                &tbl,
		Key:                      key,
		UpdateExpression:         aws.String("DELETE #tags :b"),
		ExpressionAttributeNames: map[string]string{"#tags": "tags"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":b": &types.AttributeValueMemberSS{Value: []string{"q"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.GetItem(ctx, &dynamodb.GetItemInput{TableName: &tbl, Key: key})
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
	if it["extra"].(*types.AttributeValueMemberS).Value != "dflt" {
		t.Fatalf("if_not_exists: %v", it["extra"])
	}
	if l := it["lst"].(*types.AttributeValueMemberL).Value; len(l) != 1 {
		t.Fatalf("list_append: %v", l)
	}
	if got := it["tags"].(*types.AttributeValueMemberSS).Value; len(got) != 1 || got[0] != "p" {
		t.Fatalf("set ops: %v", got)
	}
}

func scenarioQuery(t *testing.T, c *dynamodb.Client, prefix string) {
	ctx := context.Background()
	tbl := prefix + "-q"
	createTable(t, ctx, c, tbl, true)

	ids := []string{"a1", "a10", "a2", "b", "b0"}
	for _, id := range ids {
		if _, err := c.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: &tbl,
			Item:      map[string]types.AttributeValue{"pk": s("o"), "sk": s(id), "v": n("1")},
		}); err != nil {
			t.Fatal(err)
		}
	}
	// another partition must not leak
	if _, err := c.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: &tbl,
		Item:      map[string]types.AttributeValue{"pk": s("other"), "sk": s("a0")},
	}); err != nil {
		t.Fatal(err)
	}

	kce := "pk = :p"
	vals := map[string]types.AttributeValue{":p": s("o")}
	getIDs := func(out *dynamodb.QueryOutput) []string {
		var r []string
		for _, it := range out.Items {
			r = append(r, it["sk"].(*types.AttributeValueMemberS).Value)
		}
		return r
	}

	out, err := c.Query(ctx, &dynamodb.QueryInput{
		TableName: &tbl, KeyConditionExpression: &kce, ExpressionAttributeValues: vals})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := getIDs(out), []string{"a1", "a10", "a2", "b", "b0"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order: got %v want %v", got, want)
	}

	out, err = c.Query(ctx, &dynamodb.QueryInput{
		TableName: &tbl, KeyConditionExpression: &kce, ExpressionAttributeValues: vals,
		ScanIndexForward: aws.Bool(false)})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := getIDs(out), []string{"b0", "b", "a2", "a10", "a1"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("desc order: got %v want %v", got, want)
	}

	// begins_with
	kce2 := "pk = :p AND begins_with(sk, :pre)"
	out, err = c.Query(ctx, &dynamodb.QueryInput{
		TableName: &tbl, KeyConditionExpression: &kce2,
		ExpressionAttributeValues: map[string]types.AttributeValue{":p": s("o"), ":pre": s("a")}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := getIDs(out), []string{"a1", "a10", "a2"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("begins_with: got %v want %v", got, want)
	}

	// BETWEEN
	kce3 := "pk = :p AND sk BETWEEN :lo AND :hi"
	out, err = c.Query(ctx, &dynamodb.QueryInput{
		TableName: &tbl, KeyConditionExpression: &kce3,
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":p": s("o"), ":lo": s("a10"), ":hi": s("b")}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := getIDs(out), []string{"a10", "a2", "b"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("between: got %v want %v", got, want)
	}
}

func scenarioPaging(t *testing.T, c *dynamodb.Client, prefix string) {
	ctx := context.Background()
	tbl := prefix + "-page"
	createTable(t, ctx, c, tbl, true)

	const total = 7
	for i := 0; i < total; i++ {
		if _, err := c.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: &tbl,
			Item:      map[string]types.AttributeValue{"pk": s("o"), "sk": s(fmt.Sprintf("i%02d", i))},
		}); err != nil {
			t.Fatal(err)
		}
	}

	kce := "pk = :p"
	vals := map[string]types.AttributeValue{":p": s("o")}

	// manual pagination: every page <= Limit, LEK chains to next page
	var seen []string
	var startKey map[string]types.AttributeValue
	for page := 0; ; page++ {
		out, err := c.Query(ctx, &dynamodb.QueryInput{
			TableName: &tbl, KeyConditionExpression: &kce,
			ExpressionAttributeValues: vals,
			Limit:                     aws.Int32(3),
			ExclusiveStartKey:         startKey,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range out.Items {
			seen = append(seen, it["sk"].(*types.AttributeValueMemberS).Value)
		}
		if len(out.Items) > 3 {
			t.Fatalf("page exceeded limit: %d", len(out.Items))
		}
		if out.LastEvaluatedKey == nil {
			break
		}
		startKey = out.LastEvaluatedKey
		if page > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != total {
		t.Fatalf("expected %d items, got %v", total, seen)
	}
	// order is meaningful: must be globally ascending
	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] {
			t.Fatalf("pagination order broken: %v", seen)
		}
	}

	// SDK paginator agrees
	p := dynamodb.NewQueryPaginator(c, &dynamodb.QueryInput{
		TableName: &tbl, KeyConditionExpression: &kce,
		ExpressionAttributeValues: vals, Limit: aws.Int32(3)})
	count := 0
	for p.HasMorePages() {
		pg, err := p.NextPage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		count += len(pg.Items)
	}
	if count != total {
		t.Fatalf("paginator: %d", count)
	}
}

func scenarioErrors(t *testing.T, c *dynamodb.Client, prefix string) {
	ctx := context.Background()
	tbl := prefix + "-err"
	createTable(t, ctx, c, tbl, false)

	// duplicate table
	ks := []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}}
	defs := []types.AttributeDefinition{{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS}}
	_, err := c.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: &tbl, KeySchema: ks, AttributeDefinitions: defs,
		BillingMode: types.BillingModePayPerRequest})
	// real DynamoDB returns ResourceInUseException; DynamoDB Local returns
	// ResourceInUseException too
	var inUse *types.ResourceInUseException
	if !errors.As(err, &inUse) {
		t.Fatalf("duplicate create: expected ResourceInUseException, got %v", err)
	}

	// missing table
	_, err = c.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(prefix + "-missing"),
		Key:       map[string]types.AttributeValue{"pk": s("x")}})
	var notFound *types.ResourceNotFoundException
	if !errors.As(err, &notFound) {
		t.Fatalf("missing table: expected ResourceNotFoundException, got %v", err)
	}

	// key element not matching schema
	_, err = c.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &tbl,
		Key:       map[string]types.AttributeValue{"pk": s("x"), "extra": s("y")}})
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "ValidationException" {
		t.Fatalf("bad key: expected ValidationException, got %v", err)
	}

	// reserved word without ExpressionAttributeNames
	kce := "owner = :p"
	_, err = c.Query(ctx, &dynamodb.QueryInput{
		TableName: &tbl, KeyConditionExpression: &kce,
		ExpressionAttributeValues: map[string]types.AttributeValue{":p": s("o")}})
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "ValidationException" {
		t.Fatalf("reserved word: expected ValidationException, got %v", err)
	}
}

func scenarioReturnValues(t *testing.T, c *dynamodb.Client, prefix string) {
	ctx := context.Background()
	tbl := prefix + "-rv"
	createTable(t, ctx, c, tbl, false)
	key := map[string]types.AttributeValue{"pk": s("k")}

	if _, err := c.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: &tbl,
		Item:      map[string]types.AttributeValue{"pk": s("k"), "v": n("1")},
	}); err != nil {
		t.Fatal(err)
	}

	// PutItem ALL_OLD returns previous
	out, err := c.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:    &tbl,
		Item:         map[string]types.AttributeValue{"pk": s("k"), "v": n("2")},
		ReturnValues: types.ReturnValueAllOld,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Attributes["v"].(*types.AttributeValueMemberN).Value != "1" {
		t.Fatalf("ALL_OLD: %v", out.Attributes)
	}

	// UpdateItem ALL_NEW
	uout, err := c.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: &tbl, Key: key,
		UpdateExpression:          aws.String("SET #v = :v"),
		ExpressionAttributeNames:  map[string]string{"#v": "v"},
		ExpressionAttributeValues: map[string]types.AttributeValue{":v": n("3")},
		ReturnValues:              types.ReturnValueAllNew,
	})
	if err != nil {
		t.Fatal(err)
	}
	if uout.Attributes["v"].(*types.AttributeValueMemberN).Value != "3" {
		t.Fatalf("ALL_NEW: %v", uout.Attributes)
	}

	// DeleteItem ALL_OLD
	dout, err := c.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: &tbl, Key: key, ReturnValues: types.ReturnValueAllOld,
	})
	if err != nil {
		t.Fatal(err)
	}
	if dout.Attributes["v"].(*types.AttributeValueMemberN).Value != "3" {
		t.Fatalf("delete ALL_OLD: %v", dout.Attributes)
	}
}

// assertItemEqual deep-compares two items attribute-by-attribute with
// readable failures (numbers compare as strings on the wire).
func assertItemEqual(t *testing.T, want, got map[string]types.AttributeValue) {
	t.Helper()
	for k, wv := range want {
		gv, ok := got[k]
		if !ok {
			t.Errorf("missing attribute %q", k)
			continue
		}
		if !reflect.DeepEqual(wv, gv) {
			t.Errorf("attribute %q: want %#v got %#v", k, wv, gv)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected attribute %q", k)
		}
	}
}
