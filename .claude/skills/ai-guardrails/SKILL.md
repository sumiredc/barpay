---
name: ai-guardrails
description: How BarPay code that turns the AI pricing agent's output into a published rate must be written and tested. Covers treating the model's output as a proposal that code decides on, passing every proposal through the guardrails (floor, ceiling, max step, rate step) deterministically after the model, writing the explanation about the rate after the guardrails, the fallback conditions, treating slow answers and broken output from the local LLM as normal results rather than exceptions, why there is always a previous rate, and testing with a swapped-in model that returns any output. Use when writing, changing, or reviewing the pricing agent, its prompt or parsing, the guardrails, the explanation, the decision log, or the code that publishes a rate.
---

# ai-guardrails — the model proposes, code decides

The AI pricing agent's output is a proposal; the rate BarPay publishes is decided by deterministic code that runs after the model on every evaluation.
**Not a procedure.** It gives the criteria for judging whether the path from the model to the published rate is safe, and why each one matters in BarPay.
What the guardrails are, how a proposal is clamped and rounded, and when the fallback applies are stated in the PRD's Pricing model and Guarantees, Price bounds; this skill points to them by name and does not restate them, so that changing the PRD never leaves a second, stale copy here. If writing the code shows that a rule is missing from the PRD, add it to the PRD (both languages) in the issue that needs it.
How concurrent evaluation requests are merged is in `external-side-effects`; how time limits are measured is in `time-dependent-logic`.

---

## The output is a proposal

Nothing the model returns is published, logged as the published rate, or used in a charge until code has parsed it and passed it through the guardrails.
A model's output is text produced by a sampler; it can be out of range, in the wrong unit, or confidently wrong, and no prompt changes that. The mistake of trusting it happens while writing the code, before any test, so it is ruled out by the shape of the code. Broken in BarPay: a proposal of "a tenfold rise" goes straight to the panel and into the next block's charge.

- **The function that publishes a rate accepts only a value that has passed the guardrails.** Give that value its own type, constructed only by the guardrail function, so the compiler rejects a raw proposal there. Broken in BarPay: a later change adds a shortcut that publishes the model's number, and nothing fails until a customer is charged above the ceiling.
- **The model chooses a value, not an action.** It does not call tools that change the rate, settings or sessions. Broken in BarPay: a model that is told it can "adjust the ceiling" raises the ceiling it is supposed to stay under.

---

## Guardrails always run, after the model

Every proposal goes through the guardrails — floor, ceiling, max step from the current rate, and the currency's rate step — in deterministic code after the model, as the PRD's Pricing model, Guardrails says.
The guardrails are what BarPay guarantees (the PRD's Guarantees, Price bounds); telling the model the limits in its prompt is a hint, not a guard. Broken in BarPay: a rate above the owner's ceiling or a jump larger than the max step is published, and the store charges more than it promised.

- **Round to the rate step without leaving the range, as the Guardrails item says.** If the code needs an order of clamping and rounding, or a rounding direction, the PRD does not yet state, add it to the PRD in the issue that needs it. Broken in BarPay: rounding to the rate step pushes a rate at the ceiling just above it.
- **The same input and settings give the same published rate.** The guardrail function is pure and reads the current rate and settings it is given. Broken in BarPay: two evaluations with the same proposal publish different rates and the decision log cannot explain why.
- **An owner's change to the floor or ceiling is applied by the same code, not by asking the model.** Moving the current rate into a new range is the PRD's exception to the max step and is recorded in the decision log. Broken in BarPay: the rate stays outside the new range until the next evaluation, and blocks are charged outside it.

---

## The explanation describes the published rate

The explanation is written about the rate after the guardrails — the change actually published — not about what the model proposed, as the PRD's Pricing model, Explanation says.
The model's reasoning describes its proposal; once the guardrails clamp it, that text describes a rate that was never published. Broken in BarPay: the store app's admin screen shows "rate up a lot" while the published rate moved by the max step, or "rate down" for a proposal clamped back up to the floor, and staff cannot trust the log.

- **Build the published change from the before and after values in code.** The model's reasoning goes into the decision log as its reasoning, beside the proposal. Broken in BarPay: the decision log cannot show the difference between what the agent wanted and what the guardrails allowed.

---

## Slow answers and broken output are normal results

The local LLM often answers late or returns output that cannot be read as a rate, so both are ordinary outcomes of an evaluation that the code handles as a value, not exceptions that escape it.
A local model is slow under load and drifts from the requested format; treating that as rare means it is handled badly when it happens every evening. Broken in BarPay: a panic on unparseable output crashes the backend in the middle of a visit, or an error escapes the evaluation and the fallback is never written to the decision log.

- **When to fall back is the PRD's Pricing model, Fallback.** The model not answering in time and an answer that cannot be read as a rate both keep the last rate and are written to the decision log. Do not add other reasons to fall back, or other behaviour on fallback, in code. Broken in BarPay: a timeout publishes a default rate instead of the last one and the price jumps without an evaluation the PRD allows.
- **Parse strictly.** A number found somewhere in prose, a value in the wrong unit, or a field that is missing is "cannot be read", not a best guess. Broken in BarPay: the model writes "about a third more than now", and a lenient parser reads "a third" as the rate, and the guardrails clamp it down by the max step when the model meant up.
- **The time limit on the model is enforced by the caller with a context, not left to the client library.** Broken in BarPay: a hung LLM connection holds the evaluation open and every later request waits on it, so the rate stops moving.
- **There is always a previous rate to keep.** The starting rate is published when BarPay starts (the PRD's Pricing model, First rate), so a fallback never has to invent one; code paths for "no previous rate" are not needed. Broken in BarPay: a made-up default for that case becomes a second starting rate that disagrees with the config file.

---

## How to prove it

Prove the path with the model swapped for a fake that returns whatever the test chooses, and assert that for every output the published rate is inside the guardrails and every fallback is in the decision log.
A test against the real local LLM proves only what that model said that day; the guarantee is about every possible output.

| Fake model returns | What the test asserts |
|---|---|
| A rate far above the ceiling, far below the floor, or beyond the max step | The published rate is clamped and rounded as the PRD states, stays within the floor and ceiling, and moves at most the max step from the previous rate |
| Output that cannot be read as a rate (not the expected format, missing field, prose, empty) | The last rate stays published; the decision log records the fallback and what the model returned (its proposal), as the PRD's Pricing model, Decision log says |
| No answer before the time limit | The last rate stays published; the decision log records the timeout; the evaluation returns instead of hanging |
| A valid rate inside the guardrails | That rate, rounded to the rate step, is published, and the explanation states the change actually published |

- **Generate many outputs, not just the cases above.** A property test that feeds random numbers and random text and checks "published rate within range and within the max step" covers the outputs no one thought to write. Broken in BarPay: an edge such as a negative or non-finite number slips past the hand-written cases and publishes a rate below the floor.
- **Assert on the published rate and the decision log, not on the parser's return value.** The guarantee is about what customers are charged. Broken in BarPay: the parser test passes while the code that publishes the rate bypasses it.
