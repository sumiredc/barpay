package evaluation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/sumiredc/barpay/internal/clock"
	"github.com/sumiredc/barpay/internal/llm"
	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/pricing"
	"github.com/sumiredc/barpay/internal/settings"
	"github.com/sumiredc/barpay/internal/signals"
	"github.com/sumiredc/barpay/internal/store"
)

// ModelTimeout is how long the AI has to answer (the PRD's Pricing model, Fallback). It is
// shorter than the spec's 30 seconds for requestRateEvaluation, leaving time to read the
// inputs and store the decision, so the fallback comes before the store app's call is cut off.
const ModelTimeout = 20 * time.Second

// Model answers a prompt: llm.Client, or a fake in tests. An error wrapping llm.ErrUnreadable
// means it answered but not with a completion; any other error means it did not answer.
type Model interface {
	Chat(ctx context.Context, messages []llm.Message) (string, error)
}

// Options are what an Evaluator runs with, from the config file and startup.State.
type Options struct {
	Store       *store.Store
	Clock       clock.Clock
	Model       Model
	MaxStep     money.Amount
	MinInterval time.Duration
	Capacity    int
	Log         *slog.Logger
}

// Evaluator runs at most one evaluation at a time and at most one per minimum interval (the
// PRD's Pricing model, Evaluation on request).
type Evaluator struct {
	opts    Options
	timeout time.Duration

	mu      sync.Mutex
	running *run // nil when no evaluation is running
	runs    sync.WaitGroup

	joined func() // called when a request joins a running evaluation; for tests
}

type run struct {
	done chan struct{}
	res  Result
	err  error
}

// Result is the spec's RateEvaluation.
type Result struct {
	Rate      pricing.Point
	Evaluated bool
	Fallback  pricing.Fallback // "" unless the evaluation kept the rate
}

// New returns an Evaluator.
func New(o Options) *Evaluator {
	return &Evaluator{opts: o, timeout: ModelTimeout}
}

