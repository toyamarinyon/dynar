# dynar

Use a local DynamoDB as easily as opening a SQLite file — a Go library.

No Docker, no Java, no external server, no daemon, no listening port.
It receives the HTTP requests sent by the AWS SDK for Go v2 inside your
process, translates DynamoDB operations to SQLite, and returns responses
the SDK understands.

Your repository and CRUD code stays identical between production and
local development. The only thing that switches is client construction.

## Persisting to a file

```go
db, err := dynar.Open("./.local/dynamo.db")
if err != nil {
    return err
}
defer db.Close()

client := dynamodb.New(dynamodb.Options{
    Region:      "ap-northeast-1",
    Credentials: credentials.NewStaticCredentialsProvider("local", "local", ""),
    HTTPClient:  db.HTTPClient(),
})

// From here on, it's ordinary AWS SDK code.
_, err = client.PutItem(ctx, &dynamodb.PutItemInput{
    TableName: aws.String("notes"),
    Item: map[string]types.AttributeValue{
        "id":   &types.AttributeValueMemberS{Value: "note-1"},
        "text": &types.AttributeValueMemberS{Value: "hello"},
    },
})
```

`db.HTTPClient()` returns a value that satisfies the AWS SDK's
`aws.HTTPClient` interface and can be passed directly to
`dynamodb.Options.HTTPClient`. Requests are handled in-process and are
never sent to an external network. `http.DefaultClient`, environment
variables, and AWS credential files are not touched.

See `examples/file/` for a complete, copy-paste runnable example.

## In-memory for tests

```go
db, err := dynar.Open(":memory:")
if err != nil {
    t.Fatal(err)
}
t.Cleanup(func() {
    if err := db.Close(); err != nil {
        t.Error(err)
    }
})

client := dynamodb.New(dynamodb.Options{
    Region:      "ap-northeast-1",
    Credentials: credentials.NewStaticCredentialsProvider("local", "local", ""),
    HTTPClient:  db.HTTPClient(),
})
```

- Every `Open(":memory:")` creates an independent database.
- No data is shared between parallel tests.
- No substitute DynamoDB client interface is needed for tests —
  repositories keep receiving `*dynamodb.Client`.

See `examples/testing/` for a repository test example.

## Sharing business code between production and local

```go
// Switch only at dependency assembly time.
func newClient(ctx context.Context, local bool) (*dynamodb.Client, func() error, error) {
    if local {
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
```

The repository takes `*dynamodb.Client` and does not import dynar.
`examples/app/` contains a complete example including the repository
and the switching code.

Creating `./.local/dynamo.db` per worktree isolates environments.
dynar itself is unaware of Git, worktrees, and namespaces.

## Supported APIs

| API | Status | Notes |
|---|---|---|
| CreateTable | Supported | HASH / RANGE keys. BillingMode, DeletionProtectionEnabled, Tags supported. GSI/LSI/Stream rejected with an error |
| DescribeTable | Supported | The `TableExists` waiter works |
| ListTables | Supported | Limit / ExclusiveStartTableName pagination |
| DeleteTable | Supported | Honors DeletionProtectionEnabled |
| PutItem | Supported | ConditionExpression, ReturnValues(NONE/ALL_OLD), ReturnValuesOnConditionCheckFailure |
| GetItem | Supported | ProjectionExpression, ConsistentRead accepted (local reads are always consistent) |
| UpdateItem | Supported | SET / REMOVE / ADD / DELETE. if_not_exists, list_append, + / - |
| DeleteItem | Supported | ConditionExpression, ReturnValues(NONE/ALL_OLD) |
| Query | Supported | KeyConditionExpression, FilterExpression, ScanIndexForward, Limit, ExclusiveStartKey, Select |
| Scan | Supported | FilterExpression, Limit, ExclusiveStartKey, Select |
| ListTagsOfResource / TagResource / UntagResource | Supported | |
| All other APIs | Unsupported | Fails explicitly with `ValidationException` (HTTP 400); the SDK does not retry |

## Supported expressions and options

- `ExpressionAttributeNames` (`#name`) / `ExpressionAttributeValues` (`:name`)
- Condition / filter expressions: `=`, `<>`, `<`, `<=`, `>`, `>=`,
  `BETWEEN`, `IN`, `AND` / `OR` / `NOT`, `attribute_exists`,
  `attribute_not_exists`, `attribute_type`, `begins_with`, `contains`,
  `size`
- Update expressions: `SET` (`=`, `+`, `-`, `if_not_exists`,
  `list_append`), `REMOVE`, `ADD` (numbers and sets), `DELETE` (sets)
- Projection expressions: `attr`, `attr.nested`, `attr[0]` paths
- Query key conditions: partition key equality plus sort key
  `=` / `<` / `<=` / `>` / `>=` / `BETWEEN` / `begins_with`
- Attribute values: `S` `N` `B` `BOOL` `NULL` `L` `M` `SS` `NS` `BS`
- Sort key ordering: `S` is UTF-8 byte order (Unicode code point order),
  `N` is numeric order, `B` is byte order. Mixed types cannot occur
  within a key definition
- `N` values are stored and compared as strings, so there is no
  precision loss from float64 conversion

## Known limitations

- Unsupported APIs (Transact*, Batch*, PartiQL ExecuteStatement, etc.)
  fail with `ValidationException` (HTTP 400) — they never silently
  succeed
- GSI / LSI / Streams / TTL / backups / global tables are unsupported
- `ReturnConsumedCapacity` returns a fixed value (1.0)
- Pagination uses only `Limit` and `ExclusiveStartKey`; there is no
  1 MB response-size cutoff like the real service
- `ConsistentRead` is accepted, but local reads are always consistent
- Condition checks and writes are atomic via a SQLite transaction, and
  all operations are serialized through a single connection
- Exact parity with DynamoDB Local / real AWS is not guaranteed. See
  `docs/compat.md` for the compatibility verification status

## `Open` semantics

- `dynar.Open(path)`: creates the file if missing; validates the
  schema and reopens it if present
- `dynar.Open(":memory:")`: creates an independent volatile database
  per call
- A missing parent directory is not created (an
  `unable to open database file`-style error). Creating directories is
  the caller's responsibility
- Opening the same file through multiple handles is not supported
  (SQLite file locking protects against conflicts)
- The schema is versioned; files not managed by dynar or files from a
  newer version fail with an error instead of being initialized or
  destroyed

## Compatibility tests against DynamoDB Local

`internal/compat` contains a comparison suite that runs the same
scenarios against both dynar and the official DynamoDB Local. See
`docs/compat.md` for instructions. The normal `go test ./...` runs
with no external services.

## Design

- `*dynar.DB` returned by `Open` holds a SQLite `*sql.DB`; its lifetime
  is managed by Open/Close
- The transport returned by `HTTPClient()` dispatches on
  `X-Amz-Target` and uses the DynamoDB JSON protocol
  (`application/x-amz-json-1.0`) as its boundary. It does not depend on
  SDK middleware or per-operation mocks
- Each DynamoDB table maps to one SQLite table, with keys stored in
  BLOB columns using an order-preserving encoding
- The SQLite driver is the CGO-free `modernc.org/sqlite`. See
  `docs/design.md` for the rationale
