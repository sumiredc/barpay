// Package idempotency handles the store app's Idempotency-Key the same way for every operation
// that takes one, as the spec's IdempotencyKey parameter says (the PRD's Guarantees, Single payment).
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/sumiredc/barpay/internal/clock"
	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/staff"
	"github.com/sumiredc/barpay/internal/store"
)

// Header is the spec's IdempotencyKey parameter.
const Header = "Idempotency-Key"

// Retention is the spec's IdempotencyKey: how long after it was kept a response is returned again.
const Retention = 24 * time.Hour

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

// Keeper holds the keys of the requests running now and reads the responses kept for keys.
type Keeper struct {
	st  *store.Store
	clk clock.Clock

	mu      sync.Mutex
	running map[slot]bool
}

// slot is a key in the space of one API key: the store API key and the admin API key are
// different callers, so each has its own keys.
type slot struct{ scope, key string }

// New returns a Keeper that keeps responses in st.
func New(st *store.Store, clk clock.Clock) *Keeper {
	return &Keeper{st: st, clk: clk, running: map[slot]bool{}}
}

// Claim is a request's hold on its key while its operation runs.
type Claim struct {
	slot
	request []byte
}

type claimKey struct{}

// FromRequest returns the claim Require gave req, or nil for a request Require did not pass.
func FromRequest(req *http.Request) *Claim {
	c, _ := req.Context().Value(claimKey{}).(*Claim)
	return c
}

// Require wraps an operation that takes an Idempotency-Key, on a route added with a key (not
// Public). It refuses a missing or malformed key, a key whose request is running now, and a key
// kept for another request. For a key kept for the same request it returns the kept response
// without running the operation. Otherwise it runs the operation, which keeps its response with
// Claim.Keep when it changes something.
//
// Running requests are held in memory, not stored: after a restart none is running, and an
// operation cut off before it committed left neither its change nor its response, so the key
// is free to be sent again.
func (k *Keeper) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		values := req.Header.Values(Header)
		if len(values) != 1 || !keyPattern.MatchString(values[0]) {
			httpapi.WriteError(w, http.StatusBadRequest, httpapi.CodeInvalidRequest,
				Header+" must be 1 to 128 letters, digits, '_', '.', ':' or '-'.", Header)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, httpapi.MaxBodyBytes))
		if err != nil {
			httpapi.WriteError(w, http.StatusBadRequest, httpapi.CodeInvalidRequest, "The body cannot be read or is too large.", "")
			return
		}
		c := &Claim{slot: slot{scope: httpapi.KeyName(req.URL.Path), key: values[0]}}
		// Only admin requests name a staff member; on other routes the header means nothing.
		staffID := ""
		if c.scope == "admin" {
			staffID = req.Header.Get(staff.Header)
		}
		c.request = fingerprint(req, staffID, body)
		if !k.hold(c.slot) {
			httpapi.WriteError(w, http.StatusConflict, httpapi.CodeIdempotencyKeyInProgress,
				"A request with this Idempotency-Key is still being processed; send it again shortly.", "")
			return
		}
		defer k.release(c.slot)

		kept, ok, err := k.kept(req.Context(), c.slot, k.clk.Now())
		switch {
		case err != nil:
			httpapi.WriteInternal(w)
			return
		case ok && !bytes.Equal(kept.request, c.request):
			httpapi.WriteError(w, http.StatusConflict, httpapi.CodeIdempotencyKeyReused,
				"This Idempotency-Key was used with a different request.", "")
			return
		case ok:
			Write(w, kept.status, kept.body)
			return
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), claimKey{}, c)))
	})
}

// Write writes a JSON response body as kept, so the first answer and a repeat are the same bytes.
func Write(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// Keep stores the response to c's request in tx, the transaction of the change it answers, so
// a resend gets it back exactly when that change is kept; at is the instant of the change. Call
// it for every response given after something changed, an error included. A response given
// before anything changed is not kept, so the same key can be sent again.
func (c *Claim) Keep(ctx context.Context, tx store.Querier, at time.Time, status int, body []byte) error {
	// A key kept for longer than Retention is forgotten, and Require let this request run as a new one.
	res, err := tx.ExecContext(ctx, `INSERT INTO idempotency_keys (scope, key, request, status, body, kept_at)
		VALUES (?1, ?2, ?3, ?4, ?5, ?6)
		ON CONFLICT (scope, key) DO UPDATE SET request = excluded.request, status = excluded.status,
			body = excluded.body, kept_at = excluded.kept_at
		WHERE idempotency_keys.kept_at < ?7`,
		c.scope, c.key, c.request, status, body, at.UnixNano(), forgottenBefore(at))
	if err != nil {
		return fmt.Errorf("idempotency: keep: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return errors.New("idempotency: keep: a response is already kept for this key")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE kept_at < ?`, forgottenBefore(at)); err != nil {
		return fmt.Errorf("idempotency: forget: %w", err)
	}
	return nil
}

// forgottenBefore is the one boundary of Retention: at now, a response kept before the returned
// instant is forgotten, and one kept at it or later is returned, so a response is returned until
// exactly Retention after it was kept.
func forgottenBefore(now time.Time) int64 { return now.Add(-Retention).UnixNano() }

type keptResponse struct {
	request []byte
	status  int
	body    []byte
}

func (k *Keeper) kept(ctx context.Context, s slot, now time.Time) (keptResponse, bool, error) {
	var r keptResponse
	err := k.st.Read().QueryRowContext(ctx, `SELECT request, status, body FROM idempotency_keys
		WHERE scope = ? AND key = ? AND kept_at >= ?`, s.scope, s.key, forgottenBefore(now)).Scan(&r.request, &r.status, &r.body)
	if errors.Is(err, sql.ErrNoRows) {
		return keptResponse{}, false, nil
	}
	if err != nil {
		return keptResponse{}, false, fmt.Errorf("idempotency: read: %w", err)
	}
	return r, true, nil
}

func (k *Keeper) hold(s slot) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.running[s] {
		return false
	}
	k.running[s] = true
	return true
}

func (k *Keeper) release(s slot) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.running, s)
}

// fingerprint identifies a request: its method, its path with the query, the staff member it
// names (staffID, empty outside the admin API), and its body, byte for byte. The same key with
// another operation, another session, another staff member or another body is another request,
// so one staff member never gets the response kept for another's request.
func fingerprint(req *http.Request, staffID string, body []byte) []byte {
	h := sha256.New()
	fmt.Fprintf(h, "%s %s\x00%s\x00", req.Method, req.URL.RequestURI(), staffID)
	h.Write(body)
	return h.Sum(nil)
}
