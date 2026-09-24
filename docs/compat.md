# Compatibility verification against DynamoDB Local

`internal/compat` contains shared scenarios that take a
`*dynamodb.Client`. The same scenarios run against both dynar
(in-memory / file) and the official DynamoDB Local.

## Reference implementation

- DynamoDB Local **3.3.1** (the `amazon/dynamodb-local` image)
- Matching DynamoDB Local does not guarantee full compatibility with
  real AWS DynamoDB. DynamoDB Local itself differs from the real
  service in some behaviors (throughput limits, item size, transaction
  isolation, etc.). What dynar guarantees is "the same API contract as
  DynamoDB Local".

## Running

Normal tests (no external services required):

```sh
go test ./...
```

Comparison tests (requires DynamoDB Local):

```sh
# Start it (works with either `container` or Docker)
container run --name dynamodb-local -p 8000:8000 \
  amazon/dynamodb-local:3.3.1 -jar DynamoDBLocal.jar -sharedDb -inMemory
# or: docker run -p 8000:8000 amazon/dynamodb-local:3.3.1 -jar DynamoDBLocal.jar -sharedDb -inMemory

# Run. If the endpoint is unreachable, tests fail rather than skip.
go test -tags dynamodblocal ./internal/compat/

# To use an already-running instance
DYNAMODB_LOCAL_ENDPOINT=http://localhost:8000 go test -tags dynamodblocal ./internal/compat/
```

Comparison tests use unique table names prefixed `l<timestamp>-` and
delete them on completion. Existing data is never touched.

## What is compared

- Attribute-value round-trip (S/N/B/BOOL/NULL/L/M/SS/NS/BS, precision
  of large numbers)
- Error types for operations on missing items/tables
- Condition success/failure and that failed conditions leave data
  unchanged
- Update expressions (SET/REMOVE/ADD/DELETE, if_not_exists,
  list_append, +/-)
- Query ordering, ScanIndexForward, begins_with, BETWEEN, Limit,
  LastEvaluatedKey, pagination
- ReturnValues for PutItem/UpdateItem/DeleteItem
- ValidationException for reserved words and key mismatches
- Error types and HTTP status (400 + `__type`)

Values that vary per run (RequestID, CreationDateTime, the account
portion of ARNs, etc.) are not compared. Full error-message equality is
not required; error types and resulting state are compared.

## Divergences found and fixed during verification

- Overlapping document paths within one UpdateExpression (e.g. `ADD` +
  `DELETE`) are rejected with `ValidationException` ("Two document
  paths overlap").
- `list_append` fails when the target attribute does not exist
  (`list_append(if_not_exists(l, :empty), :v)` is the correct idiom).
- Ordering comparisons between scalars of different types evaluate to
  false in condition expressions, producing
  `ConditionalCheckFailedException` (not a validation error).
- Empty string/binary key attribute values yield `ValidationException`.

## Not yet verified

- Comparison against real AWS DynamoDB itself (not performed to avoid
  creating paid resources)
- The 1 MB page-size limit (dynar does not implement size-based
  pagination)
- Handling of `Limit=0` in ListTables