// Request asks for an evaluation. A request that arrives while one runs waits for it and gets its
// result. Otherwise, one at least the minimum interval after the last evaluation started (exactly
// the interval included) starts one; a sooner one gets the current rate with Evaluated false.
// The evaluation is not cut short when the request that started it goes away: other requests
// may be waiting for it.
func (e *Evaluator) Request(ctx context.Context) (Result, error) {
	e.mu.Lock()
	r := e.running
	if r == nil {
		now := e.opts.Clock.Now()
		started, err := e.start(ctx, now)
		if err != nil || !started {
			e.mu.Unlock()
			if err != nil {
				return Result{}, err
			}
			return e.current(ctx)
		}
		r = &run{done: make(chan struct{})}
		e.running = r
		e.runs.Add(1)
		go e.run(context.WithoutCancel(ctx), r, now)
	} else if e.joined != nil {
		e.joined()
	}
	e.mu.Unlock()
	select {
	case <-r.done:
		return r.res, r.err
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

// Wait returns once no evaluation is running. Call it after the HTTP server has stopped and
// before the store closes, so an evaluation that outlived its requests still stores its decision.
func (e *Evaluator) Wait() { e.runs.Wait() }

// start stores now as the start of an evaluation if the last one started at least the minimum
// interval before now, and tells whether it did. The comparison is in the store, from the
// stored start, so it holds across a restart.
func (e *Evaluator) start(ctx context.Context, now time.Time) (bool, error) {
	var started bool
	err := e.opts.Store.Tx(ctx, func(tx store.Querier) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO evaluation_start (id, started_at) VALUES (1, ?1)
			ON CONFLICT (id) DO UPDATE SET started_at = excluded.started_at WHERE ?1 - started_at >= ?2`,
			now.UnixNano(), e.opts.MinInterval.Nanoseconds())
		if err != nil {
			return fmt.Errorf("evaluation: start: %w", err)
		}
		n, err := res.RowsAffected()
		started = n == 1
		return err
	})
	return started, err
}

func (e *Evaluator) current(ctx context.Context) (Result, error) {
	s, ok, err := settings.Load(ctx, e.opts.Store.Read())
	if err != nil || !ok {
		return Result{}, errors.Join(err, pricing.ErrNoRate)
	}
	p, err := pricing.Current(ctx, e.opts.Store.Read(), s.Currency)
	return Result{Rate: p}, err
}

func (e *Evaluator) run(ctx context.Context, r *run, startedAt time.Time) {
	defer func() {
		e.mu.Lock()
		e.running = nil
		e.mu.Unlock()
		close(r.done)
		e.runs.Done()
	}()
	r.res, r.err = e.evaluate(ctx, startedAt)
	if r.err != nil {
		e.opts.Log.Error("rate evaluation failed", "error", r.err)
	}
}

// evaluate reads the inputs as they are at startedAt, asks the model, and publishes the guarded
// proposal or keeps the rate on a fallback, recording the decision with it.
func (e *Evaluator) evaluate(ctx context.Context, startedAt time.Time) (Result, error) {
	sit, err := e.situation(ctx, startedAt)
	if err != nil {
		return Result{}, err
	}
	proposal, fb := e.ask(ctx, Messages(sit))
	var d pricing.Decision
	err = e.opts.Store.Tx(ctx, func(tx store.Querier) error {
		at := e.opts.Clock.Now()
		if fb != "" {
			d, err = pricing.KeepRate(ctx, tx, sit.Inputs, fb, at)
			return err
		}
		d, err = pricing.PublishEvaluation(ctx, tx, sit.Inputs, proposal, e.opts.MaxStep, at)
		return err
	})
	if err != nil {
		return Result{}, fmt.Errorf("evaluation: %w", err)
	}
	return Result{Rate: d.Published, Evaluated: true, Fallback: d.Fallback}, nil
}

// ask returns the model's proposal, or the fallback that applies. The time limit is held here,
// not left to the model: an answer that has not come when it passes is a timeout, even from a
// model that ignores its context.
func (e *Evaluator) ask(ctx context.Context, msgs []llm.Message) (pricing.Proposal, pricing.Fallback) {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	type answer struct {
		text string
		err  error
	}
	ch := make(chan answer, 1)
	go func() {
		text, err := e.opts.Model.Chat(ctx, msgs)
		ch <- answer{text, err}
	}()
	var a answer
	select {
	case a = <-ch:
	case <-ctx.Done():
		return pricing.Proposal{}, pricing.FallbackTimeout
	}
	switch {
	case a.err != nil && errors.Is(a.err, llm.ErrUnreadable):
		return pricing.Proposal{}, pricing.FallbackUnreadable
	case a.err != nil:
		e.opts.Log.Warn("the AI did not answer", "error", a.err)
		return pricing.Proposal{}, pricing.FallbackTimeout
	}
	p, ok := Parse(a.text)
	if !ok {
		return pricing.Proposal{}, pricing.FallbackUnreadable
	}
	return p, ""
}

// situation reads the rate, the settings and the signals in one consistent state.
func (e *Evaluator) situation(ctx context.Context, at time.Time) (Situation, error) {
	var sit Situation
	err := e.opts.Store.View(ctx, func(q store.Querier) error {
		s, ok, err := settings.Load(ctx, q)
		if err != nil {
			return err
		}
		if !ok {
			return pricing.ErrNoRate
		}
		cur, err := pricing.Current(ctx, q, s.Currency)
		if err != nil {
			return err
		}
		occupancy, err := signals.Occupancy(ctx, q, e.opts.Capacity)
		if err != nil {
			return err
		}
		var liveliness *float64
		if v, ok, err := signals.LivelinessAt(ctx, q, at); err != nil {
			return err
		} else if ok {
			liveliness = &v
		}
		sit = Situation{
			Current: cur.Rate, Floor: s.Floor, Ceiling: s.Ceiling, MaxStep: e.opts.MaxStep,
			Inputs: pricing.Inputs{Occupancy: occupancy, Liveliness: liveliness, Goal: s.Goal},
		}
		return nil
	})
	if err != nil {
		return Situation{}, fmt.Errorf("evaluation: read the inputs: %w", err)
	}
	return sit, nil
}
