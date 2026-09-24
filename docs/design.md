# dynar 設計メモ

## 公開 API

公開面は意図的に最小にする。

```go
type DB struct{ ... }

func Open(path string) (*DB, error)
func (db *DB) Close() error
func (db *DB) HTTPClient() aws.HTTPClient
```

- `Open` はファイルパスまたは `":memory:"` を受け取る。
- `HTTPClient()` は AWS SDK v2 の `aws.HTTPClient`（`Do(*http.Request)`）を返す。
  `dynamodb.Options.HTTPClient` に直接渡す。
- 将来必要になったときだけ `Option` 関数や `Must` を足せるように、
  初版では追加しない。

## 境界: HTTP / DynamoDB プロトコル

SDK は DynamoDB に `application/x-amz-json-1.0` の JSON RPC を話す
（`X-Amz-Target: DynamoDB_20120810.<Op>`、POST `/`）。
dynar はこのリクエスト/レスポンス形式だけを実装する。
SDK の middleware・リトライ・認証・エンドポイント解決には一切手を入れない。

- リクエストの URL・Authorization は無視する（検証もしない）。
- エラーは `{ "__type": "com.amazonaws.dynamodb.v20120810#<Type>", "message": "..." }`
  と HTTP 400 で返す。SDK は型付き例外にデコードし、`errors.As` が効く。
- 未対応操作は `ValidationException`（400、非リトライ）で即座に失敗させる。
  5xx や接続エラーを返すと SDK がリトライして待たされるため使わない。
- Close 後のリクエストも同じく 400 で失敗させる（SDK のリトライを避けるため
  Go エラーではなく HTTP エラーレスポンスを返す）。
- context のキャンセルは `req.Context().Done()` を各処理の入口で確認し、
  `*url.Error` に包んで返す。

## SQLite ドライバー選定: `modernc.org/sqlite`

候補:

| ドライバー | CGO | 備考 |
|---|---|---|
| `github.com/mattn/go-sqlite3` | 必要 | 実績十分だが consumer に C ツールチェーンを要求する |
| `modernc.org/sqlite` | 不要 | SQLite を Go に変換した実装。`database/sql` 準拠 |
| `zombiezen.com/go/sqlite` | 不要 | 高機能だが独自 API が中心で `database/sql` との併用は別レイヤ |

採用: **modernc.org/sqlite**。consumer のインストール体験（`go build` が
そのまま通る、クロスコンパイルが効く）を最優先した。
依存サイズは大きい（SQLite 全体を含む）が、dev 用途のライブラリとして許容する。
`database/sql` 経由なので将来ドライバーを差し替えることも可能。

## 接続モデル

- `*sql.DB` に `SetMaxOpenConns(1)` を設定し、全操作を 1 接続に直列化する。
  - `:memory:` で接続ごとに別 DB が作られる問題を回避する。
  - `SQLITE_BUSY` を構造的に排除し、条件判定+書き込みの原子性を
    単純なトランザクションで保証する。
  - ローカル開発用途ではスループットは問題にならない。
- `SetMaxIdleConns(1)`、`ConnMaxLifetime`/`IdleTime` 無制限で、
  接続が途中で閉じて in-memory DB が消失しないようにする。

## ストレージスキーマ

```
dynar_meta(key TEXT PRIMARY KEY, value TEXT)     -- schema_version
dynar_tables(name TEXT PRIMARY KEY,              -- テーブルカタログ
             hash_key TEXT, hash_type TEXT,
             range_key TEXT, range_type TEXT,    -- NULL = ソートキーなし
             status TEXT, created_at REAL,
             billing_mode TEXT, deletion_protection INT,
             provisioned_rcu INT, provisioned_wcu INT,
             tags TEXT)                          -- JSON
dynar_data_<sanitized>(                          -- テーブルごとに1つ
    pk   BLOB NOT NULL,
    sk   BLOB NOT NULL DEFAULT '',
    item TEXT NOT NULL,                          -- AttributeValue map の正準 JSON
    PRIMARY KEY (pk, sk))
```

- `pk`/`sk` は**順序を保存するキーエンコーディング**で格納する。
  Query の `ORDER BY pk, sk` がそのまま DynamoDB のソート順になる。
  - `S`: `0x01` + UTF-8（0x00 を `0x00 0xFF` にエスケープ、終端 `0x00 0x00`）
  - `N`: `0x02` + 符号 + 指数(2バイト, biased) + 仮数の十進桁列。
    正規化は文字列上で行い、float64 を通さないので精度を失わない。
    負数はペイロードをビット反転して逆順にする。
  - `B`: `0x03` + バイト列（同様にエスケープ+終端）
- `item` 列は DynamoDB 形式の JSON（`{"attr":{"S":"v"}}`）をそのまま保持。
  属性値コーデックは自前で実装し、N は文字列のまま往復する。
- テーブル名→SQLite テーブル名の変換は `dynar_data_` プレフィックス + 
  ダブルクォートでエスケープ（DynamoDB のテーブル名文字種 `[a-zA-Z0-9_.-]` では
  インジェクションは成立しないが、防御のため `sqlite3_str` 相当のクォートを行う）。

## `Open` の仕様

- 新規ファイル: 作成してスキーマを初期化、`schema_version=1` を記録。
- 既存ファイル: `dynar_meta` の存在と `schema_version` を検査。
  - dynar 管理でないファイル（メタテーブルなし）→ エラー（初期化しない）。
    ※ 空ファイル（サイズ 0）は新規とみなして初期化する。
  - 未来のバージョン → エラー。
- `":memory:"`: `Open` ごとに独立した DB（単一接続の :memory:）。
- 親ディレクトリがなければ SQLite のオープンエラーをそのまま返す
  （ディレクトリを暗黙作成しない）。
- 同一ファイルの複数 handle: SQLite のファイルロックに委ねる。
  WAL は使わずデフォルトのジャーナルモード。

## 式エンジン

自前の小さな tokenizer + 再帰下降パーサで実装する。

- Condition / Filter / KeyCondition: 比較、`BETWEEN`、`IN`、`AND/OR/NOT`、
  `attribute_exists` / `attribute_not_exists` / `attribute_type` /
  `begins_with` / `contains` / `size`
- Update: `SET`（`=`、`+`/`-`、`if_not_exists`、`list_append`）、
  `REMOVE`、`ADD`、`DELETE`
- Projection: `a`, `a.b`, `a[0]` のパス
- `#name` → ExpressionAttributeNames、`:name` → ExpressionAttributeValues
- DynamoDB の予約語リストを持ち、裸の識別子が予約語なら `ValidationException`。
- 対応外の構文・関数はパース時点で `ValidationException` にする。
  無視して近似しない。

## エラー対応

| DynamoDB 例外 | 使う場面 |
|---|---|
| ValidationException | 入力不正・未対応 API/式/オプション |
| ResourceNotFoundException | 存在しないテーブル |
| ResourceInUseException | 既存テーブルの作成・削除中の操作 |
| ConditionalCheckFailedException | 条件失敗（ReturnValuesOnConditionCheckFailure=ALL_OLD で Item を同梱） |
| InternalServerError | dynar 内部の予期しない失敗 |

## 互換性テスト

`internal/compat` に `*dynamodb.Client` を受け取る共通シナリオを置き、

- 通常テスト: dynar（:memory: とファイル）に対して実行
- `//go:build dynamodblocal`: `DYNAMODB_LOCAL_ENDPOINT` への client で実行

として両方に走らせる。比較テスト実行時に接続できなければ skip せず失敗。
DynamoDB Local のバージョンは `docs/compat.md` に固定して記載。
