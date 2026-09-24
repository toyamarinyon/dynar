# dynar（ダイナー）

SQLite のファイルを開くように、ローカルの DynamoDB を使うための Go ライブラリです。

Docker も Java も外部サーバーも daemon も待受ポートも不要です。
AWS SDK for Go v2 が送信する HTTP リクエストをプロセス内で受け取り、
DynamoDB の操作を SQLite に変換して、SDK が理解するレスポンスを返します。

consumer の repository や CRUD コードは本番とローカルで共通のままにできます。
切り替えが必要なのは client の組み立て部分だけです。

## ファイルに永続化する

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

// ここから先は通常の AWS SDK のコード。
_, err = client.PutItem(ctx, &dynamodb.PutItemInput{
    TableName: aws.String("notes"),
    Item: map[string]types.AttributeValue{
        "id":   &types.AttributeValueMemberS{Value: "note-1"},
        "text": &types.AttributeValueMemberS{Value: "hello"},
    },
})
```

`db.HTTPClient()` が返すのは AWS SDK の `aws.HTTPClient` インターフェースを
満たす値で、`dynamodb.Options.HTTPClient` にそのまま渡せます。
リクエストはプロセス内で処理され、外部ネットワークには一切送信されません。
`http.DefaultClient` や環境変数、AWS 認証情報ファイルは変更されません。

完全なコピー実行可能なコードは `examples/file/` を参照してください。

## テストではメモリ上だけで動かす

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

- `Open(":memory:")` のたびに独立した DB が作られます。
- 並列テスト間でデータは共有されません。
- テスト用の代替 DynamoDB client インターフェースは不要です。
  repository は `*dynamodb.Client` のまま受け取れます。

`examples/testing/` に repository テストの例があります。

## 本番とローカルで業務コードを共有する

```go
// 起動時の依存組み立てでのみ切り替える。
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

repository は `*dynamodb.Client` を受け取り、dynar を import しません。
`examples/app/` に repository と切り替えコードを含む完全な例があります。

worktree ごとに `./.local/dynamo.db` を作れば環境を分離できます。
dynar 自身は Git や worktree を認識せず、namespace 管理も行いません。

## 対応 API

| API | 状態 | 備考 |
|---|---|---|
| CreateTable | 対応 | HASH / RANGE キー。BillingMode, DeletionProtectionEnabled, Tags 対応。GSI/LSI/Stream はエラー |
| DescribeTable | 対応 | waiter (`TableExists`) も動作 |
| ListTables | 対応 | Limit / ExclusiveStartTableName のページング対応 |
| DeleteTable | 対応 | DeletionProtectionEnabled を考慮 |
| PutItem | 対応 | ConditionExpression, ReturnValues(NONE/ALL_OLD), ReturnValuesOnConditionCheckFailure |
| GetItem | 対応 | ProjectionExpression, ConsistentRead は受理（ローカルでは常に consistent） |
| UpdateItem | 対応 | SET / REMOVE / ADD / DELETE。if_not_exists, list_append, + / - |
| DeleteItem | 対応 | ConditionExpression, ReturnValues(NONE/ALL_OLD) |
| Query | 対応 | KeyConditionExpression, FilterExpression, ScanIndexForward, Limit, ExclusiveStartKey, Select |
| Scan | 対応 | FilterExpression, Limit, ExclusiveStartKey, Select |
| ListTagsOfResource / TagResource / UntagResource | 対応 | |
| その他の API | 未対応 | `ValidationException`（HTTP 400）で明示的に失敗。SDK のリトライは発生しません |

## 対応する式・オプション

- `ExpressionAttributeNames`（`#name`）/ `ExpressionAttributeValues`（`:name`）
- 条件式 / フィルタ式: `=`, `<>`, `<`, `<=`, `>`, `>=`, `BETWEEN`, `IN`,
  `AND` / `OR` / `NOT`, `attribute_exists`, `attribute_not_exists`,
  `attribute_type`, `begins_with`, `contains`, `size`
- 更新式: `SET`（`=`、`+`、`-`、`if_not_exists`、`list_append`）、`REMOVE`、
  `ADD`（数値とセット）、`DELETE`（セット）
- 射影式: `attr`、`attr.nested`、`attr[0]` のパス
- Query のキー条件: partition key の等価 + sort key の
  `=` / `<` / `<=` / `>` / `>=` / `BETWEEN` / `begins_with`
- 属性値: `S` `N` `B` `BOOL` `NULL` `L` `M` `SS` `NS` `BS`
- sort key の順序: `S` は UTF-8 のバイト順（Unicode コードポイント順）、
  `N` は数値順、`B` はバイト列順。型の混在はキー定義上発生しません
- `N` は文字列のまま保存・比較するため、float64 変換による精度低下はありません

## 既知の制限

- 未対応の API（Transact*, Batch*, PartiQL の ExecuteStatement 等）は
  HTTP 400 の `ValidationException` で失敗します。黙って成功にはしません
- GSI / LSI / Streams / TTL / バックアップ / グローバルテーブルは未対応です
- `ReturnConsumedCapacity` を指定した場合、固定値（1.0）を返します
- ページングは `Limit` と `ExclusiveStartKey` のみで行います。
  実サービスのようなレスポンスサイズ（1MB）による打ち切りはありません
- `ConsistentRead` は受理しますが、ローカルでは常に consistent な読み取りです
- 条件判定と書き込みは SQLite のトランザクションで原子的に行われ、
  全操作は単一接続で直列化されます
- DynamoDB Local / 実 AWS との完全一致は保証しません。
  互換性の検証状況は `docs/compat.md` を参照してください

## `Open` の仕様

- `dynar.Open(path)`: ファイルがなければ作成し、あればスキーマを検査して再オープンします
- `dynar.Open(":memory:")`: 呼び出しごとに独立した揮発 DB を作ります
- 親ディレクトリが存在しない場合は作成しません（`unable to open database file` 相当のエラー）。
  ディレクトリの作成は呼び出し側の責任です
- 同じファイルを複数 handle で開くことは想定していません
  （プロセス内では同一ファイルへの `Open` は既存 handle と競合します。SQLite のロックで保護されます）
- スキーマにはバージョンがあり、dynar が管理していないファイルや
  未来のバージョンのファイルは初期化・破壊せずエラーにします

## DynamoDB Local との互換性テスト

同じシナリオを dynar と公式 DynamoDB Local の両方に実行する比較テストを
`internal/compat` に用意しています。手順は `docs/compat.md` を参照してください。
通常の `go test ./...` は外部サービスなしで実行できます。

## 設計

- `Open` が返す `*dynar.DB` が SQLite の `*sql.DB` を保持し、寿命は Open/Close で管理します
- `HTTPClient()` が返すトランスポートは `X-Amz-Target` で操作をディスパッチし、
  DynamoDB の JSON プロトコル（`application/x-amz-json-1.0`）を境界にします。
  SDK の middleware や operation 単位のモックには依存しません
- 各 DynamoDB テーブルは 1 つの SQLite テーブルに対応し、
  キーは順序を保存するエンコーディングで BLOB 列に格納します
- SQLite ドライバーは CGO 不要の `modernc.org/sqlite` を採用しています。
  選定理由は `docs/design.md` を参照してください
