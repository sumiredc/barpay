package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/store"
)

// sessionsList names the session list in its cursors.
const sessionsList = "sessions"

// errUnknownCursor is returned by list for a position that names no session.
var errUnknownCursor = errors.New("session: no session at the cursor")

// parseStates reads the spec's state filter of listSessions: comma-separated SessionState
// values. It returns nil when there is none, which lists every state.
func parseStates(q url.Values) ([]State, bool) {
	vals, ok := q["state"]
	if !ok {
		return nil, true
	}
	var out []State
	for _, v := range vals {
		for _, s := range strings.Split(v, ",") {
			if !slices.Contains(States, State(s)) {
				return nil, false
			}
			if !slices.Contains(out, State(s)) {
				out = append(out, State(s))
			}
		}
	}
	return out, true
}

// listed is one page of sessions as read, before they are shown.
type listed struct {
	sessions []Session
	seqs     []int64
	more     bool
}

// page reads up to limit sessions checked in before the one at after (0: from the newest), newest
// first, in one of states (any state when states is nil).
func page(ctx context.Context, q store.Querier, states []State, after int64, limit int) (listed, error) {
	if after != 0 {
		var one int
		err := q.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE seq = ?`, after).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return listed{}, errUnknownCursor
		}
		if err != nil {
			return listed{}, fmt.Errorf("session: list: %w", err)
		}
	}
	query := `SELECT seq, ` + sessionColumns + ` FROM sessions WHERE (?1 = 0 OR seq < ?1)`
	args := []any{after}
	if states != nil {
		query += ` AND state IN (?` + strings.Repeat(", ?", len(states)-1) + `)`
		for _, s := range states {
			args = append(args, string(s))
		}
	}
	query += ` ORDER BY seq DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return listed{}, fmt.Errorf("session: list: %w", err)
	}
	defer rows.Close()
	var out listed
	for rows.Next() {
		var seq int64
		s, err := scanSession(rows, &seq)
		if err != nil {
			return listed{}, fmt.Errorf("session: list: %w", err)
		}
		out.sessions = append(out.sessions, s)
		out.seqs = append(out.seqs, seq)
	}
	if err := rows.Err(); err != nil {
		return listed{}, fmt.Errorf("session: list: %w", err)
	}
	if len(out.sessions) > limit {
		out.sessions, out.seqs, out.more = out.sessions[:limit], out.seqs[:limit], true
	}
	return out, nil
}

// list shows a page of sessions as lookup shows each at the clock's time, from one state of the
// store. A session that reached its 3-day limit is closed first, as lookup does, so the list
// never shows it open or under a state it has left. It returns the shown sessions, the storing
// position of the last one, and whether more follow.
func (a API) list(ctx context.Context, states []State, after int64, limit int) ([]View, int64, bool, error) {
	for {
		var views []View
		var last int64
		var more, due bool
		err := a.Store.View(ctx, func(q store.Querier) error {
			p, err := page(ctx, q, states, after, limit)
			if err != nil {
				return err
			}
			now := a.Clock.Now()
			views = make([]View, len(p.sessions))
			for i, s := range p.sessions {
				if pastLimit(s, now) {
					due = true
					return nil
				}
				if views[i], err = view(ctx, q, s, now); err != nil {
					return err
				}
			}
			if len(p.seqs) > 0 {
				last = p.seqs[len(p.seqs)-1]
			}
			more = p.more
			return nil
		})
		if err != nil || !due {
			return views, last, more, err
		}
		// Every round closes at least the session found due, and a closed session is never due
		// again, so this ends.
		if _, err := a.CloseDue(ctx); err != nil {
			return nil, 0, false, err
		}
	}
}

// listSessions serves the admin screen's session list (the PRD's UC7 main flow).
func (a API) listSessions(w http.ResponseWriter, req *http.Request) {
	q, ok := httpapi.Query(w, req)
	if !ok {
		return
	}
	p, ok := httpapi.PageFrom(w, q, sessionsList)
	if !ok {
		return
	}
	states, ok := parseStates(q)
	if !ok {
		httpapi.WriteError(w, http.StatusBadRequest, httpapi.CodeInvalidRequest,
			"state must be one or more SessionState values, separated by commas.", "state")
		return
	}
	views, last, more, err := a.list(req.Context(), states, p.After, p.Limit)
	if errors.Is(err, errUnknownCursor) {
		httpapi.InvalidCursor(w)
		return
	}
	if err != nil {
		a.Log.Error("sessions not listed", "error", err)
		httpapi.WriteInternal(w)
		return
	}
	body := struct {
		Sessions   []View  `json:"sessions"`
		NextCursor *string `json:"next_cursor"`
	}{Sessions: views}
	if more {
		c := httpapi.Cursor(sessionsList, last)
		body.NextCursor = &c
	}
	httpapi.WriteJSON(w, http.StatusOK, body)
}
