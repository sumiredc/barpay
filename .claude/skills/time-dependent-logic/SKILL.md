---
name: time-dependent-logic
description: How BarPay code whose result depends on the current time must be written and tested — block ends and their charges, the pause for additional orders, the cap per visit, the maximum stay, the re-unlock window, exit token refresh, the staff retry and unpaid windows, notification retries, the minimum evaluation interval, and idempotency key retention. Covers taking the time as an input instead of reading the clock, recomputing time limits from stored times so they hold across a restart instead of keeping timers in memory, deciding what happens at the exact boundary instant, where the values and boundary rules come from, and testing with a fake clock, across a restart, and on both sides of each boundary. Use when writing, changing, or reviewing any code that reads the time, schedules or expires something, or computes an amount or a state from elapsed time.
---

# time-dependent-logic — results that depend on when they are computed

Code whose result depends on the time takes the time as an input, computes every time limit from times it has stored, and decides on purpose what happens at the exact boundary instant.
**Not a procedure.** It gives the criteria for judging whether time-dependent code is correct, and why each one matters in BarPay.
Every duration in BarPay is real time (the PRD's Pricing model, Time). The values of the durations and which side of a boundary an instant belongs to are stated in the PRD; this skill points to them by name and does not restate them, so that changing the PRD never leaves a second, stale copy here.
How to write tests in general is in `testing`; how calls to PayPal and the store app behave around timeouts is in `external-side-effects`.

---

## Take the time as an input

Code that needs the current time receives it — through a clock passed in, or as an argument — and never reads the system clock directly.
A function that calls the system clock gives a different answer each run, so its boundaries cannot be tested, and two parts of one operation can see two different "nows". Broken in BarPay: the final total is fixed at one instant and the cover charge is stopped at a slightly later one, so a block that ends in between is charged on top of a total the receipt already shows.

- **Read the time once per operation and pass that value down.** Checkout, a register request, or a block end each use one instant for every decision they make. Broken in BarPay: the receipt and the charged amount disagree, which the PRD's Guarantees, Accuracy forbids.
- **The only place that reads the real clock is the code that builds the clock.** Everything else depends on the interface. Broken in BarPay: one stray direct read makes a test pass on a fast machine and fail on a slow one, and the bug it hides reaches production.

---

## Compute time limits from stored times, not from timers

A time limit is stored as the instant it started (check-in, the last order, checkout began, the request time) and recomputed from that instant whenever it matters; it is never held only as a timer or a sleeping goroutine in memory.
A timer in memory is lost on restart, while the stored instants survive it (the PRD's Guarantees, Durability), so time limits must be derived from them. Broken in BarPay: the backend restarts during a visit, the block timer is gone, and the customer stays for free until checkout; or the maximum stay passes unnoticed and a linked session is never checked out.

- **A background loop may wake things up, but it decides from stored times.** On start and on each wake it finds what is due by comparing stored instants with the current time, so it catches up on anything that fell due while it was down. Broken in BarPay: a PaymentFailed session whose staff retry window ended during a restart is never closed as unpaid, and staff can still retry a charge the PRD no longer allows.
- **Each block is charged once, from its stored end instant, and never recomputed.** Which blocks have ended is derived from stored instants (check-in and the end of the last charged block), so a missed tick is caught up; each block's amount follows the PRD's Pricing model, Blocks and Settings changes (which rate applies, and from which block a changed setting applies), and is stored under a key for that block, so a doubled tick cannot add it twice. Broken in BarPay: a slow loop skips a block and it is never charged, runs twice and charges it twice, or recomputes past blocks at today's rate and changes what the customer already accrued.
- **Time limits after the session has moved on are checked against its state.** Which blocks count once checkout begins is the PRD's Pricing model, Blocks; a window measured from closing starts at the stored close time. Broken in BarPay: a block ends while the charge is in flight and is added to a total that is already fixed.

---

## Decide the boundary instant

For every time limit, decide what happens at exactly the boundary instant: the end of a block, the block that reaches the cap, an order at the moment a block ends, and a request at exactly the end of a window.
Most bugs in time-dependent code are off by one instant at a boundary, and the instant "exactly at" is the one a test with real time never hits. Broken in BarPay: a block ending at exactly the end of a pause is charged by one part of the code and free in another, so the panel and the receipt show different cover charges.

- **The values and which side the boundary falls on come from the PRD.** Block length, pause for additional orders and cap per visit are in the Pricing model; maximum stay, the re-unlock window and exit token refresh are in the Use cases; the staff retry and unpaid windows are in Guarantees and the Use cases; notification retries are in Responsibilities, Notifications; the minimum evaluation interval is in Pricing model, Evaluation on request; key retention is in the OpenAPI spec's `IdempotencyKey` parameter. Read a value the PRD makes configurable from the config file or settings; keep a value the PRD fixes in one named constant whose name points to the PRD item. Do not copy any value into comments or this skill. Broken in BarPay: a value copied into code stays the same after the owner changes the setting, and customers are charged by the old rule.
- **When the PRD does not say which side an instant falls on, add it to the PRD in the issue that needs it, in both languages, and test it there.** Deciding it silently in code makes the rule invisible to the next reader and to the store. Broken in BarPay: the token endpoint and the gate make opposite choices for the same instant, and the store app shows an exit QR that the gate then rejects.
- **Write each boundary as one comparison in one function.** "Has this block ended at time t" and "is this window still open at time t" are each defined once and called everywhere. Broken in BarPay: the panel says the cap is reached while the next block is still charged.

---

## How to prove it

Prove time-dependent code with a fake clock that the test moves forward, with a test that crosses a restart, and with tests just before, exactly at, and just after each boundary — never with sleep.
Sleep makes tests slow and flaky, and it cannot land on an exact instant, so the boundary that matters is never tested.

| Technique | What the test asserts |
|---|---|
| Fake clock moved forward by the test | After moving past a block end, the block is charged at the rate published at that instant; before it, the block is not charged |
| Restart in the middle | Store a session, drop every in-memory object, build a new service from the store, move the clock past a time limit: the block is charged, the maximum stay checks out, the window closes, exactly as without the restart |
| Just before, exactly at, just after | For each boundary — block end, the block that reaches the cap, end of the pause, end of each window — the three instants give the results the PRD states |

- **Move the clock in steps that land on boundaries, not only in large jumps.** A jump over a boundary hides whether the code handled the instant itself. Broken in BarPay: a test that jumps an hour ahead passes while a block ending exactly at a pause's end is charged wrongly.
- **Assert on stored state after the restart, not on what the old objects held.** Only the stored state survives. Broken in BarPay: the test passes because it kept a reference to the old timer, and production loses the time limit.
