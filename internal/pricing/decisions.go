package pricing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/settings"
	"github.com/sumiredc/barpay/internal/store"
)

// Fallback is why an evaluation kept the rate (the PRD's Pricing model, Fallback; the spec's
// Decision.fallback).
type Fallback string

const (
	FallbackTimeout    Fallback = "ai_timeout"
	FallbackUnreadable Fallback = "ai_unreadable"
)

// Limits of what the AI writes. MaxReason leaves room in the spec's 200-character explanation
// for the change BarPay adds; MaxReasoning is the spec's Decision.reasoning maxLength.
const (
	MaxReason    = 120
	MaxReasoning = 4000
)

// Inputs are the signals and goal an evaluation used: the spec's Decision.inputs.
type Inputs struct {
	Occupancy   float64  `json:"occupancy"`
	Liveliness  *float64 `json:"liveliness"` // nil: no liveliness signal
	StaffEvents []string `json:"staff_events"`
	Goal        int      `json:"goal"`
}

// Proposal is the AI's answer, read strictly.
type Proposal struct {
	// RatePerHour is in the smallest unit of the store's currency, from 0 to money.MaxValue.
	RatePerHour int64
	// Reason is the short reason the explanation starts with; BarPay adds the change it published.
	Reason string
	// Reasoning is how the proposal serves the goal, kept in the decision log.
	Reasoning string
}

// ErrInvalidProposal is returned for a Proposal outside the limits above, which the parser of
// the AI's answer rules out.
var ErrInvalidProposal = errors.New("pricing: proposal is outside its limits")

func (p Proposal) valid() bool {
	rs := utf8.RuneCountInString(p.Reasoning)
	return p.RatePerHour >= 0 && p.RatePerHour <= money.MaxValue && ReadableReason(p.Reason) &&
		rs >= 1 && rs <= MaxReasoning
}

// ReadableReason tells whether reason can start an explanation (the PRD's Pricing model,
// Prompt): 1 to MaxReason characters once whitespace around it is removed, something left once
// trailing spaces and dots are removed too, and no control, line or paragraph separator, or
// format character, which could break the line or reorder the text on the admin screen.
func ReadableReason(reason string) bool {
	r := strings.TrimSpace(reason)
	n := utf8.RuneCountInString(r)
	return n >= 1 && n <= MaxReason && trimReason(r) != "" && !strings.ContainsFunc(r, unsafeInLine)
}

// trimReason is the reason as the explanation shows it, before BarPay adds the change.
func trimReason(reason string) string {
	return strings.TrimRight(strings.TrimSpace(reason), ". ")
}

func unsafeInLine(r rune) bool {
	return unicode.IsControl(r) || r == ' ' || r == ' ' || unicode.Is(unicode.Cf, r)
}

// Decision is an entry of the decision log: the spec's Decision.
type Decision struct {
	Seq       int64 // decision order
	DecidedAt time.Time
	Trigger   Trigger
	Staff     string // for TriggerRangeChange
	Inputs    *Inputs
	Proposal  *money.Amount // nil on fallback and for TriggerRangeChange
	Reasoning *string       // nil on fallback and for TriggerRangeChange
	Published Point         // the rate published, or on fallback the rate kept
	// Explanation states the change actually published, after the guardrails.
	Explanation string
	Fallback    Fallback // "" unless the rate was kept
}

// PublishEvaluation guards p against the settings saved and the rate published now, publishes
// the result at at and records the decision with in, all in tx, which must come from
// store.Store.Tx (see PublishProposal). The explanation is written here from the published
// change, after the guardrails; only its reason comes from the AI.
func PublishEvaluation(ctx context.Context, tx store.Querier, in Inputs, p Proposal, maxStep money.Amount, at time.Time) (Decision, error) {
	if !p.valid() {
		return Decision{}, ErrInvalidProposal
	}
	s, ok, err := settings.Load(ctx, tx)
	if err != nil {
		return Decision{}, err
	}
	if !ok {
		return Decision{}, ErrNoRate
	}
	proposal := money.New(p.RatePerHour, s.Currency)
	pt, seq, err := publishProposal(ctx, tx, s, proposal, maxStep, at)
	if err != nil {
		return Decision{}, err
	}
	reasoning := p.Reasoning
	d := Decision{
		DecidedAt: pt.PublishedAt, Trigger: TriggerEvaluation, Inputs: &in, Proposal: &proposal,
		Reasoning: &reasoning, Published: pt, Explanation: evaluationExplanation(p.Reason, pt),
	}
	return d, record(ctx, tx, &d, seq)
}

// KeepRate records an evaluation that fell back for fb: the rate published now stays, and is
// what the decision names. It publishes nothing. tx is as for PublishEvaluation.
func KeepRate(ctx context.Context, tx store.Querier, in Inputs, fb Fallback, at time.Time) (Decision, error) {
	if fb != FallbackTimeout && fb != FallbackUnreadable {
		return Decision{}, fmt.Errorf("pricing: unknown fallback %q", fb)
	}
	s, ok, err := settings.Load(ctx, tx)
	if err != nil {
		return Decision{}, err
	}
	if !ok {
		return Decision{}, ErrNoRate
	}
	pt, err := Current(ctx, tx, s.Currency)
	if err != nil {
		return Decision{}, err
	}
	seq, err := currentSeq(ctx, tx, s.Currency)
	if err != nil {
		return Decision{}, err
	}
	d := Decision{
		DecidedAt: at.UTC(), Trigger: TriggerEvaluation, Inputs: &in, Published: pt,
		Explanation: fallbackExplanation(fb, pt), Fallback: fb,
	}
	return d, record(ctx, tx, &d, seq)
}

