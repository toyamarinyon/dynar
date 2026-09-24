// Command app demonstrates wiring a repository to either a real DynamoDB
// client or a dynar-backed client, chosen only at dependency assembly.
//
// Local (no network, no Docker):
//
//	go run ./examples/app -local
//
// Production (uses the default AWS credential chain):
//
//	go run ./examples/app
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/toyamarinyon/dynar"
	"github.com/toyamarinyon/dynar/examples/app/notes"
)

func main() {
	local := flag.Bool("local", false, "use the local dynar database instead of AWS")
	flag.Parse()

	if err := run(context.Background(), *local); err != nil {
		log.Fatal(err)
	}
}

// newClient assembles the DynamoDB client. This is the only place that
// knows about dynar; the repository below sees only *dynamodb.Client.
func newClient(ctx context.Context, local bool) (*dynamodb.Client, func() error, error) {
	if local {
		// Open does not create parent directories; that is the app's job.
		if err := os.MkdirAll(filepath.Join(".", ".local"), 0o755); err != nil {
			return nil, nil, err
		}
		db, err := dynar.Open("./.local/dynamo.db")
		if err != nil {
			return nil, nil, err
		}
		return dynamodb.New(dynamodb.Options{
			Region:      "ap-northeast-1",
			Credentials: credentials.NewStaticCredentialsProvider("local", "local", ""),
			HTTPClient:  db.HTTPClient(),
		}), db.Close, nil
	}

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, nil, err
	}
	return dynamodb.NewFromConfig(cfg), func() error { return nil }, nil
}

func run(ctx context.Context, local bool) error {
	client, closeDB, err := newClient(ctx, local)
	if err != nil {
		return err
	}
	defer closeDB()

	repo := notes.NewRepository(client)

	if err := repo.EnsureTable(ctx); err != nil {
		return err
	}

	for _, n := range []notes.Note{
		{Owner: "alice", ID: "note-1", Text: "hello"},
		{Owner: "alice", ID: "note-2", Text: "world"},
	} {
		if err := repo.Create(ctx, n); err != nil {
			fmt.Println("create:", err)
		}
	}

	if err := repo.UpdateText(ctx, "alice", "note-1", "hello, dynar"); err != nil {
		return err
	}

	list, err := repo.List(ctx, "alice")
	if err != nil {
		return err
	}
	for _, n := range list {
		fmt.Printf("%s/%s: %s\n", n.Owner, n.ID, n.Text)
	}

	if err := repo.Delete(ctx, "alice", "note-2"); err != nil {
		return err
	}
	fmt.Println("deleted note-2")
	return nil
}
