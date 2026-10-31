package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/store"
	"github.com/sumiredc/barpay/internal/store/storetest"
)

var t0 = time.Date(2026, 10, 31, 21, 0, 0, 0, time.UTC)

const (
	sesA = "ses_8f3k2m9q1x7v4b6n"
	sesB = "ses_2b7x9q4m1k8v3n6f"
)

type fakeSession struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

func sessionEvent(typ Type, id string) Event {
	return Event{Type: typ, SubjectID: id, Data: map[string]any{"session": fakeSession{ID: id, State: "active"}}}
}

func enqueue(t *testing.T, s *store.Store, at time.Time, ev Event) Stored {
	t.Helper()
	var got Stored
	err := s.Tx(context.Background(), func(tx store.Querier) error {
		var err error
		got, err = Enqueue(context.Background(), tx, at, ev)
		return err
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	return got
}

func storedBody(t *testing.T, s *store.Store, id string) []byte {
	t.Helper()
	var b []byte
	if err := s.Read().QueryRowContext(context.Background(), "SELECT body FROM notifications WHERE id = ?", id).Scan(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

func countNotifications(t *testing.T, s *store.Store) int {
	t.Helper()
	var n int
	if err := s.Read().QueryRowContext(context.Background(), "SELECT count(*) FROM notifications").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

var eventIDPattern = regexp.MustCompile(`^evt_[A-Za-z0-9]{16,32}$`)

// The stored body is the spec's EventBase with the event's data.
func TestStoredBodyIsTheSpecsEvent(t *testing.T) {
	s := storetest.Open(t)
	got := enqueue(t, s, t0.Add(1500*time.Millisecond), sessionEvent(SessionStarted, sesA))

	var b struct {
		ID        string          `json:"id"`
		Type      string          `json:"type"`
		CreatedAt string          `json:"created_at"`
		Sequence  int64           `json:"sequence"`
		Data      json.RawMessage `json:"data"`
	}
	raw := storedBody(t, s, got.ID)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		t.Fatalf("body %s: %v", raw, err)
	}
	if !eventIDPattern.MatchString(b.ID) || b.ID != got.ID {
		t.Errorf("id = %q, Enqueue returned %q; want one ID fitting the spec's pattern", b.ID, got.ID)
	}
	if b.Type != "session.started" || b.Sequence != 1 || b.CreatedAt != "2026-10-31T21:00:01.5Z" {
		t.Errorf("type, sequence, created_at = %q, %d, %q", b.Type, b.Sequence, b.CreatedAt)
	}
	if string(b.Data) != `{"session":{"id":"`+sesA+`","state":"active"}}` {
		t.Errorf("data = %s", b.Data)
	}
}

// Sequence numbers start at 1 and go up by 1 per subject; each subject counts on its own, and
// a session and a member with the same ID are different subjects.
func TestSequencePerSubject(t *testing.T) {
	s := storetest.Open(t)
	member := Event{Type: MemberPaypalLinked, SubjectID: sesA, Data: map[string]any{"member": map[string]any{"id": sesA}}}
	steps := []struct {
		ev   Event
		want int64
	}{
		{sessionEvent(SessionStarted, sesA), 1},
		{sessionEvent(SessionStarted, sesB), 1},
		{sessionEvent(SessionPaypalLinked, sesA), 2},
		{member, 1},
		{sessionEvent(GateUnlockFailed, sesA), 3}, // gate.* counts with the session
		{sessionEvent(SessionClosed, sesB), 2},
		{member, 2},
		{sessionEvent(SessionClosed, sesA), 4},
	}
	for i, st := range steps {
		if got := enqueue(t, s, t0, st.ev); got.Sequence != st.want {
			t.Errorf("step %d (%s %s): sequence = %d, want %d", i, st.ev.Type, st.ev.SubjectID, got.Sequence, st.want)
		}
	}
}

// Two events stored at the same moment for one subject never share a number.
func TestConcurrentEventsGetDistinctSequences(t *testing.T) {
	s := storetest.Open(t)
	const rounds, writers = 20, 4
	var mu sync.Mutex
	var seqs []int64
	for range rounds {
		var wg sync.WaitGroup
		for range writers {
			wg.Go(func() {
				var got Stored
				err := s.Tx(context.Background(), func(tx store.Querier) error {
					var err error
					got, err = Enqueue(context.Background(), tx, t0, sessionEvent(SessionPaypalLinked, sesA))
					return err
				})
				if err != nil {
					t.Errorf("Enqueue: %v", err)
					return
				}
				mu.Lock()
				seqs = append(seqs, got.Sequence)
				mu.Unlock()
			})
		}
		wg.Wait()
	}
	slices.Sort(seqs)
	for i, n := range seqs {
		if n != int64(i+1) {
			t.Fatalf("sequences = %v; want 1 to %d, each once", seqs, rounds*writers)
		}
	}
}

// The unique constraint is the last line of defence against a duplicate sequence.
func TestStoreRefusesDuplicateSequence(t *testing.T) {
	s := storetest.Open(t)
	first := enqueue(t, s, t0, sessionEvent(SessionStarted, sesA))
	err := s.Tx(context.Background(), func(tx store.Querier) error {
		_, err := tx.ExecContext(context.Background(), `INSERT INTO notifications
			(id, type, subject_kind, subject_id, sequence, body, created_at, status, next_attempt_at)
			VALUES ('evt_other0000000000000000', 'session.closed', 'session', ?, ?, x'7b7d', 0, 'retrying', 0)`,
			sesA, first.Sequence)
		return err
	})
	if err == nil {
		t.Error("a second notification with the same subject and sequence was stored")
	}
}

// An event stored in a transaction that rolls back leaves nothing behind.
func TestRolledBackEventIsNotStored(t *testing.T) {
	s := storetest.Open(t)
	abort := errors.New("the change failed")
	err := s.Tx(context.Background(), func(tx store.Querier) error {
		if _, err := Enqueue(context.Background(), tx, t0, sessionEvent(SessionStarted, sesA)); err != nil {
			return err
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatalf("Tx = %v", err)
	}
	if n := countNotifications(t, s); n != 0 {
		t.Errorf("%d notifications stored, want 0", n)
	}
	// The number was not used up either.
	if got := enqueue(t, s, t0, sessionEvent(SessionStarted, sesA)); got.Sequence != 1 {
		t.Errorf("sequence after a rollback = %d, want 1", got.Sequence)
	}
}

func TestInvalidEventsAreRefused(t *testing.T) {
	cases := map[string]Event{
		"unknown type":            {Type: "session.reopened", SubjectID: sesA, Data: map[string]any{}},
		"malformed session ID":    {Type: SessionStarted, SubjectID: "m_1024", Data: map[string]any{}},
		"member ID too long":      {Type: MemberPaypalLinked, SubjectID: string(make([]byte, 65)), Data: map[string]any{}},
		"empty member ID":         {Type: MemberSavedPaypalDeleted, SubjectID: "", Data: map[string]any{}},
		"data is not an object":   {Type: SessionStarted, SubjectID: sesA, Data: []int{1}},
		"data is null":            {Type: SessionStarted, SubjectID: sesA},
		"data does not encode":    {Type: SessionStarted, SubjectID: sesA, Data: map[string]any{"f": func() {}}},
		"gate event for a member": {Type: GateUnlockFailed, SubjectID: "m_1024", Data: map[string]any{}},
	}
	for name, ev := range cases {
		t.Run(name, func(t *testing.T) {
			s := storetest.Open(t)
			err := s.Tx(context.Background(), func(tx store.Querier) error {
				_, err := Enqueue(context.Background(), tx, t0, ev)
				return err
			})
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("Enqueue = %v, want ErrInvalid", err)
			}
			if n := countNotifications(t, s); n != 0 {
				t.Errorf("%d notifications stored", n)
			}
		})
	}
}
