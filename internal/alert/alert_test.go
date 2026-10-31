package alert

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/store"
	"github.com/sumiredc/barpay/internal/store/storetest"
)

var t0 = time.Date(2026, 10, 31, 21, 0, 0, 0, time.UTC)

func raise(s *store.Store, a Alert) (Alert, error) {
	var got Alert
	err := s.Tx(context.Background(), func(tx store.Querier) error {
		var err error
		got, err = Raise(context.Background(), tx, a)
		return err
	})
	return got, err
}

func count(t *testing.T, s *store.Store) int {
	t.Helper()
	var n int
	if err := s.Read().QueryRowContext(context.Background(), "SELECT count(*) FROM alerts").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRaisedAlertSurvivesRestart(t *testing.T) {
	s := storetest.Open(t)
	want, err := raise(s, Alert{
		Kind: CurrencyChangePending, RaisedAt: t0, SessionID: "ses_8f3k2m9q1x7v4b6n", MemberID: "m_1024",
		Message: "Currency change to JPY waits for a restart with no open session.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^alt_[A-Za-z0-9]{16,32}$`).MatchString(want.ID) {
		t.Errorf("ID %q does not match the spec's Alert.id", want.ID)
	}

	s = storetest.Reopen(t, s)
	got, err := Get(context.Background(), s.Read(), want.ID)
	if err != nil || got != want {
		t.Errorf("after restart Get = %+v, %v; want %+v", got, err, want)
	}
}

func TestEveryKindCanBeRaised(t *testing.T) {
	s := storetest.Open(t)
	ids := map[string]bool{}
	for _, k := range Kinds {
		a, err := raise(s, Alert{Kind: k, RaisedAt: t0, Message: "m"})
		if err != nil {
			t.Errorf("Raise(%s): %v", k, err)
		}
		ids[a.ID] = true
	}
	if len(ids) != len(Kinds) {
		t.Errorf("%d distinct IDs for %d alerts", len(ids), len(Kinds))
	}
}

func TestInvalidAlertIsNotStored(t *testing.T) {
	cases := map[string]Alert{
		"unknown kind":        {Kind: "gate_on_fire", RaisedAt: t0, Message: "m"},
		"no kind":             {RaisedAt: t0, Message: "m"},
		"no time":             {Kind: PaymentFailed, Message: "m"},
		"no message":          {Kind: PaymentFailed, RaisedAt: t0},
		"message too long":    {Kind: PaymentFailed, RaisedAt: t0, Message: strings.Repeat("x", 501)},
		"malformed session":   {Kind: PaymentFailed, RaisedAt: t0, Message: "m", SessionID: "123"},
		"malformed member ID": {Kind: PaymentFailed, RaisedAt: t0, Message: "m", MemberID: "a b"},
	}
	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			s := storetest.Open(t)
			if _, err := raise(s, a); !errors.Is(err, ErrInvalid) {
				t.Errorf("Raise err = %v, want ErrInvalid", err)
			}
			if n := count(t, s); n != 0 {
				t.Errorf("%d alerts stored, want 0", n)
			}
		})
	}
	if _, err := raise(storetest.Open(t), Alert{Kind: PaymentFailed, RaisedAt: t0, Message: strings.Repeat("x", 500)}); err != nil {
		t.Errorf("a 500-character message: %v", err)
	}
}

// The table refuses an unknown kind too, so code that writes around Raise cannot store one.
func TestTableRefusesUnknownKind(t *testing.T) {
	s := storetest.Open(t)
	err := s.Tx(context.Background(), func(tx store.Querier) error {
		_, err := tx.ExecContext(context.Background(), `INSERT INTO alerts (id, kind, raised_at, session_id, member_id, message)
			VALUES ('alt_AAAAAAAAAAAAAAAA', 'gate_on_fire', 0, '', '', 'm')`)
		return err
	})
	if err == nil {
		t.Error("the alerts table stored an unknown kind")
	}
}

func TestGetUnknown(t *testing.T) {
	if _, err := Get(context.Background(), storetest.Open(t).Read(), "alt_AAAAAAAAAAAAAAAA"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get err = %v, want ErrNotFound", err)
	}
}
