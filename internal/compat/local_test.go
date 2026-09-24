//go:build dynamodblocal

package compat

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// TestDynamoDBLocal runs the shared scenario suite against the official
// DynamoDB Local. Build tag: dynamodblocal.
//
//	go test -tags dynamodblocal ./internal/compat/
//
// Point DYNAMODB_LOCAL_ENDPOINT at a running instance
// (default http://localhost:8000). If the endpoint is unreachable the
// test FAILS — it does not skip — because an explicit compat run that
// cannot reach the reference implementation is meaningless.
func TestDynamoDBLocal(t *testing.T) {
	endpoint := os.Getenv("DYNAMODB_LOCAL_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://localhost:8000"
	}
	c := dynamodb.New(dynamodb.Options{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("x", "x", ""),
		BaseEndpoint: aws.String(endpoint),
	})
	// fail fast if unreachable
	if _, err := c.ListTables(t.Context(), &dynamodb.ListTablesInput{}); err != nil {
		t.Fatalf("DynamoDB Local at %s is unreachable: %v", endpoint, err)
	}
	RunAll(t, c, fmt.Sprintf("l%d", time.Now().UnixNano()%1_000_000))
}
