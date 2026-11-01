# BarPay

`README.ja.md` is the Japanese translation of this file.

BarPay is a payment and pricing module that a bar plugs into its own mobile order system: a cover charge that moves with how crowded and lively the room is, walk-out checkout for members who linked PayPal, and register settlement for everyone else.

## What BarPay does and does not do

BarPay is an HTTP server that the store app calls from its own server; it has no screens.
It owns the visits, the cover charge and its AI pricing, and the PayPal charges, and tells the store app what happened through signed notifications. The store app owns the menu, the members and every screen, including the customer's panel and the staff's admin screen.

- Why BarPay exists: [Purpose](docs/PRD.md#1-purpose)
- What BarPay and the store app each own: [Responsibilities](docs/PRD.md#3-responsibilities)
- What BarPay does not do: [Out of scope](docs/PRD.md#7-out-of-scope)

## Running BarPay

BarPay runs from a config file you copy from the sample and fill in; it needs Go (the version in [`go.mod`](go.mod)) and the REST credentials of a PayPal sandbox app.

```sh
cp barpay.example.toml barpay.toml
```

Fill in every empty value in `barpay.toml`. The comments in the file say what each key takes, and [UC8](docs/PRD.md#uc8--store-connects-to-barpay) says which values may be left out and when BarPay refuses to start. `keys.store_api_key`, `keys.admin_api_key`, `keys.edge_shared_secret` and `notifications.signing_secret` are random strings you choose; the three under `[keys]` must differ from each other. If no LLM answers at `llm.address`, BarPay still starts and rate evaluations fall back as the [Pricing model](docs/PRD.md#4-pricing-model) describes. To run the sample app, take `store_app_origin` and `notifications.url` from the comments at the top of [`mobile-order.example.conf`](examples/mobile-order/mobile-order.example.conf).

```sh
go run ./cmd/barpay
```

BarPay is running when it logs `BarPay started` with the address it listens on. If a value is wrong, it says which one and does not start.

## API

The store app calls these operations from its server, under `/v1` of BarPay's URL, with the store API key or the admin API key from `barpay.toml`. Request and response shapes, errors and behaviour are in [`docs/api/openapi.yaml`](docs/api/openapi.yaml).

| Method | Path | Key | operationId | What it does |
|---|---|---|---|---|
| `POST` | `/checkins` | Store | `createCheckin` | Start a check-in |
| `GET` | `/sessions/{session_id}` | Store | `getSession` | Read a session |
| `POST` | `/sessions/{session_id}/orders` | Store | `reportOrder` | Report an order |
| `POST` | `/sessions/{session_id}/register-request` | Store | `requestRegister` | The customer chooses to pay at the register |
| `GET` | `/sessions/{session_id}/exit-token` | Store | `getExitToken` | Get the session's current exit token |
| `GET` | `/rate` | Store | `getRate` | Read the published rate |
| `GET` | `/rate/history` | Store | `getRateHistory` | Read the published rates over a period |
| `POST` | `/rate-evaluations` | Store | `requestRateEvaluation` | Ask BarPay to evaluate the rate |
| `GET` | `/members/{member_id}` | Store | `getMember` | Read a member's saved PayPal |
| `POST` | `/members/{member_id}/paypal-link` | Store | `createPaypalLink` | Ask for a PayPal link URL |
| `POST` | `/members/{member_id}/paypal-link/confirm` | Store | `confirmPaypalLink` | Confirm a PayPal link |
| `DELETE` | `/members/{member_id}/saved-paypal` | Store | `deleteSavedPaypal` | Delete a member's saved PayPal |
| `GET` | `/admin/sessions` | Admin | `listSessions` | List sessions |
| `POST` | `/admin/sessions/{session_id}/close` | Admin | `closeSession` | Staff close a linked session |
| `POST` | `/admin/sessions/{session_id}/retry` | Admin | `retryCharge` | Staff retry a failed charge |
| `POST` | `/admin/sessions/{session_id}/register-payment` | Admin | `recordRegisterPayment` | Staff record a payment at the register |
| `POST` | `/admin/sessions/{session_id}/settle-outside` | Admin | `settleOutside` | Staff settle a failed charge outside BarPay |
| `POST` | `/admin/sessions/{session_id}/mark-unpaid` | Admin | `markUnpaid` | Staff mark a session at the register as unpaid |
| `POST` | `/admin/gate/unlock` | Admin | `unlockGate` | Staff open the exit gate by hand |
| `GET` | `/admin/settings` | Admin | `getSettings` | Read the settings |
| `PUT` | `/admin/settings` | Admin | `updateSettings` | Change the settings |
| `POST` | `/admin/staff-events` | Admin | `sendStaffEvent` | Send a staff event |
| `GET` | `/admin/decisions` | Admin | `listDecisions` | Read the decision log |
| `GET` | `/admin/alerts` | Admin | `listAlerts` | Read the alerts |
| `GET` | `/admin/notifications` | Admin | `listNotifications` | Read the notification log |

BarPay sends these notifications, signed, to `notifications.url` in `barpay.toml`. Their shapes and how to check the signature are under `webhooks` in [`docs/api/openapi.yaml`](docs/api/openapi.yaml).

| Event type | When |
|---|---|
| `session.started` | A session started |
| `session.paypal_linked` | A session was linked to the member's saved PayPal |
| `session.register_requested` | The customer chose to pay at the register |
| `session.closed` | The session was checked out through BarPay |
| `session.payment_failed` | The checkout charge failed or could not be confirmed |
| `session.settled_outside` | Staff recorded a payment taken outside BarPay |
| `session.closed_unpaid` | The session was closed as unpaid |
| `gate.unlock_failed` | The session was paid but the exit gate did not unlock |
| `member.paypal_linked` | A member saved PayPal |
| `member.saved_paypal_deleted` | A member's saved PayPal was deleted |

## Running the development tools

The development tools in [`cmd/barpay-dev`](cmd/barpay-dev) stand in for the edge device's hardware ([Working without hardware](docs/PRD.md#8-fixed-constraints)): the virtual exit gate and the signal simulator.
They call BarPay through the edge device's API, [`docs/api/edge.yaml`](docs/api/edge.yaml), with the edge device's shared secret, which they read from an environment variable and never from `barpay.toml`. Run each in its own terminal at the repository root while BarPay is running, replacing the placeholder with the value of `keys.edge_shared_secret`:

```sh
export BARPAY_EDGE_SHARED_SECRET='<keys.edge_shared_secret in barpay.toml>'
go run ./cmd/barpay-dev gate
```

```sh
export BARPAY_EDGE_SHARED_SECRET='<keys.edge_shared_secret in barpay.toml>'
go run ./cmd/barpay-dev signals
```

The virtual exit gate takes the text of an exit QR, one per line, as a QR reader would type it: read the exit QR the store app shows with any QR scanner and paste its text. The signal simulator sends the room's liveliness until you stop it, and takes a new value, one per line. Run without arguments, the tools print their usage, including what else they take and the `-barpay` flag for a BarPay that listens elsewhere, and exit with an error status:

```sh
go run ./cmd/barpay-dev
```

## Running the sample app

[`examples/mobile-order`](examples/mobile-order) is a sample store app: a bar's mobile order system that uses BarPay only through the API and the notifications above.
It is a separate Go module that does not depend on BarPay's code, and draws the screens that the sample app's row of [Priorities](docs/PRD.md#11-priorities) names. It needs BarPay running with `store_app_origin` and `notifications.url` set as described in [Running BarPay](#running-barpay).

```sh
cd examples/mobile-order
cp mobile-order.example.conf mobile-order.conf
```

Fill in every empty value in `mobile-order.conf`: the store API key, the admin API key and the notification signing secret from `barpay.toml`, and a `staff` line with a staff ID and a password hash, as its comment shows. This command reads the password and prints the hash:

```sh
go run . hash-password
```

```sh
go run .
```

The sample app is running when it logs the URL to open. Open that URL in a browser for the entrance landing; staff sign in to the admin screen at `/admin`.

## Next to read

- [`docs/PRD.md`](docs/PRD.md): what BarPay does, guarantees and leaves out
- [`docs/api/openapi.yaml`](docs/api/openapi.yaml): the contract between the store app and BarPay
- [`CONTRIBUTING.md`](CONTRIBUTING.md): how to propose a change
- [`LICENSE`](LICENSE): the license
