package observability_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/paypal/paypaltest"
)

// about is the ID an alert must carry: a session's or a member's, or none.
type about struct{ session, member string }

// The PRD's Guarantees, Observability: each situation it lists raises its alert, which listAlerts
// returns with the session or member it is about; a failed charge and a failed unlock after a scan
// are also sent to the store app. With every secret of the config file set to a known value, none
// of them appears in what listAlerts and listSessions return.
func TestEachSituationIsAnAlertReadThroughTheAdminAPI(t *testing.T) {
	for _, c := range []struct {
		name string
		kind string
		// cause brings the situation about and returns what the alert is about, and the
		// notification the store app must also receive about that session, if any.
		cause func(t *testing.T, b *barpay) (about, string)
	}{
		{"failed charge", "payment_failed", func(t *testing.T, b *barpay) (about, string) {
			b.link(t, "m_1", "payer-1")
			id := b.linkedIn(t, "m_1")
			want(t, "reportOrder", b.order(t, id, "o-1"), http.StatusCreated, nil)
			b.pp.Broke("payer-1", true)
			if unlock, _ := b.scan(t, id); unlock {
				t.Fatal("unlocked on a refused charge")
			}
			return about{session: id}, "session.payment_failed"
		}},
		{"failed unlock", "unlock_failed", func(t *testing.T, b *barpay) (about, string) {
			b.link(t, "m_1", "payer-1")
			id := b.linkedIn(t, "m_1")
			want(t, "reportOrder", b.order(t, id, "o-1"), http.StatusCreated, nil)
			unlock, unlockID := b.scan(t, id)
			if !unlock {
				t.Fatal("not unlocked after the charge")
			}
			want(t, "reportUnlockResult", b.do(t, edgeKey, http.MethodPost, "/v1/edge/unlocks/"+unlockID+"/result",
				`{"unlocked":false}`), http.StatusNoContent, nil)
			return about{session: id}, "gate.unlock_failed"
		}},
		{"register request", "register_requested", func(t *testing.T, b *barpay) (about, string) {
			id := b.checkin(t, "")
			b.requestRegister(t, id)
			return about{session: id}, ""
		}},
		{"rejected late order", "late_order_rejected", func(t *testing.T, b *barpay) (about, string) {
			id := b.checkin(t, "")
			b.requestRegister(t, id)
			want(t, "late reportOrder", b.order(t, id, "o-late"), http.StatusConflict, nil)
			return about{session: id}, ""
		}},
		{"session past the maximum stay", "max_stay_reached", func(t *testing.T, b *barpay) (about, string) {
			id := b.checkin(t, "")
			b.clk.Set(t0.Add(b.cfg.MaxStay))
			if _, err := b.sess.MaxStayDue(context.Background()); err != nil {
				t.Fatal(err)
			}
			return about{session: id}, ""
		}},
		{"notification given up, about a session", "notification_given_up", func(t *testing.T, b *barpay) (about, string) {
			b.app.ack.Store(false)
			id := b.checkin(t, "") // session.started
			giveUp(t, b)
			return about{session: id}, ""
		}},
		{"notification given up, about a member", "notification_given_up", func(t *testing.T, b *barpay) (about, string) {
			b.app.ack.Store(false)
			b.link(t, "m_1", "payer-1") // member.paypal_linked
			giveUp(t, b)
			return about{member: "m_1"}, ""
		}},
		{"failed deletion", "deletion_failing", func(t *testing.T, b *barpay) (about, string) {
			b.link(t, "m_1", "payer-1")
			for range 70 {
				b.pp.Next(paypaltest.DeletePaymentToken, paypaltest.Refuse)
			}
			b.do(t, storeKey, http.MethodDelete, "/v1/members/m_1/saved-paypal", "")
			for m := 1; m <= 60; m++ {
				b.clk.Set(t0.Add(time.Duration(m) * time.Minute))
				if _, err := b.mem.DeleteDue(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			return about{member: "m_1"}, ""
		}},
		{"currency change waiting", "currency_change_pending", func(t *testing.T, b *barpay) (about, string) {
			b.checkin(t, "")
			b.restart(t, configFile(t, money.JPY))
			return about{}, ""
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := start(t)
			want, sent := c.cause(t, b)
			b.app.ack.Store(true)
			b.deliverDue(t)
			alerts, bodies := b.adminReads(t)
			found := 0
			for _, a := range alerts {
				if a.Kind != c.kind {
					continue
				}
				found++
				if deref(a.SessionID) != want.session || deref(a.MemberID) != want.member {
					t.Errorf("%s about session %q, member %q; want %q, %q", a.Kind, deref(a.SessionID), deref(a.MemberID), want.session, want.member)
				}
			}
			if found == 0 {
				t.Errorf("no %s alert in %+v", c.kind, alerts)
			}
			if sent != "" && !b.app.got(t, sent, want.session) {
				t.Errorf("the store app did not receive %s about %s", sent, want.session)
			}
			noSecrets(t, bodies)
		})
	}
}

// giveUp sends every stored notification until it is given up: once at once, and again at the
// last retry time, which counts for every retry time before it.
func giveUp(t *testing.T, b *barpay) {
	t.Helper()
	b.deliverDue(t)
	b.clk.Set(b.clk.Now().Add(24 * time.Hour))
	b.deliverDue(t)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
