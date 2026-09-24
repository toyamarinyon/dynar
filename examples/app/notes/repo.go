// Package notes is a repository that only depends on *dynamodb.Client.
// It has no knowledge of dynar; the same code runs against production
// DynamoDB and the local SQLite-backed database.
package notes

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const tableName = "notes"

type Note struct {
	Owner string
	ID    string
	Text  string
}

type Repository struct {
	client *dynamodb.Client
}

func NewRepository(client *dynamodb.Client) *Repository {
	return &Repository{client: client}
}

// EnsureTable creates the table if it does not exist.
func (r *Repository) EnsureTable(ctx context.Context) error {
	_, err := r.client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(tableName),
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("owner"), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String("id"), KeyType: types.KeyTypeRange},
		},
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("owner"), AttributeType: types.ScalarAttributeTypeS},
			{AttributeName: aws.String("id"), AttributeType: types.ScalarAttributeTypeS},
		},
		BillingMode: types.BillingModePayPerRequest,
	})
	if err != nil {
		var inUse *types.ResourceInUseException
		if errors.As(err, &inUse) {
			return nil
		}
		return fmt.Errorf("create table: %w", err)
	}
	return nil
}

// Create stores a new note, failing if the id already exists for the owner.
func (r *Repository) Create(ctx context.Context, n Note) error {
	_, err := r.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(tableName),
		ConditionExpression: aws.String("attribute_not_exists(id)"),
		Item: map[string]types.AttributeValue{
			"owner": &types.AttributeValueMemberS{Value: n.Owner},
			"id":    &types.AttributeValueMemberS{Value: n.ID},
			"text":  &types.AttributeValueMemberS{Value: n.Text},
		},
	})
	if err != nil {
		var cond *types.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			return fmt.Errorf("note %q already exists", n.ID)
		}
		return fmt.Errorf("put item: %w", err)
	}
	return nil
}

// List returns all notes owned by owner, ordered by id.
func (r *Repository) List(ctx context.Context, owner string) ([]Note, error) {
	out, err := r.client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(tableName),
		KeyConditionExpression: aws.String("#o = :o"),
		ExpressionAttributeNames: map[string]string{
			"#o": "owner",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":o": &types.AttributeValueMemberS{Value: owner},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	notes := make([]Note, 0, len(out.Items))
	for _, item := range out.Items {
		notes = append(notes, Note{
			Owner: stringAttr(item, "owner"),
			ID:    stringAttr(item, "id"),
			Text:  stringAttr(item, "text"),
		})
	}
	return notes, nil
}

// UpdateText changes the text of an existing note.
func (r *Repository) UpdateText(ctx context.Context, owner, id, text string) error {
	_, err := r.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"owner": &types.AttributeValueMemberS{Value: owner},
			"id":    &types.AttributeValueMemberS{Value: id},
		},
		ConditionExpression: aws.String("attribute_exists(id)"),
		UpdateExpression:    aws.String("SET #t = :t"),
		ExpressionAttributeNames: map[string]string{
			"#t": "text",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":t": &types.AttributeValueMemberS{Value: text},
		},
	})
	if err != nil {
		var cond *types.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			return fmt.Errorf("note %q does not exist", id)
		}
		return fmt.Errorf("update item: %w", err)
	}
	return nil
}

// Delete removes a note if it exists.
func (r *Repository) Delete(ctx context.Context, owner, id string) error {
	_, err := r.client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(tableName),
		Key: map[string]types.AttributeValue{
			"owner": &types.AttributeValueMemberS{Value: owner},
			"id":    &types.AttributeValueMemberS{Value: id},
		},
		ConditionExpression: aws.String("attribute_exists(id)"),
	})
	if err != nil {
		var cond *types.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			return fmt.Errorf("note %q does not exist", id)
		}
		return fmt.Errorf("delete item: %w", err)
	}
	return nil
}

func stringAttr(item map[string]types.AttributeValue, name string) string {
	if v, ok := item[name].(*types.AttributeValueMemberS); ok {
		return v.Value
	}
	return ""
}
