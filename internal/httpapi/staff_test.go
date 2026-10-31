package httpapi

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/sumiredc/barpay/internal/staff"
)

func staffRouter(called *bool, got *staff.ID) *Router {
	r := NewRouter(keys)
	r.Admin("GET /probe", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		*called, *got = true, StaffID(req)
		w.WriteHeader(http.StatusNoContent)
	}))
	r.Store("GET /probe", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	return r
}

var adminAuth = "Bearer " + keys.Admin.Reveal()

func TestAdminRefusesMissingOrMalformedStaffID(t *testing.T) {
	cases := map[string][]string{
		"missing":       nil,
		"empty":         {""},
		"space":         {"a b"},
		"too long":      {strings.Repeat("a", 65)},
		"non-ASCII":     {"stäff"},
		"other symbols": {"staff@1"},
	}
	for name, ids := range cases {
		t.Run(name, func(t *testing.T) {
			var called bool
			var got staff.ID
			w := doStaff(staffRouter(&called, &got), "/v1/admin/probe", adminAuth, ids)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", w.Code)
			}
			var body struct {
				Error struct {
					Code    string `json:"code"`
					Details struct {
						Field string `json:"field"`
					} `json:"details"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil ||
				body.Error.Code != "invalid_request" || body.Error.Details.Field != "BarPay-Staff-Id" {
				t.Errorf("body = %s; want invalid_request with details.field BarPay-Staff-Id", w.Body)
			}
			if called {
				t.Error("the handler was called")
			}
		})
	}
}

func TestAdminPassesValidStaffID(t *testing.T) {
	for _, id := range []string{"a", strings.Repeat("Z", 64), "staff_01-A"} {
		var called bool
		var got staff.ID
		w := doStaff(staffRouter(&called, &got), "/v1/admin/probe", adminAuth, []string{id})
		if w.Code != http.StatusNoContent || got != staff.ID(id) {
			t.Errorf("%q: status = %d, StaffID = %q", id, w.Code, got)
		}
	}
}

// The key is checked first: without the admin API key the answer is 401, whatever the header.
func TestKeyIsCheckedBeforeStaffID(t *testing.T) {
	var called bool
	var got staff.ID
	if w := doStaff(staffRouter(&called, &got), "/v1/admin/probe", "Bearer "+keys.Store.Reveal(), nil); w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestStaffIDIsNotRequiredOutsideAdmin(t *testing.T) {
	var called bool
	var got staff.ID
	if w := doStaff(staffRouter(&called, &got), "/v1/probe", "Bearer "+keys.Store.Reveal(), nil); w.Code != http.StatusNoContent {
		t.Errorf("store route without a staff ID: status = %d, want 204", w.Code)
	}
}

// Every operation under /admin in the spec documents the 400 that a bad BarPay-Staff-Id gets.
func TestSpecAdminOperationsHave400(t *testing.T) {
	f, err := os.Open("../../docs/api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	pathLine := regexp.MustCompile(`^  (/\S*):\s*$`)
	opLine := regexp.MustCompile(`^    (get|put|post|patch|delete):\s*$`)
	var path, op string
	var has400 bool
	ops := 0
	check := func() {
		if op != "" && strings.HasPrefix(path, "/admin") {
			ops++
			if !has400 {
				t.Errorf("%s %s has no '400' response", strings.ToUpper(op), path)
			}
		}
		op, has400 = "", false
	}
	sc := bufio.NewScanner(f)
	inPaths := false
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "paths:":
			inPaths = true
		case inPaths && line != "" && !strings.HasPrefix(line, " "):
			check()
			inPaths = false
		case !inPaths:
		case pathLine.MatchString(line):
			check()
			path = pathLine.FindStringSubmatch(line)[1]
		case opLine.MatchString(line):
			check()
			op = opLine.FindStringSubmatch(line)[1]
		case strings.TrimSpace(line) == "'400':":
			has400 = true
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if ops < 13 {
		t.Errorf("found %d admin operations; the scan of openapi.yaml is broken", ops)
	}
}
