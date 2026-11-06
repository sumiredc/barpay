package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/store"
)

// listName names the notification log in its cursors.
const listName = "notifications"

// logRecord is the spec's NotificationRecord.
type logRecord struct {
	EventID  string
	Type     Type
	Status   Status
	Attempts int
	// CreatedAt is when the event was stored.
	CreatedAt time.Time
	// LastAttemptAt is zero before the first attempt.
	LastAttemptAt time.Time
	// NextAttemptAt is zero once delivered or given up.
	NextAttemptAt time.Time
	// SubjectKind tells whether SubjectID is a session ID or a member ID.
	SubjectKind subjectKind
	SubjectID   string

	seq int64
}

// errUnknownCursor is returned by listRecords for a position that names no stored notification.
var errUnknownCursor = errors.New("notify: no notification at the cursor")

// listRecords returns up to limit records stored before the one at after (0: from the newest),
// newest first, and whether more follow.
func listRecords(ctx context.Context, q store.Querier, after int64, limit int) ([]logRecord, bool, error) {
	if after != 0 {
		var one int
		err := q.QueryRowContext(ctx, "SELECT 1 FROM notifications WHERE seq = ?", after).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, errUnknownCursor
		}
		if err != nil {
			return nil, false, fmt.Errorf("notify: list: %w", err)
		}
	}
	query := `SELECT seq, id, type, subject_kind, subject_id, status, attempts, created_at, last_attempt_at, next_attempt_at
		FROM notifications`
	args := []any{}
	if after != 0 {
		query += ` WHERE seq < ?`
		args = append(args, after)
	}
	query += ` ORDER BY seq DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("notify: list: %w", err)
	}
	defer rows.Close()
	var out []logRecord
	for rows.Next() {
		var r logRecord
		var typ, kind, status string
		var created int64
		var last, next sql.NullInt64
		if err := rows.Scan(&r.seq, &r.EventID, &typ, &kind, &r.SubjectID, &status, &r.Attempts, &created, &last, &next); err != nil {
			return nil, false, fmt.Errorf("notify: list: %w", err)
		}
		r.Type, r.SubjectKind, r.Status, r.CreatedAt = Type(typ), subjectKind(kind), Status(status), time.Unix(0, created).UTC()
		if last.Valid {
			r.LastAttemptAt = time.Unix(0, last.Int64).UTC()
		}
		if next.Valid {
			r.NextAttemptAt = time.Unix(0, next.Int64).UTC()
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("notify: list: %w", err)
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

type recordJSON struct {
	EventID       string  `json:"event_id"`
	Type          Type    `json:"type"`
	SessionID     string  `json:"session_id,omitempty"`
	MemberID      string  `json:"member_id,omitempty"`
	Status        Status  `json:"status"`
	Attempts      int     `json:"attempts"`
	CreatedAt     string  `json:"created_at"`
	LastAttemptAt *string `json:"last_attempt_at"`
	NextAttemptAt *string `json:"next_attempt_at"`
}

type listJSON struct {
	Notifications []recordJSON `json:"notifications"`
	NextCursor    *string      `json:"next_cursor"`
}

// ListHandler serves listNotifications. Add it with httpapi.Router.Admin, which checks the
// admin API key and the staff ID first.
func ListHandler(st *store.Store, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		page, ok := httpapi.ParsePage(w, req, listName)
		if !ok {
			return
		}
		recs, more, err := listRecords(req.Context(), st.Read(), page.After, page.Limit)
		if errors.Is(err, errUnknownCursor) {
			httpapi.InvalidCursor(w)
			return
		}
		if err != nil {
			log.Error("notification log not read", "error", err)
			// The spec's Error has no code for a failure inside BarPay.
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		body := listJSON{Notifications: make([]recordJSON, len(recs))}
		for i, r := range recs {
			j := recordJSON{EventID: r.EventID, Type: r.Type, Status: r.Status, Attempts: r.Attempts, CreatedAt: formatTime(r.CreatedAt)}
			if r.SubjectKind == memberSubject {
				j.MemberID = r.SubjectID
			} else {
				j.SessionID = r.SubjectID
			}
			if !r.LastAttemptAt.IsZero() {
				s := formatTime(r.LastAttemptAt)
				j.LastAttemptAt = &s
			}
			if !r.NextAttemptAt.IsZero() {
				s := formatTime(r.NextAttemptAt)
				j.NextAttemptAt = &s
			}
			body.Notifications[i] = j
		}
		if more {
			c := httpapi.Cursor(listName, recs[len(recs)-1].seq)
			body.NextCursor = &c
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
}
