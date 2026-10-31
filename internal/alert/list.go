package alert

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/store"
)

// listName names the alert list in its cursors.
const listName = "alerts"

// errUnknownCursor is returned by list for a position that names no alert.
var errUnknownCursor = errors.New("alert: no alert at the cursor")

// listed is an alert and its raising position.
type listed struct {
	Alert
	seq int64
}

// list returns up to limit alerts raised before the one at after (0: from the newest), newest
// first, and whether more follow.
func list(ctx context.Context, q store.Querier, after int64, limit int) ([]listed, bool, error) {
	if after != 0 {
		var one int
		err := q.QueryRowContext(ctx, `SELECT 1 FROM alerts WHERE seq = ?`, after).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, errUnknownCursor
		}
		if err != nil {
			return nil, false, fmt.Errorf("alert: list: %w", err)
		}
	}
	rows, err := q.QueryContext(ctx, `SELECT seq, id, kind, raised_at, session_id, member_id, message FROM alerts
		WHERE ?1 = 0 OR seq < ?1 ORDER BY seq DESC LIMIT ?2`, after, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("alert: list: %w", err)
	}
	defer rows.Close()
	var out []listed
	for rows.Next() {
		var l listed
		var kind string
		var at int64
		if err := rows.Scan(&l.seq, &l.ID, &kind, &at, &l.SessionID, &l.MemberID, &l.Message); err != nil {
			return nil, false, fmt.Errorf("alert: list: %w", err)
		}
		l.Kind, l.RaisedAt = Kind(kind), time.Unix(0, at).UTC()
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("alert: list: %w", err)
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// alertJSON is the spec's Alert. session_id and member_id are left out when it is not about one.
type alertJSON struct {
	ID        string `json:"id"`
	Kind      Kind   `json:"kind"`
	RaisedAt  string `json:"raised_at"`
	SessionID string `json:"session_id,omitempty"`
	MemberID  string `json:"member_id,omitempty"`
	Message   string `json:"message"`
}

// API serves listAlerts.
type API struct {
	Store *store.Store
	Log   *slog.Logger
}

// Register adds listAlerts to r.
func (a API) Register(r *httpapi.Router) {
	r.Admin("GET /alerts", http.HandlerFunc(a.listAlerts))
}

func (a API) listAlerts(w http.ResponseWriter, req *http.Request) {
	page, ok := httpapi.ParsePage(w, req, listName)
	if !ok {
		return
	}
	as, more, err := list(req.Context(), a.Store.Read(), page.After, page.Limit)
	if errors.Is(err, errUnknownCursor) {
		httpapi.InvalidCursor(w)
		return
	}
	if err != nil {
		a.Log.Error("alerts not listed", "error", err)
		httpapi.WriteInternal(w)
		return
	}
	body := struct {
		Alerts     []alertJSON `json:"alerts"`
		NextCursor *string     `json:"next_cursor"`
	}{Alerts: make([]alertJSON, len(as))}
	for i, l := range as {
		body.Alerts[i] = alertJSON{
			ID: l.ID, Kind: l.Kind, RaisedAt: httpapi.FormatTime(l.RaisedAt),
			SessionID: l.SessionID, MemberID: l.MemberID, Message: l.Message,
		}
	}
	if more {
		c := httpapi.Cursor(listName, as[len(as)-1].seq)
		body.NextCursor = &c
	}
	httpapi.WriteJSON(w, http.StatusOK, body)
}
