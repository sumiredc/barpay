package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/sumiredc/barpay/internal/alert"
	"github.com/sumiredc/barpay/internal/clock"
	"github.com/sumiredc/barpay/internal/secret"
	"github.com/sumiredc/barpay/internal/store"
)

// Status is the spec's NotificationRecord.status.
type Status string

const (
	Retrying  Status = "retrying" // not acknowledged yet; another attempt is due
	Delivered Status = "delivered"
	GivenUp   Status = "given_up"
)

// retryAfter is the PRD's Responsibilities, Notifications: when an unacknowledged notification is
// sent again, counted from its first attempt. After the last one it is given up.
var retryAfter = []time.Duration{
	time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour,
}

// attemptTimeout is the spec's Acknowledged: no answer within it counts as not acknowledged.
const attemptTimeout = 10 * time.Second

// SignatureHeader is the spec's Signature parameter.
const SignatureHeader = "BarPay-Signature"

// pollInterval is how often Run looks for notifications that fell due. It only wakes the loop:
// what is due is decided from the stored times.
const pollInterval = time.Second

// batch is how many due notifications one DeliverDue sends at most.
const batch = 100

// nextAttempt returns the first retry time after at, counted from first, or false when the last
// one is not after at. An attempt made late, past one or more retry times (BarPay was stopped, or
// still sending other notifications), therefore counts for every retry time it passed, and the
// next one is the first still ahead.
func nextAttempt(first, at time.Time) (time.Time, bool) {
	for _, d := range retryAfter {
		if next := first.Add(d); next.After(at) {
			return next, true
		}
	}
	return time.Time{}, false
}

// sign returns the BarPay-Signature header for body sent at t: HMAC-SHA256 with key over
// "<unix seconds>.<body>", as the spec's Signature parameter says.
func sign(key secret.Value, t time.Time, body []byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(key.Reveal()))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// Options configure a Dispatcher.
type Options struct {
	// URL is the notification URL from the config file, already checked by it.
	URL string
	// Secret is the signing secret.
	Secret secret.Value
	Clock  clock.Clock
	Log    *slog.Logger
	// Transport sends the requests; nil means http.DefaultTransport.
	Transport http.RoundTripper
	// Timeout is how long an attempt waits for an answer; 0 means the spec's 10 seconds.
	Timeout time.Duration
}

// Dispatcher sends stored notifications to the store app, retries them on the PRD's schedule
// and gives them up after the last retry. It keeps nothing in memory between attempts: every
// decision is made from the stored attempt times, so a restart resumes where it stopped.
type Dispatcher struct {
	st     *store.Store
	url    string
	key    secret.Value
	clk    clock.Clock
	log    *slog.Logger
	client *http.Client
	wake   chan struct{}

	mu sync.Mutex // one DeliverDue at a time, so a notification is not sent twice at once
}

// NewDispatcher returns a Dispatcher sending what st holds.
func NewDispatcher(st *store.Store, o Options) *Dispatcher {
	timeout := o.Timeout
	if timeout == 0 {
		timeout = attemptTimeout
	}
	transport := o.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &Dispatcher{
		st: st, url: o.URL, key: o.Secret, clk: o.Clock, log: o.Log,
		client: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			// A redirect is not an acknowledgement, and following it could send the body to
			// another place than the configured URL, or over http.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		wake: make(chan struct{}, 1),
	}
}

// Wake makes Run deliver at once. Call it after committing a transaction that stored a notification.
func (d *Dispatcher) Wake() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Run delivers due notifications when it starts, when woken and every second, until ctx ends.
func (d *Dispatcher) Run(ctx context.Context) {
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		if _, err := d.DeliverDue(ctx); err != nil && ctx.Err() == nil {
			d.log.Error("notifications not delivered", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-d.wake:
		case <-tick.C:
		}
	}
}

type pending struct {
	id, typ, subjectKind, subjectID string
	body                            []byte
	attempts                        int
	first                           sql.NullInt64
}

// DeliverDue makes one attempt for each notification whose next attempt time is not after now,
// and returns how many attempts it made. An attempt cut off because ctx ended is not recorded,
// so the notification is sent again after a restart.
func (d *Dispatcher) DeliverDue(ctx context.Context) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	due, err := d.due(ctx, d.clk.Now())
	if err != nil {
		return 0, err
	}
	for i, n := range due {
		if err := d.attempt(ctx, n); err != nil {
			return i, err
		}
	}
	return len(due), nil
}

