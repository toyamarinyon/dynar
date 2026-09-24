# DynamoDB Local との互換性検証

`internal/compat` に、`*dynamodb.Client` を受け取る共通シナリオ群を置いている。
同じシナリオを dynar（in-memory / ファイル）と公式 DynamoDB Local の両方に実行する。

## 参照実装

- DynamoDB Local **3.3.1**（`amazon/dynamodb-local` イメージ）
- 一致は実 AWS DynamoDB との完全互換を保証しない。DynamoDB Local 自身が
  一部の挙動（スループット制限、項目サイズ、トランザクション分離など）で
  実サービスと異なる。dynar が保証するのは「DynamoDB Local と同じ
  API 契約」まで。

## 実行

通常テスト（外部サービス不要）:

```sh
go test ./...
```

比較テスト（DynamoDB Local が必要）:

```sh
# 起動（container / Docker どちらでも）
container run --name dynamodb-local -p 8000:8000 \
  amazon/dynamodb-local:3.3.1 -jar DynamoDBLocal.jar -sharedDb -inMemory
# もしくは: docker run -p 8000:8000 amazon/dynamodb-local:3.3.1 -jar DynamoDBLocal.jar -sharedDb -inMemory

# 実行。接続できなければ skip ではなく失敗する。
go test -tags dynamodblocal ./internal/compat/

# 既に稼働しているインスタンスを使う場合
DYNAMODB_LOCAL_ENDPOINT=http://localhost:8000 go test -tags dynamodblocal ./internal/compat/
```

比較テストは `l<timestamp>-*` プレフィックスの一意なテーブル名を使い、
終了時に削除する。既存データには触れない。

## 比較している項目

- 属性値の round-trip（S/N/B/BOOL/NULL/L/M/SS/NS/BS、大数値の精度）
- 存在しない項目・テーブルへの操作のエラー型
- 条件式の成功・失敗、失敗時にデータが変わらないこと
- 更新式（SET/REMOVE/ADD/DELETE、if_not_exists、list_append、+/-）
- Query の順序・ScanIndexForward・begins_with・BETWEEN・Limit・
  LastEvaluatedKey・ページング
- PutItem/UpdateItem/DeleteItem の ReturnValues
- 予約語・キー不整合の ValidationException
- エラー型と HTTP status（400 + `__type`）

実行ごとに変わる値（RequestID、CreationDateTime、ARN のアカウント部など）は
比較しない。エラーメッセージ全文の一致は要求せず、エラー型と状態を比較する。

## 検証で見つけて修正した差分

- `ADD` + `DELETE` など、同一 UpdateExpression 内のパス重複は
  `ValidationException`（"Two document paths overlap"）で拒否する。
- `list_append` の対象属性が存在しない場合は失敗する
  （`list_append(if_not_exists(l, :empty), :v)` が正しいイディオム）。
- 型の異なるスカラー同士の順序比較は条件式では「偽」に評価され、
  `ConditionalCheckFailedException` になる（バリデーションエラーではない）。
- キー属性の空文字列・空バイナリは `ValidationException`。

## 未検証

- 実 AWS DynamoDB そのものとの比較（有料リソースを作らないため未実施）
- 1MB ページサイズ制限（dynar はサイズベースのページングを行わない）
- ListTables の `Limit=0` の扱い
