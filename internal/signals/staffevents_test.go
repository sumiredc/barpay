package signals

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/spectest"
	"github.com/sumiredc/barpay/internal/staff"
)

func (f *fixture) send(t *testing.T, label string, phase Phase) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"label": label, "phase": string(phase)})
	if w := f.do(http.MethodPost, "/v1/admin/staff-events", adminAuth, "staff_1", string(b)); w.Code != http.StatusCreated {
		t.Fatalf("sendStaffEvent %s %s: %d %s", label, phase, w.Code, w.Body)
	}
	f.clk.Advance(time.Second)
}

func (f *fixture) ongoing(t *testing.T) []string {
	t.Helper()
	got, err := OngoingStaffEvents(context.Background(), f.st.Read())
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// An event that starts is an input until the same label ends; other labels are not touched,
// and a label can start again.
func TestStaffEventIsOngoingFromStartsToEnds(t *testing.T) {
	f := newFixture(t)
	if got := f.ongoing(t); len(got) != 0 {
		t.Fatalf("ongoing before any event = %v", got)
	}
	f.send(t, "Live show", Starts)
	f.send(t, "Happy hour", Starts)
	if got := f.ongoing(t); !slices.Equal(got, []string{"Live show", "Happy hour"}) {
		t.Errorf("after two starts: %v", got)
	}
	f.send(t, "Live show", Ends)
	if got := f.ongoing(t); !slices.Equal(got, []string{"Happy hour"}) {
		t.Errorf("after Live show ends: %v", got)
	}
	f.send(t, "live show", Ends) // labels are compared exactly
	f.send(t, "Happy hour", Ends)
	f.send(t, "Happy hour", Ends) // a second ends changes nothing
	if got := f.ongoing(t); len(got) != 0 {
		t.Errorf("after both end: %v", got)
	}
	f.send(t, "Live show", Starts)
	f.restart(t)
	if got := f.ongoing(t); !slices.Equal(got, []string{"Live show"}) {
		t.Errorf("started again, after a restart: %v", got)
	}
}

// An ends with no starts before it leaves nothing ongoing.
func TestStaffEventEndsAlone(t *testing.T) {
	f := newFixture(t)
	f.send(t, "Live show", Ends)
	if got := f.ongoing(t); len(got) != 0 {
		t.Errorf("ongoing = %v", got)
	}
}

// The response is the spec's StaffEvent with recorded_at, and the staff member is recorded with it.
func TestSendStaffEventIsRecordedWithTheStaffMember(t *testing.T) {
	f := newFixture(t)
	w := f.do(http.MethodPost, "/v1/admin/staff-events", adminAuth, "alice-7", `{"label":"Live show","phase":"starts"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	spectest.Valid(t, "StaffEvent", w.Body.Bytes())
	if want := `{"label":"Live show","phase":"starts","recorded_at":"2026-10-31T18:00:00Z"}`; strings.TrimSpace(w.Body.String()) != want {
		t.Errorf("body = %s, want %s", w.Body, want)
	}
	f.restart(t)
	actions, err := staff.Actions(context.Background(), f.st.Read(), "staff_events")
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Staff != "alice-7" || actions[0].Operation != "sendStaffEvent" || !actions[0].At.Equal(t0) {
		t.Errorf("staff actions = %+v", actions)
	}
}

func TestSendStaffEventRefusesBadBodies(t *testing.T) {
	f := newFixture(t)
	for body, field := range map[string]string{
		`{"phase":"starts"}`:            "label",
		`{"label":"","phase":"starts"}`: "label",
		`{"label":"` + strings.Repeat("あ", 101) + `","phase":"starts"}`:               "label",
		`{"label":7,"phase":"starts"}`:                                                "label",
		`{"label":"Live show"}`:                                                       "phase",
		`{"label":"Live show","phase":"started"}`:                                     "phase",
		`{"label":"Live show","phase":null}`:                                          "phase",
		`{"label":"Live show","phase":"starts","recorded_at":"2026-10-31T18:00:00Z"}`: "recorded_at",
		`{"Label":"Live show","phase":"starts"}`:                                      "Label",
		`{"label":"a","label":"b","phase":"starts"}`:                                  "label",
		`[]`: "",
		`{"label":"Live show","phase":"starts"} {}`: "",
		``: "",
	} {
		w := f.do(http.MethodPost, "/v1/admin/staff-events", adminAuth, "staff_1", body)
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Details struct {
					Field string `json:"field"`
				} `json:"details"`
			} `json:"error"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		if w.Code != http.StatusBadRequest || e.Error.Code != "invalid_request" || e.Error.Details.Field != field {
			t.Errorf("%s: %d %s; want 400 invalid_request on %q", body, w.Code, w.Body, field)
		}
	}
	if n := f.count(t, "staff_events"); n != 0 {
		t.Errorf("%d staff events stored", n)
	}
	// 100 characters, not bytes.
	f.send(t, strings.Repeat("あ", 100), Starts)
}

// Only the admin API key, with a staff ID.
func TestSendStaffEventAccess(t *testing.T) {
	f := newFixture(t)
	body := `{"label":"Live show","phase":"starts"}`
	for name, auth := range map[string]string{"no key": "", "store API key": storeAuth, "edge shared secret": edgeAuth} {
		if w := f.do(http.MethodPost, "/v1/admin/staff-events", auth, "staff_1", body); w.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, w.Code)
		}
	}
	w := f.do(http.MethodPost, "/v1/admin/staff-events", adminAuth, "", body)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"field":"BarPay-Staff-Id"`) {
		t.Errorf("no staff ID: %d %s; want 400 on BarPay-Staff-Id", w.Code, w.Body)
	}
	if n := f.count(t, "staff_events"); n != 0 {
		t.Errorf("%d staff events stored", n)
	}
}
