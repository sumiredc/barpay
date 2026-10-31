package main

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"testing"
	"time"
)

// unlockResults returns the results the gate reported, by unlock, in the order of the unlocks;
// "" for one not reported.
func (b *barpay) unlockResults(t *testing.T) []string {
	t.Helper()
	rows, err := b.st.Read().QueryContext(context.Background(), `SELECT coalesce(result, '') FROM gate_unlocks ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// UC5 main flow: the exit token of a session linked to PayPal is charged once and unlocks the gate.
func TestGateChargesALinkedSessionAndUnlocks(t *testing.T) {
	b := newBarPay(t)
	id := b.linkedSession(t, "m_1")
	g := start(t, b, "gate")

	g.typeLine(t, b.exitToken(t, id))
	g.expect(t, "reader: Thank you", "gate: unlocked")

	if c := b.pp.Captures(); len(c) != 1 || c[0].Amount.Value() != 1300 {
		t.Errorf("PayPal captured %+v, want one charge of $13.00", c)
	}
	if s := b.state(t, id); s != "closed" {
		t.Errorf("state %s, want closed", s)
	}
	waitFor(t, func() bool { r := b.unlockResults(t); return len(r) == 1 && r[0] == "unlocked" },
		func() string { return "unlock results " + join(b.unlockResults(t)) })
}

// UC5 alternative and exception flows: without payment the gate stays locked, and the reader shows
// what edge.yaml decides for the token.
func TestGateStaysLockedWithoutPayment(t *testing.T) {
	b := newBarPay(t)
	g := start(t, b, "gate")

	// BarPay issues no exit token to a session without PayPal linked; one shown before the
	// customer chose to pay at the register is still current.
	t.Run("paying at the register", func(t *testing.T) {
		id := b.linkedSession(t, "m_register")
		token := b.exitToken(t, id)
		b.must(t, http.StatusOK, http.MethodPost, "/v1/sessions/"+id+"/register-request", storeKey, "", nil)
		g.typeLine(t, token)
		g.expect(t, "reader: Please pay at the register", "gate: locked")
	})
	t.Run("charge refused", func(t *testing.T) {
		id := b.linkedSession(t, "m_broke")
		b.pp.Broke(payer("m_broke"), true)
		g.typeLine(t, b.exitToken(t, id))
		g.expect(t, "reader: Please see staff", "gate: locked")
		if s := b.state(t, id); s != "payment_failed" {
			t.Errorf("state %s, want payment_failed", s)
		}
	})
	t.Run("unknown token", func(t *testing.T) {
		g.typeLine(t, "not-a-token-0123456789")
		g.expect(t, "reader: Please show a new QR code", "gate: locked")
	})
	t.Run("token no longer current", func(t *testing.T) {
		old := b.exitToken(t, b.linkedSession(t, "m_old"))
		b.clk.Advance(31 * time.Second) // past the exit token refresh of 30s
		g.typeLine(t, old)
		g.expect(t, "reader: Please show a new QR code", "gate: locked")
	})

	if c := b.pp.Captures(); len(c) != 0 {
		t.Errorf("PayPal captured %+v, want nothing", c)
	}
	if r := b.unlockResults(t); len(r) != 0 {
		t.Errorf("unlocks %v, want none", r)
	}
}

// UC5 exception (the charge succeeds but unlocking fails) and UC7 (staff open the gate by hand).
func TestFailedUnlockIsReportedAndStaffOpenTheGateByHand(t *testing.T) {
	b := newBarPay(t)
	id := b.linkedSession(t, "m_1")
	g := start(t, b, "gate", "-fail-unlock")
	g.expect(t, "setting: unlocking fails")

	g.typeLine(t, b.exitToken(t, id))
	g.expect(t, "reader: Thank you", "gate: did not unlock")
	waitFor(t, func() bool { r := b.unlockResults(t); return len(r) == 1 && r[0] == "failed" },
		func() string { return "unlock results " + join(b.unlockResults(t)) })

	var alerts int
	if err := b.st.Read().QueryRowContext(context.Background(),
		`SELECT count(*) FROM alerts WHERE kind = 'unlock_failed' AND session_id = ?`, id).Scan(&alerts); err != nil || alerts != 1 {
		t.Errorf("%d unlock_failed alerts for the session, %v; want 1", alerts, err)
	}
	var log struct {
		Notifications []struct {
			Type      string `json:"type"`
			SubjectID string `json:"subject_id"`
		} `json:"notifications"`
	}
	b.must(t, http.StatusOK, http.MethodGet, "/v1/admin/notifications", adminKey, "", &log)
	found := false
	for _, n := range log.Notifications {
		found = found || n.Type == "gate.unlock_failed"
	}
	if !found {
		t.Errorf("notification log %+v has no gate.unlock_failed", log.Notifications)
	}

	g.typeLine(t, "fail off")
	g.expect(t, "setting: unlocking works")
	// The gate is connected while its call to waitForUnlock waits; between two calls it is not.
	waitFor(t, func() bool {
		code, _ := b.call(t, http.MethodPost, "/v1/admin/gate/unlock", adminKey, `{"session_id":"`+id+`"}`)
		return code == http.StatusAccepted
	}, func() string { return "unlockGate was not accepted" })
	g.expect(t, "gate: unlocked by staff")
	waitFor(t, func() bool { r := b.unlockResults(t); return len(r) == 2 && r[1] == "unlocked" },
		func() string { return "unlock results " + join(b.unlockResults(t)) })
}

// Every request of the tools is to the edge device's API, with the edge device's shared secret and
// nothing else that names a caller.
func TestToolsCallOnlyTheEdgeAPIWithTheSharedSecret(t *testing.T) {
	b := newBarPay(t)
	id := b.linkedSession(t, "m_1")
	g := start(t, b, "gate")
	s := start(t, b, "signals", "-liveliness", "0.4")
	s.expect(t, "liveliness 0.4 sent")
	g.typeLine(t, b.exitToken(t, id))
	g.expect(t, "reader: Thank you", "gate: unlocked")
	waitFor(t, func() bool {
		code, _ := b.call(t, http.MethodPost, "/v1/admin/gate/unlock", adminKey, `{}`)
		return code == http.StatusAccepted
	}, func() string { return "unlockGate was not accepted" })
	g.expect(t, "gate: unlocked by staff")
	waitFor(t, func() bool { return len(b.unlockResults(t)) == 2 && b.unlockResults(t)[1] != "" },
		func() string { return "unlock results " + join(b.unlockResults(t)) })

	ops := map[string]bool{}
	for _, r := range b.devRequests() {
		op := r.Method + " " + r.Path
		if r.Method == http.MethodPost && resultPath.MatchString(r.Path) {
			op = "POST /v1/edge/unlocks/{id}/result"
		}
		ops[op] = true
		if got := r.Header.Get("Authorization"); got != "Bearer "+edgeSecret {
			t.Errorf("%s %s: Authorization is not the edge secret", r.Method, r.Path)
		}
		for _, h := range []string{"BarPay-Staff-Id", "Idempotency-Key", "Cookie"} {
			if r.Header.Get(h) != "" {
				t.Errorf("%s %s sent %s", r.Method, r.Path, h)
			}
		}
	}
	want := []string{"POST /v1/edge/exit-scans", "GET /v1/edge/unlocks/next", "POST /v1/edge/unlocks/{id}/result", "POST /v1/edge/signals"}
	for _, op := range want {
		if !ops[op] {
			t.Errorf("the tools did not call %s", op)
		}
		delete(ops, op)
	}
	if len(ops) != 0 {
		t.Errorf("the tools called %v, outside the edge device's API", ops)
	}
}

var resultPath = regexp.MustCompile(`^/v1/edge/unlocks/glk_[A-Za-z0-9]+/result$`)

// waitFor retries ok for a few seconds, failing the test with what() unless it becomes true.
func waitFor(t *testing.T, ok func() bool, what func() string) {
	t.Helper()
	giveUpAt := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(giveUpAt) {
			t.Fatal(what())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func join(s []string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
