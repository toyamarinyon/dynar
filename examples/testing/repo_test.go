// Package testing shows how a repository test runs entirely in memory:
// no Docker, no Java, no ports, no cleanup beyond db.Close.
//
// Run: go test ./examples/testing
package testing

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/toyamarinyon/dynar"
	"github.com/toyamarinyon/dynar/examples/app/notes"
)

// Each call to Open(":memory:") creates an independent database, so
// parallel tests never share data.
func newClient(t *testing.T) *dynamodb.Client {
	t.Helper()

	db, err := dynar.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})

	return dynamodb.New(dynamodb.Options{
		Region:      "ap-northeast-1",
		Credentials: credentials.NewStaticCredentialsProvider("local", "local", ""),
		HTTPClient:  db.HTTPClient(),
	})
}

func TestRepository(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := notes.NewRepository(newClient(t))

	if err := repo.EnsureTable(ctx); err != nil {
		t.Fatal(err)
	}
	if err := repo.Create(ctx, notes.Note{Owner: "alice", ID: "note-1", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Create(ctx, notes.Note{Owner: "alice", ID: "note-1", Text: "dup"}); err == nil {
		t.Fatal("duplicate create should fail")
	}
	if err := repo.UpdateText(ctx, "alice", "note-1", "updated"); err != nil {
		t.Fatal(err)
	}

	list, err := repo.List(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Text != "updated" {
		t.Fatalf("unexpected notes: %+v", list)
	}
}

// A second test proves data does not leak between Open(":memory:") handles.
func TestRepositoryIsolation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := notes.NewRepository(newClient(t))

	if err := repo.EnsureTable(ctx); err != nil {
		t.Fatal(err)
	}
	list, err := repo.List(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("expected empty database, got %+v", list)
	}
}
