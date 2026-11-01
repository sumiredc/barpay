# BarPay

> `README.md` の日本語訳。内容が食い違う場合は `README.md` が正。

BarPay は、バーが自分のモバイルオーダーに組み込む決済と値付けのモジュール。店の混み具合と賑わいで動くチャージ料、PayPal を連携した会員の歩いて出るだけの精算、それ以外の客のレジ精算を受け持つ。

## BarPay が何をし、何をしないか

BarPay は、店のアプリが自分のサーバーから呼ぶ HTTP のサーバーで、画面を持たない。
来店、チャージ料とその AI の値付け、PayPal での請求を受け持ち、起きたことを署名付きの通知で店のアプリに伝える。店のアプリは、メニュー、会員と、客のパネルやスタッフの管理画面を含むすべての画面を受け持つ。

- BarPay がなぜあるか：[目的](docs/PRD.ja.md#1-目的)
- BarPay と店のアプリがそれぞれ何を受け持つか：[責務の分担](docs/PRD.ja.md#3-責務の分担)
- BarPay がしないこと：[扱わないこと](docs/PRD.ja.md#7-扱わないこと)

## BarPay の起動

BarPay は、見本からコピーして値を入れた設定ファイルで動く。Go（版は [`go.mod`](go.mod) のもの）と、PayPal の sandbox のアプリの REST の資格情報が要る。

```sh
cp barpay.example.toml barpay.toml
```

`barpay.toml` の空の値をすべて埋める。各キーに何を入れるかはファイルのコメントにあり、どの値を省けるか、どんなときに BarPay が起動しないかは [UC8](docs/PRD.ja.md#uc8--店が-barpay-とつながる) にある。`keys.store_api_key`、`keys.admin_api_key`、`keys.edge_shared_secret`、`notifications.signing_secret` は自分で決めたランダムな文字列で、`[keys]` の各値は互いに違うものにする。`llm.address` で LLM が答えなくても BarPay は起動し、レートの見直しは [価格モデル](docs/PRD.ja.md#4-価格モデル) のとおり代替動作になる。サンプルアプリを動かすなら、`store_app_origin` と `notifications.url` は [`mobile-order.example.conf`](examples/mobile-order/mobile-order.example.conf) の先頭のコメントから取る。

```sh
go run ./cmd/barpay
```

`BarPay started` と待ち受けるアドレスをログに出せば、BarPay は動いている。値が間違っていれば、どの値かを言って起動しない。

## API

店のアプリは、次の操作を自分のサーバーから、BarPay の URL の `/v1` の下に、`barpay.toml` の店の API キーか管理の API キーで呼ぶ。要求と応答の形、エラー、振る舞いは [`docs/api/openapi.yaml`](docs/api/openapi.yaml) にある。

| メソッド | パス | 鍵 | operationId | 何をするか |
|---|---|---|---|---|
| `POST` | `/checkins` | 店 | `createCheckin` | 入店を始める |
| `GET` | `/sessions/{session_id}` | 店 | `getSession` | セッションを読む |
| `POST` | `/sessions/{session_id}/orders` | 店 | `reportOrder` | 注文を伝える |
| `POST` | `/sessions/{session_id}/register-request` | 店 | `requestRegister` | 客がレジで払うことを選ぶ |
| `GET` | `/sessions/{session_id}/exit-token` | 店 | `getExitToken` | セッションの今の退店トークンを得る |
| `GET` | `/rate` | 店 | `getRate` | 公開中のレートを読む |
| `GET` | `/rate/history` | 店 | `getRateHistory` | ある期間に公開したレートを読む |
| `POST` | `/rate-evaluations` | 店 | `requestRateEvaluation` | BarPay にレートの見直しを頼む |
| `GET` | `/members/{member_id}` | 店 | `getMember` | 会員の保存した PayPal を読む |
| `POST` | `/members/{member_id}/paypal-link` | 店 | `createPaypalLink` | PayPal 連携の URL を求める |
| `POST` | `/members/{member_id}/paypal-link/confirm` | 店 | `confirmPaypalLink` | PayPal 連携を確定する |
| `DELETE` | `/members/{member_id}/saved-paypal` | 店 | `deleteSavedPaypal` | 会員の保存した PayPal を削除する |
| `GET` | `/admin/sessions` | 管理 | `listSessions` | セッションの一覧 |
| `POST` | `/admin/sessions/{session_id}/close` | 管理 | `closeSession` | スタッフが連携済みのセッションを締める |
| `POST` | `/admin/sessions/{session_id}/retry` | 管理 | `retryCharge` | スタッフが失敗した請求を再試行する |
| `POST` | `/admin/sessions/{session_id}/register-payment` | 管理 | `recordRegisterPayment` | スタッフがレジでの支払いを記録する |
| `POST` | `/admin/sessions/{session_id}/settle-outside` | 管理 | `settleOutside` | スタッフが失敗した請求を BarPay の外で精算する |
| `POST` | `/admin/sessions/{session_id}/mark-unpaid` | 管理 | `markUnpaid` | スタッフがレジ待ちのセッションを未払いにする |
| `POST` | `/admin/gate/unlock` | 管理 | `unlockGate` | スタッフが手で出口ゲートを開ける |
| `GET` | `/admin/settings` | 管理 | `getSettings` | 設定を読む |
| `PUT` | `/admin/settings` | 管理 | `updateSettings` | 設定を変える |
| `POST` | `/admin/staff-events` | 管理 | `sendStaffEvent` | スタッフのイベントを送る |
| `GET` | `/admin/decisions` | 管理 | `listDecisions` | 判断ログを読む |
| `GET` | `/admin/alerts` | 管理 | `listAlerts` | 警告を読む |
| `GET` | `/admin/notifications` | 管理 | `listNotifications` | 通知の記録を読む |

BarPay は次の通知を、署名して `barpay.toml` の `notifications.url` に送る。形と署名の確かめ方は [`docs/api/openapi.yaml`](docs/api/openapi.yaml) の `webhooks` にある。

| イベントの種類 | いつ |
|---|---|
| `session.started` | セッションが始まった |
| `session.paypal_linked` | セッションが会員の保存した PayPal に結び付いた |
| `session.register_requested` | 客がレジで払うことを選んだ |
| `session.closed` | セッションが BarPay で精算された |
| `session.payment_failed` | 精算の請求が失敗した、または確かめられなかった |
| `session.settled_outside` | スタッフが BarPay の外で受け取った支払いを記録した |
| `session.closed_unpaid` | セッションが未払いで閉じられた |
| `gate.unlock_failed` | 支払い済みのセッションで出口ゲートが開かなかった |
| `member.paypal_linked` | 会員が PayPal を保存した |
| `member.saved_paypal_deleted` | 会員の保存した PayPal が削除された |

## 開発用ツールの動かし方

[`cmd/barpay-dev`](cmd/barpay-dev) の開発用ツールは、エッジ端末のハードウェアの代わりをする（[ハードウェア無しでの開発](docs/PRD.ja.md#8-動かせない前提)）。仮想の出口ゲートと、シグナルのシミュレーター。
ツールはエッジ端末の API（[`docs/api/edge.yaml`](docs/api/edge.yaml)）で BarPay を呼び、エッジ端末の共有シークレットを環境変数から読む。`barpay.toml` は読まない。BarPay を動かしたまま、リポジトリの直下で、それぞれを別の端末で動かす。プレースホルダーは `keys.edge_shared_secret` の値に置き換える。

```sh
export BARPAY_EDGE_SHARED_SECRET='<keys.edge_shared_secret in barpay.toml>'
go run ./cmd/barpay-dev gate
```

```sh
export BARPAY_EDGE_SHARED_SECRET='<keys.edge_shared_secret in barpay.toml>'
go run ./cmd/barpay-dev signals
```

仮想の出口ゲートは、退店 QR の中身を、QR リーダーが打ち込むように行ごとに受け取る。店のアプリが出す退店 QR を任意の QR スキャナーで読み、その中身を貼り付ける。シグナルのシミュレーターは、止めるまで店の賑わいを送り、新しい値を行ごとに受け取る。引数無しで動かすと、ツールは使い方（ほかに何を受け取るかと、別の場所で待ち受ける BarPay を指す `-barpay` フラグを含む）を表示し、エラーの終了ステータスで終わる。

```sh
go run ./cmd/barpay-dev
```

## サンプルアプリの動かし方

[`examples/mobile-order`](examples/mobile-order) は店のアプリの見本で、上の API と通知だけで BarPay を使うバーのモバイルオーダー。
BarPay のコードに依存しない別の Go モジュールで、[優先度](docs/PRD.ja.md#11-優先度) のサンプルアプリの行が挙げる画面を描く。[BarPay の起動](#barpay-の起動) のとおりに `store_app_origin` と `notifications.url` を設定した BarPay が動いている必要がある。

```sh
cd examples/mobile-order
cp mobile-order.example.conf mobile-order.conf
```

`mobile-order.conf` の空の値をすべて埋める。`barpay.toml` の店の API キー、管理の API キー、通知の署名用シークレットと、コメントにあるとおりスタッフの ID とパスワードのハッシュを持つ `staff` の行。次のコマンドがパスワードを読んでハッシュを表示する。

```sh
go run . hash-password
```

```sh
go run .
```

開く URL をログに出せば、サンプルアプリは動いている。その URL をブラウザで開くと入口の画面が出る。スタッフは `/admin` で管理画面にログインする。

## 次に読むもの

- [`docs/PRD.ja.md`](docs/PRD.ja.md)：BarPay が何をし、何を保証し、何を扱わないか
- [`docs/api/openapi.yaml`](docs/api/openapi.yaml)：店のアプリと BarPay の間の契約
- [`CONTRIBUTING.ja.md`](CONTRIBUTING.ja.md)：変更の出し方
- [`LICENSE`](LICENSE)：ライセンス