func (d *Dispatcher) due(ctx context.Context, now time.Time) ([]pending, error) {
	rows, err := d.st.Read().QueryContext(ctx, `SELECT id, type, subject_kind, subject_id, body, attempts, first_attempt_at
		FROM notifications WHERE status = 'retrying' AND next_attempt_at <= ?
		ORDER BY next_attempt_at, seq LIMIT ?`, now.UnixNano(), batch)
	if err != nil {
		return nil, fmt.Errorf("notify: due: %w", err)
	}
	defer rows.Close()
	var out []pending
	for rows.Next() {
		var n pending
		if err := rows.Scan(&n.id, &n.typ, &n.subjectKind, &n.subjectID, &n.body, &n.attempts, &n.first); err != nil {
			return nil, fmt.Errorf("notify: due: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (d *Dispatcher) attempt(ctx context.Context, n pending) error {
	at := d.clk.Now()
	acked, reason := d.send(ctx, n.body, at)
	if err := ctx.Err(); err != nil {
		return err
	}
	return d.record(ctx, n, at, acked, reason)
}

// send posts body signed for at, and reports whether the store app acknowledged it. reason says
// why not, without the URL, which can carry credentials.
func (d *Dispatcher) send(ctx context.Context, body []byte, at time.Time) (bool, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(body))
	if err != nil {
		return false, "request not built"
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "BarPay")
	req.Header.Set(SignatureHeader, sign(d.key, at, body))
	resp, err := d.client.Do(req)
	if err != nil {
		var ue *url.Error
		switch {
		case errors.As(err, &ue) && ue.Timeout():
			return false, "no answer within " + d.client.Timeout.String()
		case errors.As(err, &ue):
			return false, ue.Err.Error()
		}
		return false, "not sent"
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return true, ""
	}
	return false, "status " + strconv.Itoa(resp.StatusCode)
}

func (d *Dispatcher) record(ctx context.Context, n pending, at time.Time, acked bool, reason string) error {
	first := at
	if n.first.Valid {
		first = time.Unix(0, n.first.Int64)
	}
	attempts := n.attempts + 1
	status, next := Delivered, sql.NullInt64{}
	if !acked {
		status = Retrying
		if t, ok := nextAttempt(first, at); ok {
			next = sql.NullInt64{Int64: t.UnixNano(), Valid: true}
		} else {
			status = GivenUp
		}
	}
	err := d.st.Tx(ctx, func(tx store.Querier) error {
		_, err := tx.ExecContext(ctx, `UPDATE notifications SET status = ?, attempts = ?,
			first_attempt_at = ?, last_attempt_at = ?, next_attempt_at = ?
			WHERE id = ? AND status = 'retrying'`,
			string(status), attempts, first.UnixNano(), at.UnixNano(), next, n.id)
		if err != nil {
			return fmt.Errorf("notify: record attempt: %w", err)
		}
		if status == GivenUp {
			return d.giveUp(ctx, tx, n, at, attempts)
		}
		return nil
	})
	if err != nil {
		return err
	}
	log := d.log.With("event_id", n.id, "type", n.typ, "attempts", attempts)
	switch status {
	case Delivered:
		log.Info("notification delivered")
	case Retrying:
		log.Warn("notification not acknowledged", "reason", reason, "next_attempt_at", time.Unix(0, next.Int64).UTC())
	case GivenUp:
		log.Error("notification given up", "reason", reason)
	}
	return nil
}

func (d *Dispatcher) giveUp(ctx context.Context, tx store.Querier, n pending, at time.Time, attempts int) error {
	a := alert.Alert{
		Kind:     alert.NotificationGivenUp,
		RaisedAt: at,
		Message: fmt.Sprintf("The store app did not acknowledge the notification %s (%s) after %d attempts over %.0f hours, "+
			"so BarPay stopped sending it. The store app can read the current state by lookup.",
			n.id, n.typ, attempts, retryAfter[len(retryAfter)-1].Hours()),
	}
	if subjectKind(n.subjectKind) == memberSubject {
		a.MemberID = n.subjectID
	} else {
		a.SessionID = n.subjectID
	}
	_, err := alert.Raise(ctx, tx, a)
	return err
}
