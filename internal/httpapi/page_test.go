package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func parsePage(query string) (Page, *httptest.ResponseRecorder, bool) {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/things?"+query, nil)
	p, ok := ParsePage(w, req, "things")
	return p, w, ok
}

func assertBadField(t *testing.T, w *httptest.ResponseRecorder, field string) {
	t.Helper()
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
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Error.Code != "invalid_request" || body.Error.Details.Field != field {
		t.Errorf("body = %s; want invalid_request with details.field %s", w.Body, field)
	}
}

func TestPageDefaults(t *testing.T) {
	p, _, ok := parsePage("")
	if !ok || p != (Page{Limit: DefaultLimit}) {
		t.Errorf("page = %+v, %v; want the first page of %d", p, ok, DefaultLimit)
	}
}

func TestLimitRange(t *testing.T) {
	for _, l := range []string{"1", "100"} {
		if p, _, ok := parsePage("limit=" + l); !ok || p.Limit == 0 {
			t.Errorf("limit=%s refused", l)
		}
	}
	for _, l := range []string{"0", "101", "-1", "", "ten", "1.5", "99999999999999999999"} {
		t.Run(l, func(t *testing.T) {
			_, w, ok := parsePage("limit=" + url.QueryEscape(l))
			if ok {
				t.Fatal("accepted")
			}
			assertBadField(t, w, "limit")
		})
	}
}

func TestCursorRoundTrip(t *testing.T) {
	p, _, ok := parsePage("cursor=" + Cursor("things", 42))
	if !ok || p.After != 42 {
		t.Errorf("page = %+v, %v; want After 42", p, ok)
	}
}

func TestMalformedCursorIsRefused(t *testing.T) {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	cases := map[string]string{
		"empty":           "",
		"not base64":      "!!!",
		"padded":          enc("things:42") + "=",
		"other list":      Cursor("alerts", 42),
		"no position":     enc("things"),
		"zero":            enc("things:0"),
		"negative":        enc("things:-1"),
		"leading zero":    enc("things:042"),
		"plus sign":       enc("things:+42"),
		"not a number":    enc("things:x"),
		"too long":        strings.Repeat("A", 257),
		"plain position":  "42",
		"overflowing int": enc("things:99999999999999999999"),
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, w, ok := parsePage("cursor=" + url.QueryEscape(c))
			if ok {
				t.Fatal("accepted")
			}
			assertBadField(t, w, "cursor")
		})
	}
}