func record(ctx context.Context, tx store.Querier, d *Decision, rateSeq int64) error {
	if d.Inputs.StaffEvents == nil {
		d.Inputs.StaffEvents = []string{}
	}
	inputs, err := json.Marshal(d.Inputs)
	if err != nil {
		return fmt.Errorf("pricing: record decision: %w", err)
	}
	var proposal, reasoning, fallback any
	if d.Proposal != nil {
		proposal = d.Proposal.Value()
	}
	if d.Reasoning != nil {
		reasoning = *d.Reasoning
	}
	if d.Fallback != "" {
		fallback = string(d.Fallback)
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO decisions (decided_at, trigger, staff_id, inputs, proposal, reasoning, rate_seq, explanation, fallback)
		VALUES (?, ?, '', ?, ?, ?, ?, ?, ?) RETURNING seq`,
		d.DecidedAt.UnixNano(), string(d.Trigger), string(inputs), proposal, reasoning, rateSeq, d.Explanation, fallback).Scan(&d.Seq)
	if err != nil {
		return fmt.Errorf("pricing: record decision: %w", err)
	}
	return nil
}

// evaluationExplanation: the AI's reason, then the change actually published, e.g.
// "Room is half empty — rate down $0.50 to $11.50/h."
func evaluationExplanation(reason string, p Point) string {
	return trimReason(reason) + " — " + changeText(p) + "."
}

func changeText(p Point) string {
	switch c := p.Change.Value(); {
	case c > 0:
		return fmt.Sprintf("rate up %s to %s/h", format(p.Change), format(p.Rate))
	case c < 0:
		return fmt.Sprintf("rate down %s to %s/h", format(money.New(-c, p.Change.Currency())), format(p.Rate))
	}
	return fmt.Sprintf("rate unchanged at %s/h", format(p.Rate))
}

func fallbackExplanation(fb Fallback, p Point) string {
	why := "No answer from the AI in time"
	if fb == FallbackUnreadable {
		why = "The AI's answer could not be read"
	}
	return fmt.Sprintf("%s — rate kept at %s/h.", why, format(p.Rate))
}

// ErrUnknownCursor is returned by ListDecisions for a position that names no decision.
var ErrUnknownCursor = errors.New("pricing: no decision at the cursor")

// ListDecisions returns up to limit decisions recorded before the one at after (0: from the
// newest), newest first, and whether more follow.
func ListDecisions(ctx context.Context, q store.Querier, after int64, limit int) ([]Decision, bool, error) {
	if after != 0 {
		var one int
		err := q.QueryRowContext(ctx, `SELECT 1 FROM decisions WHERE seq = ?`, after).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, ErrUnknownCursor
		}
		if err != nil {
			return nil, false, fmt.Errorf("pricing: list decisions: %w", err)
		}
	}
	rows, err := q.QueryContext(ctx, `SELECT d.seq, d.decided_at, d.trigger, d.staff_id, d.inputs, d.proposal,
		d.reasoning, d.explanation, d.fallback, r.currency, r.rate, r.change, r.published_at
		FROM decisions d JOIN rates r ON r.seq = d.rate_seq
		WHERE ?1 = 0 OR d.seq < ?1 ORDER BY d.seq DESC LIMIT ?2`, after, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("pricing: list decisions: %w", err)
	}
	defer rows.Close()
	var out []Decision
	for rows.Next() {
		var d Decision
		var decided, rate, change, published int64
		var trigger, staffID, cur string
		var inputs, reasoning, fallback sql.NullString
		var proposal sql.NullInt64
		if err := rows.Scan(&d.Seq, &decided, &trigger, &staffID, &inputs, &proposal, &reasoning,
			&d.Explanation, &fallback, &cur, &rate, &change, &published); err != nil {
			return nil, false, fmt.Errorf("pricing: list decisions: %w", err)
		}
		c := money.Currency(cur)
		d.DecidedAt, d.Trigger, d.Staff, d.Fallback = time.Unix(0, decided).UTC(), Trigger(trigger), staffID, Fallback(fallback.String)
		d.Published = Point{Rate: money.New(rate, c), Change: money.New(change, c), PublishedAt: time.Unix(0, published).UTC()}
		if inputs.Valid {
			d.Inputs = &Inputs{}
			if err := json.Unmarshal([]byte(inputs.String), d.Inputs); err != nil {
				return nil, false, fmt.Errorf("pricing: list decisions: inputs of %d: %w", d.Seq, err)
			}
		}
		if proposal.Valid {
			a := money.New(proposal.Int64, c)
			d.Proposal = &a
		}
		if reasoning.Valid {
			d.Reasoning = &reasoning.String
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("pricing: list decisions: %w", err)
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}
