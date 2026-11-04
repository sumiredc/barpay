# PRD — BarPay

`PRD.ja.md` is the Japanese translation of this file.

## 1. Purpose

BarPay is a payment and pricing module that a bar plugs into its own mobile order system. It adds a cover charge that moves like a stock price with how crowded and lively the room is, and lets a member who linked PayPal settle the whole visit by walking out.

- **Customers** pay a flat cover charge (table charge / *otoshi* in Japan) whether the bar is dead or packed, and often learn about it only on the bill. Foreign visitors in particular find it surprising. They cannot see what they are paying for while they stay, and they queue at the register to leave.
- **Bar owners** cannot lower prices to fill a slow evening or capture value on a packed one. Mobile order systems take orders, but none of them prices the cover charge by demand or lets the customer simply walk out.
- **Mobile order system developers** would have to build demand pricing and walk-out settlement themselves.

BarPay solves this as a module: the store keeps its own app, menu, members and screens; BarPay adds the live cover charge, an optional PayPal link for members that turns the exit QR into the checkout on every visit, and a register path for everyone else, paid into the store's own PayPal account. BarPay has no screens of its own: it offers an API and signed notifications, and the store app decides what to show and when to call BarPay.

## 2. Users

| User | Goal |
|---|---|
| Customer | Enter, drink and eat, see what they are paying, and leave by walking out (a member with PayPal linked) or by paying at the register |
| Store app | The store's mobile order system (here: the sample app). Starts visits, reports orders, asks for rate evaluations, draws every screen (the customer's panel and the staff's admin screen), and learns how each visit was settled |
| Staff / owner | Watch the room's state and handle the exceptions on the store app's admin screen; the owner sets prices, limits and cover-charge rules there |

## 3. Responsibilities

BarPay owns what is about the cover charge and the money; the store app owns everything about the menu, the customer relationship and the screens.

