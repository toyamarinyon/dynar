package compat

import (
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/toyamarinyon/dynar"
)

// The same scenarios always run against dynar (memory + file) as part of
// the normal test suite — no external service needed.
func TestDynarMemory(t *testing.T) {
	db, err := dynar.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := dynamodb.New(dynamodb.Options{
		Region:      "ap-northeast-1",
		Credentials: credentials.NewStaticCredentialsProvider("local", "local", ""),
		HTTPClient:  db.HTTPClient(),
	})
	RunAll(t, c, fmt.Sprintf("m%d", time.Now().UnixNano()%1_000_000))
}

func TestDynarFile(t *testing.T) {
	db, err := dynar.Open(t.TempDir() + "/dynamo.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := dynamodb.New(dynamodb.Options{
		Region:      "ap-northeast-1",
		Credentials: credentials.NewStaticCredentialsProvider("local", "local", ""),
		HTTPClient:  db.HTTPClient(),
	})
	RunAll(t, c, fmt.Sprintf("f%d", time.Now().UnixNano()%1_000_000))
}
