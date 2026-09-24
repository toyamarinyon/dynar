// Command file demonstrates dynar with a persistent SQLite file.
//
// Run: go run ./examples/file
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/toyamarinyon/dynar"
)

func main() {
	if err := run(context.Background()); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	dir, err := os.MkdirTemp("", "dynar-example-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "dynamo.db")
	fmt.Println("db file:", path)

	db, err := dynar.Open(path)
	if err != nil {
		return err
	}

	client := dynamodb.New(dynamodb.Options{
		Region:      "ap-northeast-1",
		Credentials: credentials.NewStaticCredentialsProvider("local", "local", ""),
		HTTPClient:  db.HTTPClient(),
	})

	// CreateTable: owner (HASH) + id (RANGE)
	if _, err := client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String("notes"),
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("owner"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("id"), KeyType: types.KeyTypeRange},
		},
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("owner"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("id"), AttributeType: types.ScalarAttributeTypeS},
		},
		BillingMode: types.BillingModePayPerRequest,
	}); err != nil {
		return fmt.Errorf("CreateTable: %w", err)
	}
	fmt.Println("created table: notes")

	// PutItem with a condition on the sort key attribute.
	_, err = client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String("notes"),
		ConditionExpression: aws.String("attribute_not_exists(id)"),
		Item: map[string]types.AttributeValue{
			"owner": &types.AttributeValueMemberS{Value: "alice"},
			"id":    &types.AttributeValueMemberS{Value: "note-1"},
			"text":  &types.AttributeValueMemberS{Value: "hello"},
			"views": &types.AttributeValueMemberN{Value: "3"},
		},
	})
	if err != nil {
		return fmt.Errorf("PutItem: %w", err)
	}
	fmt.Println("put item: alice/note-1")

	// Query all notes for the owner. "owner" is a DynamoDB reserved word,
	// so it must go through ExpressionAttributeNames — same as production.
	out, err := client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String("notes"),
		KeyConditionExpression: aws.String("#o = :o"),
		ExpressionAttributeNames: map[string]string{
			"#o": "owner",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":o": &types.AttributeValueMemberS{Value: "alice"},
		},
	})
	if err != nil {
		return fmt.Errorf("Query: %w", err)
	}
	for _, item := range out.Items {
		fmt.Printf("query result: id=%s text=%s\n",
			item["id"].(*types.AttributeValueMemberS).Value,
			item["text"].(*types.AttributeValueMemberS).Value)
	}

	// Close, reopen, and confirm persistence.
	if err := db.Close(); err != nil {
		return err
	}
	fmt.Println("closed; reopening")

	db, err = dynar.Open(path)
	if err != nil {
		return err
	}
	defer db.Close()

	client = dynamodb.New(dynamodb.Options{
		Region:      "ap-northeast-1",
		Credentials: credentials.NewStaticCredentialsProvider("local", "local", ""),
		HTTPClient:  db.HTTPClient(),
	})

	got, err := client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String("notes"),
		Key: map[string]types.AttributeValue{
			"owner": &types.AttributeValueMemberS{Value: "alice"},
			"id":    &types.AttributeValueMemberS{Value: "note-1"},
		},
	})
	if err != nil {
		return fmt.Errorf("GetItem after reopen: %w", err)
	}
	fmt.Printf("reopened; text=%s\n", got.Item["text"].(*types.AttributeValueMemberS).Value)
	return nil
}
