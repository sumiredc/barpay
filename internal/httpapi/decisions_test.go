package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sumiredc/barpay/internal/pricing"
	"github.com/sumiredc/barpay/internal/spectest"
	"github.com/sumiredc/barpay/internal/store"
	"github.com/sumiredc/barpay/internal/store/storetest"
)

// decided records an evaluation proposing each rate, a minute apart from t0, and a fallback after them.
func decided(t *testing.T, st *store.Store, proposals ...int64) {
	t.Helper()
	ctx := context.Background()
	in := pricing.Inputs{Occupancy: 0.5, Goal: 30}
	err := st.Tx(ctx, func(tx store.Querier) error {
		for i, p := range proposals {
			_, err := pricing.PublishEvaluation(ctx, tx, in, pricing.Proposal{RatePerHour: p, Reason: "Busy", Reasoning: "Because"},
				usd(300), t0.Add(time.Duration(i+1)*time.Minute))
			if err != nil {
				return err
			}
		}
		_, err := pricing.KeepRate(ctx, tx, in, pricing.FallbackUnreadable, t0.Add(time.Hour))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

type decisionsPage struct {
	Decisions  []json.RawMessage `json:"decisions"`
	NextCursor *string           `json:"next_cursor"`
}

func listDecisions(t *testing.T, r http.Handler, query string) decisionsPage {
	t.Helper()
	w := send(r, http.MethodGet, "/v1/admin/decisions"+query, adminKey, "staff_1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	return decode[decisionsPage](t, w)
}

// Newest first, every entry the spec's Decision, after a restart.
func TestListDecisions(t *testing.T) {
	st := started(t)
	decided(t, st, 1300)
	st = storetest.Reopen(t, st)
	page := listDecisions(t, pricingRouter(st), "")
	if len(page.Decisions) != 2 || page.NextCursor != nil {
		t.Fatalf("page = %+v", page)
	}
	for _, d := range page.Decisions {
		spectest.Valid(t, "Decision", d)
	}
	want := []string{
		`{"decided_at":"2026-10-31T19:00:00Z","trigger":"evaluation","inputs":{"occupancy":0.5,"liveliness":null,"goal":30},` +
			`"proposal":null,"reasoning":null,"published":{"rate_per_hour":{"value":1300,"currency":"USD"},"change":{"value":100,"currency":"USD"},"published_at":"2026-10-31T18:01:00Z"},` +
			`"explanation":"The AI's answer could not be read — rate kept at $13.00/h.","fallback":"ai_unreadable",` +
			`"range":{"floor":{"value":600,"currency":"USD"},"ceiling":{"value":2400,"currency":"USD"}}}`,
		`{"decided_at":"2026-10-31T18:01:00Z","trigger":"evaluation","inputs":{"occupancy":0.5,"liveliness":null,"goal":30},` +
			`"proposal":{"value":1300,"currency":"USD"},"reasoning":"Because","published":{"rate_per_hour":{"value":1300,"currency":"USD"},"change":{"value":100,"currency":"USD"},"published_at":"2026-10-31T18:01:00Z"},` +
			`"explanation":"Busy — rate up $1.00 to $13.00/h.","fallback":null,` +
			`"range":{"floor":{"value":600,"currency":"USD"},"ceiling":{"value":2400,"currency":"USD"}}}`,
	}
	for i, d := range page.Decisions {
		if string(d) != want[i] {
			t.Errorf("decision %d =\n%s\nwant\n%s", i, d, want[i])
		}
	}
}

// A decision recorded while staff events were an input keeps its stored JSON, and is listed
// with only the inputs the spec's Decision has.
func TestListDecisionsLeavesOutStaffEventsStoredBefore(t *testing.T) {
	st := started(t)
	decided(t, st, 1300)
	_, err := st.Read().ExecContext(context.Background(),
		`UPDATE decisions SET inputs = '{"occupancy":0.5,"liveliness":null,"staff_events":["Live show"],"goal":30}'`)
	if err != nil {
		t.Fatal(err)
	}
	page := listDecisions(t, pricingRouter(st), "")
	if len(page.Decisions) != 2 {
		t.Fatalf("page = %+v", page)
	}
	for _, d := range page.Decisions {
		spectest.Valid(t, "Decision", d)
		var got struct {
			Inputs json.RawMessage `json:"inputs"`
		}
		if err := json.Unmarshal(d, &got); err != nil {
			t.Fatal(err)
		}
		if want := `{"occupancy":0.5,"liveliness":null,"goal":30}`; string(got.Inputs) != want {
			t.Errorf("inputs = %s, want %s", got.Inputs, want)
		}
	}
}

// A range change is listed with its staff member and no inputs.
func TestListDecisionsShowsARangeChange(t *testing.T) {
	st := started(t)
	r := pricingRouter(st)
	if w := putSettings(r, `{"floor":{"value":1500,"currency":"USD"},"ceiling":{"value":2400,"currency":"USD"},"block_minutes":10,"pause_minutes":30,"cap_per_visit":{"value":3000,"currency":"USD"},"goal":50}`); w.Code != http.StatusOK {
		t.Fatalf("updateSettings: %d %s", w.Code, w.Body)
	}
	page := listDecisions(t, r, "")
	if len(page.Decisions) != 1 {
		t.Fatalf("page = %+v", page)
	}
	spectest.Valid(t, "Decision", page.Decisions[0])
	if s := string(page.Decisions[0]); !strings.Contains(s, `"trigger":"range_change","staff_id":"staff_1","inputs":null`) ||
		!strings.HasSuffix(s, `"range":{"floor":{"value":1500,"currency":"USD"},"ceiling":{"value":2400,"currency":"USD"}},`+
			`"previous_range":{"floor":{"value":600,"currency":"USD"},"ceiling":{"value":2400,"currency":"USD"}}}`) {
		t.Errorf("decision = %s", s)
	}
}

// A fallback decided before any settings change in effect (a clock set back) has range null.
func TestListDecisionsGivesNoRangeWhenNoneWasInEffect(t *testing.T) {
	st := started(t)
	err := st.Tx(context.Background(), func(tx store.Querier) error {
		_, err := pricing.KeepRate(context.Background(), tx, pricing.Inputs{Goal: 50}, pricing.FallbackTimeout, t0.Add(-time.Minute))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	page := listDecisions(t, pricingRouter(st), "")
	if len(page.Decisions) != 1 {
		t.Fatalf("page = %+v", page)
	}
	spectest.Valid(t, "Decision", page.Decisions[0])
	if s := string(page.Decisions[0]); !strings.HasSuffix(s, `"fallback":"ai_timeout","range":null}`) {
		t.Errorf("decision = %s", s)
	}
}

// Pages continue from the last item, and the last page has no cursor.
func TestListDecisionsPages(t *testing.T) {
	st := started(t)
	decided(t, st, 1300, 1400, 1500, 1600)
	r := pricingRouter(st)
	var seen []string
	query := "?limit=2"
	for range 5 {
		page := listDecisions(t, r, query)
		for _, d := range page.Decisions {
			var v struct {
				DecidedAt string `json:"decided_at"`
			}
			_ = json.Unmarshal(d, &v)
			seen = append(seen, v.DecidedAt)
		}
		if page.NextCursor == nil {
			break
		}
		query = "?limit=2&cursor=" + url.QueryEscape(*page.NextCursor)
	}
	want := "2026-10-31T19:00:00Z 2026-10-31T18:04:00Z 2026-10-31T18:03:00Z 2026-10-31T18:02:00Z 2026-10-31T18:01:00Z"
	if got := strings.Join(seen, " "); got != want {
		t.Errorf("decided_at in page order = %s, want %s", got, want)
	}
}

func TestListDecisionsRefusesBadLimitAndCursor(t *testing.T) {
	st := started(t)
	decided(t, st, 1300)
	r := pricingRouter(st)
	for query, field := range map[string]string{
		"?limit=0": "limit", "?limit=101": "limit", "?limit=x": "limit", "?limit=": "limit",
		"?cursor=": "cursor", "?cursor=bm90LWEtY3Vyc29y": "cursor",
		"?cursor=" + Cursor("notifications", 1): "cursor", // another list's
		"?cursor=" + Cursor(decisionsList, 99):  "cursor", // names nothing listed
	} {
		w := send(r, http.MethodGet, "/v1/admin/decisions"+query, adminKey, "staff_1", "")
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"invalid_request"`) ||
			!strings.Contains(w.Body.String(), `"field":"`+field+`"`) {
			t.Errorf("%s: %d %s; want 400 invalid_request on %s", query, w.Code, w.Body, field)
		}
	}
}

// Only the admin API key, with a staff ID.
func TestListDecisionsAccess(t *testing.T) {
	r := pricingRouter(started(t))
	for name, auth := range map[string]string{
		"no key": "", "store API key": storeAuth, "edge shared secret": "Bearer " + keys.Edge.Reveal(),
	} {
		t.Run(name, func(t *testing.T) {
			assertUnauthorized(t, send(r, http.MethodGet, "/v1/admin/decisions", auth, "staff_1", ""))
		})
	}
	w := send(r, http.MethodGet, "/v1/admin/decisions", adminKey, "", "")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"field":"BarPay-Staff-Id"`) {
		t.Errorf("no staff ID: %d %s; want 400 on BarPay-Staff-Id", w.Code, w.Body)
	}
}