| BarPay | Store app |
|---|---|
| Visits (sessions): check-in, stay, checkout, register settlement | The entrance landing reached from the store's QR / NFC tag, login or guest join |
| The cover charge and its AI pricing, evaluated when the store app asks | Menu, order screen, kitchen; item prices and order contents; when to ask for a rate evaluation |
| PayPal: saving a member's PayPal (optional), the checkout charge, deleting it when the member asks | Members and login; the settings screen where a member links or deletes their PayPal, with the consent shown before linking |
| Sending the member to PayPal approval and handling the return, without showing a page | Where the customer goes before and after PayPal |
| Exit tokens and the decision to unlock the exit gate | The panel: rate ticker, running total, the Pay button with the choice of PayPal (exit QR) or register |
| The data and operations for running the room, through the admin API | The admin screen and the authentication of its staff |
| Signed notifications of every result | Receipts and history in its own UI (from BarPay's results) |

```mermaid
sequenceDiagram
  actor C as Customer
  participant A as Store app
  participant B as BarPay
  participant P as PayPal
  C->>A: scan entrance QR / NFC, log in or join as guest
  A->>B: start check-in
  B-->>A: session started (webhook)
  loop on the store app's schedule
    A->>B: ask for a rate evaluation
  end
  C->>A: order
  A->>B: order lines and amounts
  opt member links PayPal (once)
    C->>A: agree to the consent text
    A->>B: ask for a link URL (member ID, consent)
    C->>B: open the link URL (redirected to PayPal)
    C->>P: approve saving PayPal
    P->>B: return
    B->>C: redirect to the store app with a one-time code
    A->>B: confirm the link (code, logged-in member ID)
    B->>P: save PayPal (no payment)
    B-->>A: PayPal linked to the member (webhook)
  end
  alt PayPal linked
    A->>B: get an exit token, show it as a QR
    C->>B: exit QR at the gate
    B->>P: charge the total with saved PayPal
    B-->>A: session closed + receipt (webhook)
  else pay at the register
    C->>A: choose "Pay at register" in the panel
    A->>B: register requested
    B-->>A: register requested (webhook)
    Note over A: staff record the payment on the admin screen
    A->>B: record the register payment (admin API)
    B-->>A: settled outside BarPay (webhook)
  end
```

Read: checking in needs no PayPal; a member approves PayPal once, and every later visit uses it without asking; the store app and BarPay talk server to server, and the customer's browser passes through BarPay only on its way to and from PayPal.

Boundary rules:

- **Check-in**: the store app starts a check-in from its server; the session starts at once and its ID is returned.
- **PayPal link**: linking belongs to a member, not to a visit, and guests cannot link. Before linking, the store app shows the member that the visit's total will be charged at checkout without asking again, and asks BarPay for a link URL for the logged-in member, naming the version of the consent text it showed. The member opens the URL as a full page, never inside a frame, because PayPal approval is not expected to run inside an embedded frame (believed, not verified). A link URL works for 30 minutes from when it was made, up to just before that instant, and a new request replaces an earlier unused one. BarPay shows no page: it redirects the browser to PayPal, and when PayPal returns, it checks the approval and redirects the browser to the store app with a one-time code. The return carries only the result, the one-time code and the store app's own `state` value, never a PayPal token, and BarPay's redirects send no referrer. The store app binds `state` to the browser session that asked for the link, so the member must approve in the same browser where they are logged in. The link is saved only when the store app's server, after checking that `state` belongs to that browser session, confirms it with the code and the logged-in member's ID, and only if that is the member the link URL was made for. When a member with a saved PayPal checks in, BarPay links the session by itself.
- **Rate evaluation**: the store app asks BarPay to evaluate the rate on its own schedule (the sample app: every 5 minutes). BarPay does not evaluate on its own and does not know the store's opening hours.
- **Lookup**: the store app can read, at any time, a session's state, totals, rate history over the visit and receipt by its ID; whether a member has a saved PayPal by the member ID; and the published rate. This is how it draws the panel, confirms the result when the customer returns, decides whether its settings screen offers linking or deleting, and recovers missed notifications.
- **Admin**: the store app's admin screen uses the admin API to list sessions, read the rate history with its explanations, the decision log, alerts and the notification log, change settings, send staff events, and act on sessions (record a register payment, mark unpaid, close, retry a charge, settle outside BarPay, open the gate by hand). Each admin request names the staff member acting, and BarPay records it. The store app authenticates its own staff. Every list (sessions, decisions, alerts, notifications) is newest first and read a page at a time, each page handing a cursor for the next one the same way; a cursor that is malformed, comes from another list or names nothing in the list is refused, and items added while paging do not shift the pages. The order is the one BarPay stored the items in, the same on every read, also among items with the same time. A listed session shows what its lookup shows at the same moment.
- **Exit token**: for an active linked session, or a session closed (charged, or recorded by staff) within the re-unlock window, the store app's server asks BarPay for the current exit token and shows it only to that session's customer as a QR. The token changes at a fixed interval until the session closes, and works only at the exit gate.
- **Notifications**: the events are `session.started`, `session.paypal_linked`, `session.register_requested`, `session.closed`, `session.payment_failed`, `session.settled_outside`, `session.closed_unpaid`, `gate.unlock_failed`, and for members `member.paypal_linked` and `member.saved_paypal_deleted`. Each carries an event ID and a sequence number per session (per member for `member.*` events) and is signed. Notifications go to the notification URL in the config file, which uses https except for http to a loopback IP address ([UC8](#uc8--store-connects-to-barpay)). A notification not acknowledged is retried 1 minute, 5 minutes, 30 minutes, 2 hours, 6 hours, 12 hours and 24 hours after the first attempt; after that it is marked as given up in the notification log and raised as an alert. Only a 2xx answer acknowledges a notification; a redirect is not followed. The retry times are counted from the stored time of the first attempt, so a restart keeps them. If one or more of them passed before BarPay could send (it was stopped, or still sending earlier notifications), it sends the notification once as soon as it can, not once per missed time, and goes on with the next retry time still ahead; if the last one (24 hours) passed that way, that attempt is the last, and the notification is given up if it is not acknowledged. A notification stored just before BarPay stopped is first sent when it starts again. A session ends with exactly one of `session.closed` (checked out through BarPay: charged, or a total of 0), `session.settled_outside` (staff recorded a register or outside payment) or `session.closed_unpaid`, and that event carries the receipt.
- **Panel**: the store app's own screen. BarPay gives it, through lookup, the live rate, its change and history, whether the cover charge is paused or capped, time stayed, cover charge so far, orders, payment method (PayPal or register), the running total and, after checkout, the result. Pay opens a choice: PayPal (shown only when linked) shows the exit QR; "Pay at register" asks BarPay to fix the total for staff to collect.
- **API keys**: the store app calls BarPay only from its server, with the store API key for customer-facing calls and a separate admin API key for admin calls; the sample app has a backend that holds both.

## 4. Pricing model

The cover charge is a **rate per hour** that changes over time, charged in **blocks**: every block (settings, default 10 minutes) after check-in, the block that just ended is charged at the rate published at that moment.

```mermaid
flowchart LR
  S1["Occupancy<br/>(active sessions / capacity)"] --> AI
  S2["Liveliness<br/>(microphone loudness on the edge device)"] --> AI
  S4["Staff events<br/>(e.g. live show starts)"] --> AI
  Goal["Owner's goal<br/>(fill the room ↔ maximize revenue)"] --> AI
  AI["AI pricing agent"] --> G["Guardrails<br/>floor · ceiling · max step"]
  G --> P["Published rate"]
  P --> C["Store app's panel<br/>(ticker chart)"]
  G --> D["Store app's admin screen<br/>(explanation + decision log)"]
```

The AI decides the rate toward the owner's goal; guardrails set by the owner decide what can actually be charged.

- **Signals**: occupancy and liveliness (the author's core inputs), plus staff events. Occupancy is the number of active sessions divided by capacity (config file), at most 1 (full): it counts checked-in phones, not people, and no camera counts people. Only active sessions are counted, not those checking out, at the register, whose payment failed, closed or closed unpaid: one at the register or whose payment failed can stay so for up to 3 days, after the customer may have left, and one checking out is a customer on the way out. Liveliness is computed on the edge device from the microphone's loudness only, without classifying the sound, and only numbers leave the device; without the edge device there is no liveliness signal, and in development the signal simulator supplies it. Liveliness is one number from 0 (quiet) to 1 (loudest), sent about every 10 seconds; BarPay uses the last value it received for 1 minute after receiving it. From exactly 1 minute on there is no liveliness signal, as without the edge device, so a stopped edge device does not leave an old value in use. The edge device measures liveliness over each 10-second interval it sends: the interval's loudness is the root mean square (RMS) of all its samples in dBFS (full scale is 1, so silence is −∞), mapped in a straight line from the store's quiet loudness (liveliness 0) to its loudest (liveliness 1); a loudness at or below the quiet one is 0, and at or above the loudest one is 1. The average is taken over the sending interval so that each value stands for the 10 seconds before it is sent, and dBFS follows how loud a room sounds more closely than the amplitude does. The two loudness values are each store's tuning, given to the edge device as settings that are not secrets (environment variables): the quiet one is below the loudest, and the loudest is at most 0 dBFS. They start at −50 dBFS and −20 dBFS until the store sets its own. An interval in which the microphone could not be read is not sent, so BarPay soon has no liveliness signal instead of a wrong one; a value that did not reach BarPay is not sent again, since a late value would stand for a room that has changed, and the next interval's value is sent instead. Staff events come from the store app's admin screen through the admin API, each with a label and a phase (starts or ends). An evaluation uses the events going on: the labels whose last event, in the order BarPay recorded them, is starts. An ends with the same label, compared exactly, takes it out, so a live show that has ended no longer raises the rate; an event whose ends was never sent stays in the decision log's inputs, where staff can see it.
- **Goal**: the owner sets where the store sits between "fill the room" and "maximize revenue". The agent states how each decision serves that goal.
- **Evaluation on request**: the rate is re-evaluated only when the store app asks. BarPay runs at most one evaluation at a time and at most one per minimum interval (config file): a request that arrives while an evaluation is running gets that evaluation's result, and a request sooner than the minimum interval after the last one returns the current rate without a new evaluation. The interval is counted from when the last evaluation started, as BarPay stored it, so it holds across a restart; an evaluation that fell back counts, so a slow local LLM is not asked again on every request. A request exactly the minimum interval after that start is evaluated.
- **First rate**: when no rate is published in the store's currency (the first start, and the start that applies a currency change), BarPay publishes the starting rate from the config file at start, with a change of 0, so check-in works before the first evaluation. Every other start keeps the rate last published: going back to the starting rate would move the rate without an evaluation, beyond the max step. The rate history holds only the store's current currency; rates from before a currency change are not part of it.
- **Guardrails** (settings): floor, ceiling, and maximum change per evaluation (the max step: an amount of rate per hour, in the config file). A proposal is clamped into the floor and ceiling and to within the max step of the published rate, then rounded to the nearest multiple of the currency's rate step (fixed: 10 JPY, 0.05 USD), a tie going toward the published rate. The floor, ceiling, starting rate and max step are multiples of the rate step, so rounding never leaves the range or goes beyond the max step. Were the published rate ever further outside the range than the max step, the range would win. When the owner changes the floor or ceiling and the published rate falls outside the new range, the rate moves to the nearer bound at once, even beyond the max step, and the decision log records it with the staff member and an explanation BarPay writes from the change (no AI takes part). A change that leaves the published rate inside the new range publishes no rate and records nothing.
- **Explanation**: each published rate carries a short, plain-English reason that states the change actually published, after clamping (e.g. "Room is half empty — rate down $0.50 to $11.50/h."). Only the reason comes from the AI; BarPay writes the direction, the amount and the new rate from the rate it published, because a number the AI writes can be clamped away. When the guardrails leave the rate where it was, the explanation says so (e.g. "Steady room — rate unchanged at $12.00/h."). A fallback's explanation says why the rate was kept. It is readable through the admin API only; the customer sees the rate, its change and its chart.
- **Decision log**: every evaluation's inputs, the agent's proposal and reasoning, and the published rate, and every floor or ceiling change that moved the rate, are kept and readable through the admin API.
- **Fallback**: if the AI does not answer in time, or its answer cannot be read as a rate, the rate stays at its last value and the log says so. "In time" means within 20 seconds of BarPay sending the request: an answer that has not arrived 20 seconds after it was sent is a timeout. The limit is fixed so that the fallback comes before `requestRateEvaluation`'s own 30-second limit cuts off the store app's call. An AI that cannot be reached has not answered; one that answers with an HTTP error or with something other than a chat completion has answered with something that cannot be read as a rate.
- **Prompt**: BarPay asks the local LLM through its OpenAI-compatible chat API (`/chat/completions`, which llama.cpp's server, Ollama, vLLM and LM Studio serve), with temperature 0, and with `reasoning_effort` (how much the model may reason before it answers, as the server spells it) only when the config file sets one: a model that thinks before answering may need its thinking turned off to answer within the AI's time limit, and a server or model that does not know the field gets none. A fixed system message states the role, what each input means and the answer format. The user message is JSON: the currency and its smallest unit, the published rate, floor, ceiling, max step and rate step, the goal, occupancy, liveliness (`null` when there is none) and the staff events going on, whose labels go in as JSON strings to weigh, not to follow. The rate and its limits are hints only: the guardrails apply whatever the AI answers. The answer must be one JSON object with exactly three fields and nothing else, apart from whitespace around it and one Markdown code fence (```` ``` ```` or ```` ```json ````) that may enclose it: `rate_per_hour`, a whole number of the currency's smallest unit per hour from 0 to 100,000,000; `reason`, for the explanation, 1 to 120 characters once whitespace around it is removed, not only spaces and dots, without line breaks, other control characters, line or paragraph separators, or format characters such as bidirectional overrides; and `reasoning`, how the rate serves the goal, for the decision log, 1 to 4,000 characters, not only whitespace. Characters are Unicode code points. The prompt also asks for `reason` to be one plain-English sentence about the room, without numbers or the direction of the change; that is guidance to the AI, and only the limits above decide whether the answer can be read. Anything else cannot be read as a rate: prose, a missing, extra or repeated field, a rate written with a decimal point, an exponent or a sign, as a string, or outside that range. A rate inside that range but outside the guardrails is a rate, and the guardrails clamp it.
- **Blocks**: a block's amount is the rate × block length, rounded down to the currency's smallest unit (1 JPY, 0.01 USD), and is fixed when the block ends; it never changes afterwards. A rate published exactly when a block ends is that block's rate. Check-in itself costs nothing, and a block that has not ended when checkout begins (or the register is requested) is not charged; a block ending exactly then has ended and is charged.
- **Pause for additional orders**: the first order of a visit does not count (one-drink rule). Each later order, counted by order and including food, pauses the cover charge for a set time (settings, default 30 minutes, 0 turns it off) from that order; a new order restarts the pause rather than adding to it. A block that ends during a pause costs 0. A pause includes the instant of its order and ends just before the pause time has passed: a block ending exactly when an order is placed costs 0, and a block ending exactly when a pause ends is charged.
- **Cap per visit** (settings, default ¥3,000 / $30, 0 for none): a limit on the cover charge only; orders do not count toward it. The block that reaches the cap is charged only up to the cap, and later blocks cost 0.
- **Settings changes**: settings are changed through the admin API. Block length, pause and cap apply to active sessions from their next block: each block uses the settings in effect when it starts, so a change made exactly when a block ends applies to the block starting then, not to the one ending. A cap lowered below what a session has already accrued stops further blocks; nothing already accrued is reduced.
- **Time**: every duration in this document is real time.

## 5. Payment model

Linking PayPal is optional and moves no money: it saves a member's PayPal for this store, and the whole visit is charged once, at checkout.

| Moment | What happens in PayPal |
|---|---|
| Check-in | For a member with a saved PayPal, BarPay checks that PayPal still has it; nothing else |
| Link PayPal (members only, optional) | The member approves saving their PayPal for merchant-initiated charges; once the store app confirms the link, BarPay keeps it under the member ID. No payment is made |
| Stay | Nothing. Orders and cover charge are recorded by BarPay |
| Checkout, total = 0 | Nothing is charged |
| Checkout, total > 0 | The total is charged once with the saved PayPal, without the customer's approval; the gate unlocks only after it succeeds |
| After checkout | Nothing. The member's saved PayPal is kept for later visits until they delete it (UC9) |

- The **running total** = orders + cover charge so far.
- A guest, a member without a saved PayPal, or anyone who chooses the register pays at the register. BarPay fixes the total, staff take the money outside BarPay and record it through the store app's admin screen, and the session is closed as settled outside BarPay.

## 6. Guarantees

| Area | BarPay guarantees |
|---|---|
| Consent | A saved PayPal is kept only for a link request that named the version of the consent text the store app showed, which BarPay records with the saved PayPal; that text says the visit's total is charged at checkout without further approval. It is charged only at checkout. The running total is readable throughout |
| Transparency | While a session is open, BarPay provides through lookup the current rate, its change and history, the cover charge so far, whether it is paused or capped, and the running total. Showing them is the store app's job. The pricing rules before check-in are posted by the store (see [Out of scope](#7-out-of-scope)) |
| Charges only for this visit | BarPay charges a member's saved PayPal only during the checkout of that member's session or a staff retry of it, and staff can retry for at most 3 days after checkout began. A saved PayPal belongs to one member: one PayPal account is linked to at most one member. It is deleted when PayPal no longer has it (UC1, UC5), and when the member asks, though not while a session that uses it is not yet closed |
| Accuracy | When a session is closed through BarPay checkout, the amount charged equals the final total the server fixed at the moment of checkout, and the receipt shows that total with its PayPal capture ID. When it is settled at the register or outside BarPay, the receipt shows the total and the amount staff recorded; nothing was charged through BarPay, except a charge BarPay could not confirm, whose amount staff record from the PayPal dashboard (UC7) |
| Price bounds | The rate is always within the store's floor and ceiling. It never moves more than the max step per evaluation, and evaluations are at least the minimum interval apart, except when the owner changes the floor or ceiling, which the decision log records |
| Single payment | Each session is settled once. A repeated scan, a retry after a network error or timeout, a repeated admin request, or a staff retry never charges the same amount twice. Admin requests that can start a charge carry an idempotency key, and move the session to checking out before PayPal is called, so a concurrent one is rejected. BarPay sends a charge whose outcome is unknown again with the same `PayPal-Request-Id` and `invoice_id`, never as a new attempt of its own (Idempotency in [Fixed constraints](#8-fixed-constraints)), and each session's charge can succeed in PayPal only once |
| Unlock only on payment | BarPay unlocks the exit gate only for a closed session that was fully charged, or whose payment staff recorded at the register or outside BarPay, and only within the re-unlock window after it closed. Once a session is closed its exit token stops changing. Staff can also open the gate by hand through the admin API |
| Store notified | The store app is notified of every session start, PayPal link, register request and result. Notifications are signed, carry an event ID and a sequence number, and are retried for up to 24 hours, so the store app may receive one more than once or out of order; it can always read the current state by lookup |
| Access | Only the store API key can start check-ins, report orders, read a session by its ID, members and the rate, ask for rate evaluations, request the register, get exit tokens, request and confirm PayPal links and delete a member's saved PayPal. Only the admin API key can list sessions, read the decision log, alerts, notification log and settings, change settings, send staff events, and record, close, retry, settle, mark unpaid or open the gate by hand; each admin request names the staff member acting. A link is saved only for the member it was requested for, confirmed by the store app's server. Checkout is triggered only by an authenticated edge device presenting a current exit token, by an admin request, or by BarPay itself at the maximum stay. An exit token is issued only to the store API key, works only at the exit gate, and is never written to logs. The two keys protect admin actions only as far as the store app keeps the admin key apart; a store backend that holds both can do everything, and the staff member it names is attribution, not authentication |
| Durability | Sessions, orders, settings, the decision log, capture IDs, saved-PayPal references, alerts, the notification log (including notifications not yet delivered), the store app's `Idempotency-Key` records, each PayPal call's `PayPal-Request-Id` and `invoice_id` with when its attempt was first sent, the history of published rates, and the staff member recorded with each admin request survive a backend restart |
| Privacy | Raw audio is never stored or sent off the edge device |
| Observability | Failed charges, charges whose outcome BarPay could not confirm, failed unlocks, register requests, rejected late orders, sessions past the maximum stay, notifications given up and failed deletions are raised as alerts readable through the admin API; failed charges, and a failed unlock after a scan (the session paid or settled), are also sent to the store app |

## 7. Out of scope

| Not doing | Why |
|---|---|
| Real-money payments | Charging saved PayPal in production needs PayPal's approval for reference transactions (see [Fixed constraints](#8-fixed-constraints)); everything is built and run in the PayPal sandbox |
| Screens | The store app draws every screen: the panel, the admin screen, and the consent before linking. BarPay provides the data and the operations |
| Authenticating staff | The store app's job. BarPay checks the admin API key and records the staff member the store app names |
| Deciding when to evaluate the rate | The store app's schedule. BarPay evaluates when asked, within the minimum interval |
| Store onboarding screens, more than one store per deployment | One store is set up with a config file; prices and cover-charge rules are changed through the admin API |
| BarPay as a payment intermediary (platform fees, multiparty) | Each store is paid into its own PayPal account |
| Using a saved PayPal across a chain's stores | Future: may work when the stores share one PayPal account; not verified |
| Members and login | The store app's job. BarPay only keeps a member's saved PayPal under the ID the store app gives it (UC2, UC9) |
| PayPal for guests | A saved PayPal needs an owner across visits, so linking needs a store account; guests pay at the register |
| Menu, order screen, kitchen workflow | The store app's job; BarPay records order lines and amounts only |
| Showing the pricing rules before check-in | The store posts them (variable rate, block length, cap per visit) on its website or in the store |
| Taking money at the register | The store's job; BarPay fixes the total and records what staff say they received |
| Cancelling or correcting an order line | Once reported, an order stays in the total; the store refunds through the PayPal dashboard |
| Correctness of what the store app reports | BarPay charges the order amounts the store app reports and trusts the member ID and staff member it sends; checking them, limiting how much a visit may order, and showing each exit token only to its session's customer, is the store app's job |
| Limiting check-ins | The store app decides who may check in, including rate-limiting guest joins |
| Showing the loaded config file | The owner reads the file; the settings in effect are readable through the admin API |
| Notifications after 24 hours of retries | The store app recovers through lookup |
| Collecting a refused charge from a customer who has left | It stays as an unpaid amount readable through the admin API; collecting it is the store's job outside BarPay |
| Splitting a bill within a group | Each phone is its own session; splitting is a separate product |
| Locking in a price at entry | Would undo the stock-like model; the customer is protected by the cap per visit and the running total instead |
| A deposit or holding funds at check-in | The gate opens only after payment, so a customer whose charge fails is still inside and pays at the register; a hold would add a second charge and a pending amount on the customer's statement |
| Refunds and disputes | Handled in the PayPal dashboard |
| Tax calculation | All prices are tax-inclusive |
| Japanese text from BarPay | Explanations and error messages are in English; the language of the screens is the store app's choice. Currency (JPY/USD) is selectable |
| Locking customers in | The door must always open from inside; the lock unlocks an exit gate as part of the walk-through experience, not as a way to stop customers from leaving |

## 8. Fixed constraints

- **Verified in the sandbox (2026-10-28, USD)**: a `CAPTURE` order with only `payment_source.paypal.vault_id` was `COMPLETED` in the create response, without buyer approval. The saved PayPal used then came from an order approval, not from the setup-token flow BarPay now uses, which the next item covers.
- **Verified in the sandbox (2026-10-31, USD)**: setup tokens with `usage_type` `MERCHANT`, every call with a `PayPal-Request-Id`. Some rows used the saved PayPal from 2026-10-28, which saving returned (third row).

  | Call | Observed |
  |---|---|
  | Save, then charge | A setup token the buyer approved saved a PayPal (201); a `CAPTURE` order with its `vault_id` was `COMPLETED` (201) with one capture, without buyer approval |
  | Save 40 minutes after the setup token was created (link URL 30 minutes + code 10 minutes) | Saved |
  | Save a setup token approved while the buyer already had a saved PayPal with the store | Returned that saved PayPal's `vault_id` (200), not a new one, also for another setup token approved separately. Once that saved PayPal was deleted, saving such a setup token failed with 422 `INTERNAL_ERROR`, also with a new `PayPal-Request-Id`. Setup tokens approved while none existed gave separate `vault_id`s |
  | Look up by ID | 200 while saved, also after a refused charge. After BarPay deleted it, and after the buyer cancelled it in their PayPal account: 404 `TOKEN_NOT_FOUND`, and a charge with it 403 `PERMISSION_DENIED` |
  | Delete | 204; deleting again, with the same or a new `PayPal-Request-Id`, also 204. Deleting one the buyer had already cancelled in their PayPal account: 204 |
  | Same `PayPal-Request-Id` again | Save: 200 with the same `vault_id`, also about 91 minutes later for a save that had created a new one; a new `PayPal-Request-Id` for an already saved setup token also gave the same `vault_id`. Charge: the same order and capture, no second charge (200 shortly after; 201 about 39 minutes later, the longest tried; how long PayPal remembers it is not known). Delete: 204. A refused charge is not remembered: a repeat is processed again |
  | Refused charge | 422 `TRANSACTION_REFUSED`, no order ID returned. Caused by removing the sandbox buyer's linked bank account and card (balance 0). The sandbox's mock response header gives 422 `INSTRUMENT_DECLINED` instead and does not use up the `invoice_id` |
  | Same `invoice_id` with a new `PayPal-Request-Id` | After a successful charge: 422 `DUPLICATE_INVOICE_ID`. After a refused charge: refused again while the buyer had no funds, `COMPLETED` once they had (a staff retry) |
- **Verify before trusting**: BarPay's return and cancel URLs carry an unguessable link ID. When the customer returns from PayPal, the backend uses only the setup token stored with that link, ignores any token ID added to the URL, accepts a return once per link, and checks that the setup token was approved. It creates the saved PayPal from that setup token only when the store app confirms the link, and saves it to the member only after that succeeds.
- **Idempotency**: every PayPal call that saves PayPal, moves money or deletes a saved PayPal carries a `PayPal-Request-Id` derived from the session, the step and the attempt, stored before the call. Linking and deleting have no session, so a link's calls derive it from the link's ID and a deletion's from the deletion's ID instead. A network retry reuses it; a staff retry starts a new attempt, and so does a deletion's next try after PayPal refused the previous one. The checkout charge carries an `invoice_id` per session, also stored before the call: PayPal rejects a charge whose `invoice_id` was already charged as `DUPLICATE_INVOICE_ID`, while a refused charge does not use it up, so a staff retry keeps it (verified above). The `invoice_id` is unique across the store's whole PayPal account, because one that another charge on the account already used gives a `DUPLICATE_INVOICE_ID` that no staff retry can clear. A call's outcome is unknown when PayPal gives no answer within the call's time limit, the connection drops, BarPay restarts before the answer was stored, or the answer does not say: a 5xx, 408 or 429, a 2xx that cannot be read, or, for a charge, a capture neither completed nor declined (such as pending). Only an order completed with a completed capture is a successful charge; another 4xx, or a declined or failed capture, is a refusal. When a call's outcome is unknown, BarPay sends the same call again with the same `PayPal-Request-Id` instead of looking it up, because a charge whose answer was lost leaves BarPay no order ID to look up, and a refused one has none. A repeated save or delete returns the first result. A repeated charge returns the first order and capture if that charge succeeded, with no second charge; if it was refused or never reached PayPal, it is charged now, which is the same charge still owed, and the `invoice_id` lets it succeed only once. PayPal was seen to remember a charge's `PayPal-Request-Id` for about 39 minutes, longer not known, so BarPay sends a charge again only within 30 minutes of that attempt's first send; from exactly 30 minutes on it does not. If the outcome is still unknown after that, or a repeat is rejected as `DUPLICATE_INVOICE_ID` while BarPay has no capture recorded for the session, BarPay does not charge again: the session becomes PaymentFailed with a `charge_unconfirmed` alert, and staff settle it from the PayPal dashboard (UC7). The charge may then have gone through without BarPay holding its capture ID, which the receipt needs and a later repeat is inferred, not verified, not to return, so only staff, seeing PayPal's record, can tell whether the customer paid.
- **The store's own PayPal**: BarPay calls PayPal with the store's REST credentials and keeps them as secrets. The REST app those credentials belong to must not hold saved PayPals used outside BarPay: saving can return a saved PayPal the buyer's account already has with the store, BarPay saves one that no member holds to the linking member (UC2), and that member's deletion (UC9) deletes it in PayPal.
- **Approval for saved PayPal**: in production, saving PayPal for merchant-initiated charges needs PayPal's approval for reference transactions. It does not block the sandbox.
- **Currency**: each store picks JPY or USD; all amounts in that store use it. JPY has no decimal places. JPY in the sandbox depends on [Open question 3](#12-open-questions).
- **Clock**: BarPay assumes its server's clock never steps back; keep it synchronized by adjusting it gradually (slewing), never by stepping it. Every amount and time limit is computed from the instants BarPay stored (check-in, orders, published rates, settings changes), so a clock set back could give a rate or an order a time before a block that has already ended and change that block's amount, which the [Pricing model](#4-pricing-model), Blocks forbids.
- **Hardware**: Raspberry Pi 5, a USB QR reader, a lock that opens only while powered, driven by a GPIO line through a relay, a microphone, and an LED and a buzzer at the gate.
  - **QR reader**: it reads QR codes, also from a phone's screen; it sends what it reads as a USB HID keyboard with the US layout; and it ends each read with Enter. No model is named, since models get replaced. The edge device depends on these: an exit token is a string whose contents are not defined (`ExitToken` in `openapi.yaml`), so the edge device cannot depend on which characters come, and a layout or Shift handling that does not match would misread it.
  - **Lock**: the lock is locked while it has no power and open only while it has power, which it gets through a relay's contacts; it must always open from inside, even without power ([Locking customers in](#7-out-of-scope)). The edge device drives the relay through one GPIO line, so the lock is open only while that line is high: an edge device that stops normally, a Raspberry Pi 5 that loses power or a cut wire leaves the gate locked. An edge device that crashes or is killed may leave the line high until the step its service runs after every stop sets it low, or until it starts again, which also sets it low. An unlock sets the line high for the unlock time and then low again; the unlock time is a setting in an environment variable, not a secret, 5 seconds by default, more than 0 and at most 60 seconds, and it is the longest the gate stays open for one unlock. The edge device only unlocks; there is no lock command, and only the line going low locks the gate, so no forgotten lock command can leave the gate open. An unlock while the gate is open keeps the line high and counts the unlock time again from that unlock. From exactly the end of the unlock time, an earlier unlock no longer holds the line high: an unlock that does not work at that instant sets it low, and one that works counts the unlock time from then. An unlock worked when the edge device set the line high and read it back as high; the relay and the lock have no line that reports their state, so that is as far as the edge device can check, and it reports that with `reportUnlockResult`. When an unlock did not work, the edge device sets the line low at once, unless an earlier unlock that worked is still within its unlock time, which is then not counted again. The edge device sets the line low when it starts and when it stops. Which line it is, is a setting in an environment variable, not a secret; the edge device does not start when it is one of the indicator's lines.
  - **One edge device**: one BarPay connects to one edge device. `waitForUnlock` hands an unlock by hand to one of the waiting calls and never sends it again, so with two, a different gate could open; and two microphones would overwrite each other's liveliness.
  - **Indicator**: how the edge device shows the customer the state of the gate, through the LED and the buzzer as in the table below. Where this document or the edge device's API says the reader shows a message, the edge device shows that state through the indicator. The QR reader's own beep means only that it read a code; the result is shown through the indicator only. Every buzzer sound is at least 0.5 seconds long, with 0.3 seconds of silence between sounds, so it is not taken for the reader's short beep.

    | State | When | LED color | LED pattern | Buzzer | Then |
    |---|---|---|---|---|---|
    | Waiting for a scan | Nothing else is shown | White | Steady | None | Stays until the next state |
    | Working (waiting for BarPay or the lock) | From a scan until BarPay answers, and while the lock unlocks (after `unlock` `true` or by hand) | Blue | Blinks 0.2 s on, 0.2 s off | None | The state for the answer, or for how the unlock went |
    | Thank you | `reader_message` `Thank you`, once the lock has unlocked | Green | Steady for 3 s | One 0.5 s sound | Waiting for a scan |
    | Please pay at the register | `reader_message` `Please pay at the register` | Amber | Steady for 5 s | Three 0.5 s sounds | Waiting for a scan |
    | Please see staff | `reader_message` `Please see staff` | Red | Steady for 10 s | One 1.5 s sound | Waiting for a scan |
    | Please show a new QR code | `reader_message` `Please show a new QR code` | Amber | Blinks 0.5 s on, 0.5 s off for 5 s | Two 0.5 s sounds | Waiting for a scan |
    | BarPay unreachable | A scan's call to BarPay gets no answer, or an answer that is not a `ScanResult` | Red | Blinks 0.5 s on, 0.5 s off for 10 s | Two 1.5 s sounds | Waiting for a scan |
    | Unlock failed | An unlock (after a scan or by hand) did not work, as judged in Lock above | Shown as Please see staff | Shown as Please see staff | Shown as Please see staff | Waiting for a scan |
    | Unlocked by hand | An unlock from `waitForUnlock`, once the lock has unlocked | Green | Blinks 0.5 s on, 0.5 s off for 3 s | One 0.5 s sound | Waiting for a scan |

    Any two states other than Unlock failed, which is Please see staff on purpose, differ in the LED pattern or the buzzer, not only in color, so a customer who cannot tell the colors apart can still tell them apart, and every result other than Unlocked by hand can be told apart by sound alone. Unlocked by hand shares its sound with Thank you because both mean the gate is open; staff are at the gate then.

    A new state replaces the one being shown at once, stopping its light and sound even in the middle of a sound, so the customer is never shown the last state late. When the edge device stops, it turns the LED and the buzzer off, so no state is left showing as if it still ran. The LED is one RGB LED on three GPIO lines, amber being red and green on together, and the buzzer is an active buzzer on one line, so the edge device only turns each line on or off; sounds differ in number and length, not in pitch. Each color of the LED and the buzzer is on while its line is high, so the LED must be common-cathode and the buzzer must sound while its line is high. Which lines they are is a setting in environment variables, not a secret.
- **AI**: the pricing agent runs on a local LLM that the BarPay backend reaches over the network; there is no per-call cost.
- **Secrets**: PayPal credentials, the store API key, the admin API key, the notification signing secret and the edge device's shared secret never enter the public repository.
- **Working without hardware**: every hardware step has a development tool that stands in for it (virtual exit gate, signal simulator) and uses the edge device's API. The tools hold neither the store API key nor the admin API key, and are not part of a production build. Their only credential is the edge device's shared secret, which they take from an environment variable; they never read BarPay's config file, which holds the other keys. The virtual exit gate sends the text of an exit QR that a person gives it, as the reader would read it, and never fetches an exit token itself: exit tokens are issued only to the store API key.
- **Language**: text BarPay returns, the sample app's UI and the README are in English. Every non-exempt `.md` in the repo has a `.ja.md` translation (see `.claude/skills/bilingual-docs`).

## 9. Use cases

```mermaid
stateDiagram-v2
  [*] --> Active: checked in (guest or member)
  Active --> Active: order / rate evaluation / PayPal linked
  Active --> CheckingOut: exit QR scanned / staff closes / max stay (PayPal linked)
  Active --> AtRegister: customer chooses "Pay at register"
  Active --> Closed: staff record a register payment
  AtRegister --> Closed: staff record a register payment
  AtRegister --> ClosedUnpaid: staff mark it unpaid / 3 days after the request
  CheckingOut --> Closed: full total charged (gate unlocks if at the gate)
  CheckingOut --> PaymentFailed: the charge failed or could not be confirmed
  PaymentFailed --> CheckingOut: staff retries
  PaymentFailed --> Closed: staff settles outside BarPay
  PaymentFailed --> ClosedUnpaid: 3 days after checkout began
  ClosedUnpaid --> [*]
  Closed --> [*]
```

Read: a session only ends through a successful charge, a register payment or settlement that staff record, or being closed as unpaid by staff or at a 3-day limit; neither a payment failure nor a customer's register request closes it. Sessions are never removed.

Staff act through the store app's admin screen, which calls BarPay's admin API.

### UC1 — Customer checks in

- **Precondition**: the BarPay backend is running (a rate is published from start).
- **Main flow**: the customer scans the store's entrance QR code or taps its NFC tag, which opens the store app. They log in or join as a guest; the store app asks BarPay for a check-in and the session starts. The store app is notified and shows its menu and its panel.
- **Alternative flow**: a member with a saved PayPal checks in → the session starts at once; BarPay then checks with PayPal that the saved PayPal still exists and links the session. The member does nothing. The check does not tell whether a charge will go through: PayPal still has a saved PayPal whose buyer has nothing left to pay with. A charge refused at checkout is handled as in UC5 and keeps the saved PayPal, since staff retry with it (UC7).
- **Exception**: PayPal no longer has the member's saved PayPal (the customer cancelled it in their PayPal account) → the session stays unlinked; BarPay deletes the saved PayPal and notifies the store app (`member.saved_paypal_deleted`); the member can link again (UC2) or pay at the register. BarPay treats a saved PayPal as gone only when PayPal says so: not found when it is checked, or permission denied when it is charged (UC5).
- **Exception**: the check times out or its answer cannot be read → the saved PayPal is kept, since PayPal has not said it is gone. BarPay checks again 1 minute after the previous check ended, also across a restart, until PayPal answers; if PayPal answers that it still has it while the session is still active, the session is linked then. Until then the session is unlinked, and a session that is no longer active is not linked.
- **Exception**: the store API key is missing or wrong → rejected; nothing is created.
- **Exception**: the store app starts the same check-in twice (same idempotency key) → the same session is returned. A member who already has an open session → rejected.
- **Postcondition**: the session is active, linked only if PayPal still has the member's saved PayPal, with no cover charge yet; its first block ends one block length after check-in; the store's occupancy count includes it.

### UC2 — Member links PayPal

- **Precondition**: the customer is logged in to the store app as a member and has no saved PayPal.
- **Main flow**: in the store app's settings, the member chooses to link PayPal and agrees to the consent text that the visit's total is charged at checkout without asking again. The store app asks BarPay for a link URL for that member, naming the consent version, and opens it as a full page. BarPay redirects the member to PayPal, where they approve saving PayPal for this store. PayPal returns the member to BarPay, which checks the approval and redirects them to the store app with a one-time code. The store app's server confirms the link with the code and the logged-in member's ID; BarPay saves PayPal under that member ID. If the member has an active session, it becomes linked at once; otherwise the next check-in uses it (UC1). The store app is notified.
- **Exception**: the customer is a guest → the store app does not offer linking and says that it needs an account; BarPay rejects a link request without a member ID.
- **Exception**: the member already has a saved PayPal, or its deletion is still pending → rejected.
- **Exception**: the store app asks to return the member to a URL outside its origin (config file) → rejected; no link URL is created.
- **Exception**: the member opens the link URL after it stopped working → nothing is saved; the member is returned to the store app and can start again.
- **Exception**: the member cancels in PayPal → nothing is saved; the member is returned to the store app and can try again later or pay at the register.
- **Exception**: the setup token was not approved → nothing is saved; the member is returned to the store app with the failure and can try again.
- **Exception**: the store app confirms with a code that is unknown, already used or expired, or with a member ID other than the one the link URL was made for → rejected; nothing is saved, and the code cannot be used again.
- **Exception**: the return reaches a browser session other than the one that asked for the link (`state` does not match) → the store app does not confirm; nothing is saved.
- **Exception**: the member closes the tab after approving, or the store app never confirms → nothing is saved; linking again starts over.
- **Exception**: saving PayPal fails at confirmation → nothing is saved; the store app shows an error and the member can try again. This includes the case where the member's PayPal account already had a saved PayPal with the store when they approved, and that saved PayPal was deleted before the store app confirmed: PayPal refuses to save, and linking again from the start saves a new one. If PayPal's answer is still unknown after BarPay's resends within the confirmation, it is handled as this failure; a saved PayPal that PayPal may have made then belongs to no member, and linking again with the same PayPal account saves it to the member (see The store's own PayPal in [Fixed constraints](#8-fixed-constraints)). The same happens in the rare case where two links of one member are confirmed at once with different PayPal accounts: the saved PayPal left over belongs to no member, so it is never charged, and a later link of that PayPal account takes it.
- **Exception**: saving returns a saved PayPal that BarPay already holds for another member (PayPal returns the existing saved PayPal when the PayPal account already has one with the store) → rejected; nothing is saved for this member, and the other member's saved PayPal is left as it is in PayPal; the member can link another PayPal account or pay at the register. One PayPal account is linked to at most one member, because sharing it would charge one member's visits to the other's PayPal, and one member's deletion (UC9) would remove the other's. When BarPay sends its save again after an unknown outcome and gets back the saved PayPal it is saving for this member, that is success. A saved PayPal that BarPay holds for no member is saved to this member (see The store's own PayPal in [Fixed constraints](#8-fixed-constraints)).
- **Exception**: the member's session is no longer active (at the register, checking out, closed) → the saved PayPal is kept for the next visit; that session is not linked.
- **Postcondition**: the member has a saved PayPal with the consent version recorded; an active session of theirs is linked; no money has moved.

### UC3 — Customer stays and watches the price

- **Precondition**: the session is active.
- **Main flow**: the store app's panel shows, from BarPay's lookup, the rate as a stepped ticker chart over the visit, its latest change, time stayed, the cover charge so far, the orders' total, the payment method and the running total. While a pause or the cap applies, the panel says so.
- **Exception**: the session reaches the maximum stay (config file, default 12 hours) → if PayPal is linked, it is checked out as in UC5 without the gate, and the customer can check in again; if not, it stays active and BarPay raises an alert, once per session, so staff can speak to the customer. A linked session's total is fixed at the maximum stay itself, also when BarPay was stopped then and checks it out only when it starts again, so the amount does not depend on how long BarPay was down.
- **Postcondition**: the cover charge equals the sum of the ended blocks' amounts since check-in, with blocks in a pause or after the cap counted as 0.

### UC4 — Store app reports an order

- **Precondition**: the session is active.
- **Main flow**: the customer orders in the store app; the store app sends BarPay the order lines and amounts. BarPay adds them to the session. From the second order of the visit, the cover charge pauses as in the [Pricing model](#4-pricing-model).
- **Exception**: the session is not active (at the register, checking out, payment failed, closed, closed unpaid) → rejected and raised as an alert; anything already served is collected by the store outside BarPay.
- **Exception**: no session has the ID → rejected; no alert is raised, since there is no customer for staff to see to, and requests with made-up IDs cannot fill the alerts.
- **Exception**: the same order is reported twice (same order ID) → recorded once, and the pause is not restarted; this holds even after the session is no longer active.
- **Exception**: an order ID already recorded comes with different lines → rejected; the recorded order stays as it was.
- **Exception**: an amount is negative or in another currency → rejected.
- **Exception**: with the order, the running total would be above 9,000,000,000,000,000 in the currency's smallest unit, which leaves room under the largest total BarPay holds (the API spec's `Total`) for the cover charge that accrues afterwards → rejected; nothing is recorded.
- **Postcondition**: the running total includes the order.

### UC5 — Customer checks out at the exit gate

- **Precondition**: the session is active and PayPal is linked.
- **Main flow**: the customer taps Pay in the store app's panel and chooses PayPal. The store app gets the session's exit token from BarPay and shows it as a QR, getting a new one each time it changes (config file, default every 30 seconds), so an old screenshot does not work. The customer shows the QR to the reader at the exit. The server fixes the final total at that moment and stops the cover charge, charges the total once with the saved PayPal as in the [Payment model](#5-payment-model), and unlocks the gate; the reader shows "Thank you" once the lock has unlocked. The store app is notified with the receipt and shows the result. In this use case, the reader shows a message when the edge device shows that state through its indicator, and the gate has unlocked when the unlock worked as judged in Lock (Hardware in [Fixed constraints](#8-fixed-constraints)).
- **Alternative flow**: the same exit QR is scanned again within the re-unlock window (config file, default 2 minutes) after success → the gate unlocks again; nothing is charged.
- **Exception**: the session has no PayPal linked → no exit token is issued, and a scanned token for it is rejected; the reader shows "Please pay at the register"; nothing is charged.
- **Exception**: the charge fails or is refused → the gate stays locked, the reader shows "Please see staff", the session becomes PaymentFailed with the unpaid amount, an alert is raised, and the store app is notified (`session.payment_failed`, reason `refused`). The saved PayPal is kept for the staff retry (UC7), unless PayPal answers that it no longer has it (permission denied): then BarPay deletes it as in UC1's exception, and staff settle outside BarPay.
- **Exception**: the charge's outcome is unknown (a timeout, no readable answer, a restart) → the gate stays locked, and from the first timeout the reader shows "Please see staff", so the customer does not wait at the gate for an answer that may take minutes. BarPay sends the same charge again as in Idempotency in [Fixed constraints](#8-fixed-constraints), each time 1 minute after the previous send ended, until PayPal answers. Resends start only when PayPal is slow or unreachable, so sending more often would add load for little gain while the customer is already with staff, and a minute apart still gives up to about 30 tries in the window. An admin request that started the charge (UC7's close or retry) gets the session in checking out at once, and the result comes later by notification (`session.closed` or `session.payment_failed`); a repeat with the same idempotency key gets that same response and neither waits nor charges again. Success closes the session as in the main flow but does not unlock the gate, since the customer may have walked away from it: within the re-unlock window the store app gets a new exit token and shows it as a QR, which the customer shows at the gate, or staff open the gate by hand. A refusal is handled as above. If there is still no answer 30 minutes after the first send, or PayPal says this session's charge already succeeded while BarPay has no capture recorded, nothing is charged again; the session becomes PaymentFailed with a `charge_unconfirmed` alert, the store app is notified (`session.payment_failed`, reason `unconfirmed`), and staff check the PayPal dashboard (UC7).
- **Exception**: the charge succeeds but unlocking fails → the payment stands, and the reader shows "Please see staff"; an alert is raised and the store app is notified so staff can open the gate by hand. The edge device reports whether each unlock worked; a failed unlock by hand (UC7) raises the alert only, since staff are at the gate.
- **Exception**: the exit token is unknown or expired, belongs to a session closed longer ago than the re-unlock window, or belongs to a PaymentFailed session → rejected, nothing charged, the gate stays locked. The reader shows "Please show a new QR code" for a token that is unknown or no longer current, and "Please see staff" for the others.
- **Exception**: two scans of the same QR arrive at the same time → only one checkout runs; both get the same result.
- **Postcondition (on success)**: total charged = final total on the receipt; the session is closed; the cover charge has stopped; the member's saved PayPal is kept.

### UC6 — Customer pays at the register

- **Precondition**: the session is active, with or without PayPal linked.
- **Main flow**: the customer taps Pay in the store app's panel and chooses "Pay at register"; the store app asks BarPay for it. BarPay fixes the final total, stops the cover charge, sets the session's payment method to register, marks the session as waiting at the register, raises an alert and notifies the store app. At the register, staff take the money and record it on the store app's admin screen. The session is closed as settled outside BarPay, the store app is notified (`session.settled_outside` with the receipt), and within the re-unlock window the store app can get an exit token that opens the gate.
- **Alternative flow**: the customer goes straight to the register → staff record the payment from the session list; the total is fixed at that moment.
- **Alternative flow**: staff record an amount different from the total → the receipt shows both; BarPay charges nothing for the difference.
- **Exception**: the customer leaves after choosing the register without paying → staff mark the session unpaid, or it is closed as unpaid 3 days after the request. Either way the unpaid total is recorded and nothing is charged to PayPal. The limit falls at exactly 3 days after the request: from that instant staff can no longer record a payment or mark the session unpaid, and it is closed as unpaid; until just before it, they can.
- **Exception**: the customer wants to pay with PayPal after choosing the register → the request cannot be withdrawn; they pay at the register. The maximum stay no longer applies, because the cover charge has stopped.
- **Postcondition**: the session is closed; the receipt shows the total and the amount recorded at the register; nothing was charged through BarPay.

### UC7 — Staff run the room

- **Precondition**: the store app has authenticated the staff member on its admin screen and holds the admin API key.
- **Main flow**: the admin screen, designed by the store app, uses the admin API to show every session with check-in time, stay, number of orders, running total, payment method (PayPal or register), state and any pause or cap; the rate history with explanations and the decision log; alerts and the notification log; and the settings: floor and ceiling, block length, pause for additional orders, cap per visit and the pricing goal. Staff can send staff events (e.g. a live show), and the store app can ask for an evaluation at once (store API key).
- **Alternative flow**: a linked customer leaves through another door or has no battery → staff close the session, which checks out as in UC5 without the gate.
- **Alternative flow**: staff record a register payment as in UC6, or mark a session waiting at the register as unpaid when the customer left without paying.
- **Alternative flow**: staff open the gate by hand after a failed unlock. The edge device keeps a request open to BarPay to wait for such an unlock, and BarPay counts it as connected while that request is open; BarPay hands the unlock to it then, and with no edge device connected it refuses the request and sends nothing later, so a gate never opens long after staff asked.
- **Alternative flow**: staff change a setting → it applies as in the [Pricing model](#4-pricing-model). Floor and ceiling above 0 and multiples of the currency's rate step (see [Guardrails](#4-pricing-model)) with the floor below the ceiling, a block of at least 1 minute, and a pause or cap of 0 or more are required; otherwise the change is rejected.
- **Exception**: a session is PaymentFailed → staff retry, which charges the total again with the saved PayPal as a new attempt; while its outcome is unknown, the request gets the session in checking out at once and the result comes by notification, as in UC5; or staff mark it settled outside BarPay with the amount settled, which closes the session. A retry is rejected when the member no longer has a saved PayPal (UC5); staff settle outside BarPay.
- **Exception**: a session is PaymentFailed with a `charge_unconfirmed` alert (UC5) → its charge may have gone through. Staff check the PayPal dashboard and, if it went through, settle outside BarPay with the amount charged; if not, they retry. A retry cannot charge twice: if the first charge succeeded, PayPal rejects it as `DUPLICATE_INVOICE_ID` and the session stays PaymentFailed with a new `charge_unconfirmed` alert, and the store app is notified again (`session.payment_failed`, reason `unconfirmed`).
- **Exception**: a PaymentFailed session reaches 3 days after checkout began → retries stop, it is closed as unpaid with the unpaid amount recorded. As for the register (UC6), the limit falls at exactly 3 days after checkout began: from that instant staff can no longer retry or settle outside BarPay, and the session is closed as unpaid; until just before it, they can. A retry from that instant is refused as past the retry window, also once the session has been closed as unpaid. The limit does not apply while a retry is checking out, since its charge may still succeed: a retry still being sent again when the limit falls goes on, the session is closed as charged if it succeeds, and closed as unpaid at once if it fails or cannot be confirmed.
- **Exception**: the same close or retry request arrives twice (same idempotency key) → it runs once: a repeat after it finished gets the same result, including the stored `checking_out` response once the session is in checking out with its outcome unknown (UC5); one that arrives before that is rejected and can be sent again.
- **Postcondition**: every admin action is recorded with the staff member the store app named.

### UC8 — Store connects to BarPay

- **Main flow**: the owner sets, in a config file loaded at start: the store's PayPal REST credentials, the store API key, the admin API key, the store app's notification URL and signing secret, the store app's origin (the only place BarPay returns members to after linking), BarPay's own public URL (where the member's browser opens the link URL and PayPal returns it), the edge device's shared secret, currency (JPY/USD), the starting rate, max step, minimum evaluation interval, the local LLM's address and model, and optionally how much it may reason before answering, capacity, maximum stay, re-unlock window, exit token refresh interval, and the starting values of the settings.
- **Config file**: TOML, read from `barpay.toml` in the working directory unless BarPay is started with `-config <path>`; git ignores that name, and `barpay.example.toml` in the repository lists its keys with no secret values. It also sets the address BarPay listens on and its database file. Amounts are integers in the currency's smallest unit (cents for USD); durations, except the settings' block and pause in minutes, are written like `30s` or `12h`. The max step is an amount of rate per hour, like the starting rate (`max_step = 300` is $3.00 per hour), not a share of the current rate. A value whose default this document states may be left out; every other value is required.
- **Exception**: starting floor not below starting ceiling, starting rate outside them or not a multiple of the rate step, a max step below the rate step, not a multiple of it or above the largest amount, maximum stay of 3 days or more, starting settings that a change through the admin API would reject (UC7), missing credentials (PayPal, the store API key, the admin API key, the edge device's shared secret, the signing secret), two of the store API key, the admin API key and the edge device's shared secret being the same, or an unknown key in the file, a required value missing, or a value of the wrong form → BarPay does not start, and says why without showing any secret value.
- **Exception**: the notification URL or BarPay's public URL does not use https, other than http to a loopback IP address written as one (`127.0.0.0/8` or `::1`) → BarPay does not start, and says why. Plain http to a name such as `localhost` is refused. Notifications carry receipts and requests to the public URL carry the link ID, which is all a browser shows to be the member linking; the signature stops forgery and tampering but not reading in transit; loopback traffic never leaves the machine, which running BarPay and the store app on one machine needs, while a name can resolve elsewhere and an IP address can be judged when the file is read.
- **Exception**: the currency is JPY → BarPay does not start, and says JPY is not supported yet. JPY is planned in P1 (see [Priorities](#11-priorities)).
- **Exception**: currency changed while sessions are open → BarPay starts with the previous currency, keeping its published rate and the max step it ran with, raises an alert, and applies the change at the first restart with no open session.
- **Postcondition**: BarPay runs with the config file's values; a rate is published: the starting rate on the first start and on the start that applies a currency change, otherwise the rate last published (see [First rate](#4-pricing-model)); the settings hold the values last saved through the admin API, or the starting values on first start and on the start that applies a currency change (the previous currency's floor, ceiling and cap mean nothing in the new one).

### UC9 — Member deletes their saved PayPal

- **Precondition**: the member has a saved PayPal.
- **Main flow**: the member deletes it in the store app's settings; the store app asks BarPay, which deletes it in PayPal. The store app is notified.
- **Exception**: a session that uses it is not yet closed → rejected; the store app says it can be deleted after paying. A session uses the saved PayPal from when it was linked with it until it is closed or closed unpaid, also while it waits at the register.
- **Alternative flow**: PayPal does not confirm the deletion at once → BarPay accepts the request and retries until PayPal confirms, each time 1 minute after the previous try ended, also across a restart. Until then the saved PayPal shows as being deleted: the member cannot link again and a check-in starts unlinked. The store app is notified when the deletion is confirmed; a deletion that PayPal has still not confirmed 1 hour after the member asked is raised as an alert, once.
- **Postcondition**: the member has no saved PayPal; their next check-in starts unlinked, and linking again goes through PayPal approval (UC2).

## 10. System overview

```mermaid
flowchart LR
  Phone["Customer phone"] --> App["Sample mobile order app<br/>(store app: panel, admin screen)"]
  Phone -- "link URL: redirects only" --> API
  App -- "store API key / admin API key" --> API
  API -- "signed notifications" --> App
  API["BarPay backend (Go)<br/>sessions · pricing · payments"] -- "Vault v3 / Orders v2<br/>(store's credentials)" --> PP["PayPal sandbox"]
  API -- "signals, goal" --> LLM["AI pricing agent<br/>(local LLM)"]
  Edge["Edge (Raspberry Pi 5, Python)<br/>QR reader · mic · LED · buzzer"] -- "exit token, signals<br/>(shared secret)" --> API
  Edge -- "GPIO line" --> Relay["Relay"] -- "power" --> Lock["Lock<br/>(open only while powered)"]
  Dev["Development tools<br/>virtual exit gate · signal simulator"] -- "edge device's API" --> API
```

Read: only the BarPay backend talks to PayPal and the AI; the store app reports orders and draws every screen from its server's calls, and the customer's phone never sends amounts BarPay trusts.

Implementation notes:

- Linking creates a setup token with `POST /v3/vault/setup-tokens` (`payment_source.paypal` with `usage_type: MERCHANT` and `experience_context` return and cancel URLs back to BarPay), and when the store app confirms the link, creates the saved PayPal with `POST /v3/vault/payment-tokens` from that setup token.
- At check-in, a member's saved PayPal is checked with `GET /v3/vault/payment-tokens/{id}`.
- The checkout charge is an order with intent `CAPTURE`, `payment_source.paypal.vault_id` and an `invoice_id`.
- A member's saved PayPal is deleted, on request, with `DELETE /v3/vault/payment-tokens/{id}`.
- The edge device reads the USB QR reader through the input device (`evdev`), not `stdin`, so it works as a background service.
- The edge device takes the QR reader's input device from a setting in an environment variable, not a secret, and grabs it, so what the reader types reaches nothing else, such as the console. When the device cannot be opened, grabbed or read, such as when the reader is unplugged, including when the edge device starts, the edge device opens it again every 1 second and keeps running meanwhile: unlocks by hand and liveliness go on, and the reader shows "Waiting for a scan". It logs once each time the device can no longer be had and once each time it is open again, not on each failed try. Of the QR reader, only a setting that is not a path, or the `evdev` package not installed, keeps the edge device from starting, since waiting fixes neither. It turns the key codes back into characters with the US layout and Shift, covering every printable ASCII character, and takes what comes up to Enter as one read. A read that has a key typing no printable ASCII character, a key held until it repeats, or key events lost before they were read, is longer than 128 characters (the longest token `scanExitToken` takes), or stops for 0.5 seconds before Enter (from exactly 0.5 seconds after its last key on) is not sent to BarPay, and the reader shows "Please show a new QR code", so a broken read never reaches BarPay as a token and the customer shows the QR again; Enter alone is ignored. A reader types a whole read within milliseconds a key, so 0.5 seconds is far longer than any pause within a read.
- A scan waits up to 1 minute for BarPay's answer, which may come only after BarPay's call to PayPal (at most 10 seconds each). This and the other time limits of the edge device's calls below (10 seconds for a report, 35 seconds for `waitForUnlock`) bound connecting to BarPay and each wait for more of its answer, not the call as a whole. The gate stays locked and the reader shows "BarPay unreachable" when BarPay is not reached, does not answer within that minute, answers with a 5xx or with anything that is not a `ScanResult`, or refuses the scan (`400` or `401`, which a working edge device does not get). A QR read while a scan waits for BarPay's answer is dropped, not sent after it, so the gate opens at most once for each answer. A read of the same exit token as the last scan within 3 seconds after its result was shown is not sent, since a reader reads a QR held in front of it again and again; from exactly 3 seconds on it is sent, so scanning again to have the gate opened again (UC5's alternative flow) works as soon as the shortest result (Thank you) has been shown.
- The edge device reports each unlock once with `reportUnlockResult`. It sends a report again only when its outcome is not known (BarPay not reached, no answer within 10 seconds, a 5xx or an answer `edge.yaml` does not define), 2 seconds after the last send failed and up to 3 sends in all; a report BarPay refused is not sent again, since BarPay kept nothing and would answer the same.
- The edge device always keeps one `waitForUnlock` call waiting, so staff can open the gate by hand. Each call waits up to 35 seconds for BarPay's answer, 10 seconds longer than BarPay holds it; after a `204` or an unlock by hand it calls again at once, and after a call that failed, 2 seconds later, so it waits again soon after BarPay is back.
- The edge device shows each `reader_message` from `scanExitToken`, and the states that have none (Waiting for a scan, Working, BarPay unreachable, Unlock failed, Unlocked by hand), through the LED and the buzzer as in the indicator table (Hardware in [Fixed constraints](#8-fixed-constraints)).
- The edge device reports an unlock as worked (`unlocked` `true` in `reportUnlockResult`) when it set the lock's line high and read it back as high (Lock in Hardware, [Fixed constraints](#8-fixed-constraints)), and as failed otherwise.

## 11. Priorities

| Priority | Scope |
|---|---|
| P0 — must ship | BarPay: UC1–UC7 and UC9 with retry and "settled outside BarPay"; UC8 as a config file; the store API with session, member and rate lookup; the PayPal link with redirects and confirmation; the admin API; signed notifications with simple retries; AI pricing on request with goal, guardrails, explanations and decision log; cover charge in blocks with the pause and the cap; USD; development tools (virtual exit gate, signal simulator). Sample app with its own backend: entrance landing, sign-up, login and guest join, menu, ordering, settings with consent and PayPal link, panel, admin screen, rate evaluation requests every 5 minutes, receipt. README with an API table |
| P1 | Real edge device: QR reader → checkout → gate unlock, microphone liveliness; JPY (after Open question 3) |
| P2 | Store onboarding screens; saved PayPal across a chain's stores; AI suggestions beyond pricing (e.g. a summary of the stay on the receipt) |

## 12. Open questions

Question 2 is decided after building a mock in `mock/`. The mock is a throwaway prototype in plain HTML, CSS and JavaScript only, is not part of the product, and is not tracked in git, so it never reaches the public repository. Its UI is Japanese, for the author. It is exempt from translation pairing and from the product's tests and CI.

1. **Liveliness sensing** (closed): microphone loudness only (see Signals in the [Pricing model](#4-pricing-model)). Loudness is computed on the edge device alone and cheaply, which keeps [Privacy](#6-guarantees) simple. An on-device audio classifier (cheering, music, laughter) is not used and can be added later; a camera people-count is not used either, since it would change what occupancy means.
2. **Exit gate framing**: what the lock physically secures, so BarPay does not read as "customers are locked in".
3. **JPY in the sandbox**: confirm a sandbox business account can receive JPY without holding it as pending.
