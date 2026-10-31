package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sumiredc/barpay/internal/secret"
)

var keys = Keys{Store: secret.New("store-key"), Admin: secret.New("admin-key"), Edge: secret.New("edge-secret")}

// testRouter has one route in each zone and one public route; called records which ran.
func testRouter(called map[string]bool) *Router {
	r := NewRouter(keys)
	handler := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			called[name] = true
			w.WriteHeader(http.StatusNoContent)
		})
	}
	r.Store("GET /probe", handler("store"))
	r.Admin("GET /probe", handler("admin"))
	r.Edge("GET /probe", handler("edge"))
	r.Public("GET /open", handler("public"))
	return r
}

func do(r http.Handler, path, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func assertUnauthorized(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Error.Code != "unauthorized" || body.Error.Message == "" {
		t.Errorf("body = %s; want the spec's Error with code unauthorized", w.Body)
	}
}

// The table of the issue's Access criterion: each key passes in its own zone only.
func TestEachKeyPassesOnlyInItsZone(t *testing.T) {
	routes := map[string]string{"store": "/v1/probe", "admin": "/v1/admin/probe", "edge": "/v1/edge/probe"}
	presented := map[string]string{
		"nothing":      "",
		"wrong value":  "Bearer not-a-key",
		"empty token":  "Bearer ",
		"other scheme": "Basic " + keys.Store.Reveal(),
		"store":        "Bearer " + keys.Store.Reveal(),
		"admin":        "Bearer " + keys.Admin.Reveal(),
		"edge":         "Bearer " + keys.Edge.Reveal(),
	}
	for shown, auth := range presented {
		for zone, path := range routes {
			t.Run(shown+" on "+zone, func(t *testing.T) {
				called := map[string]bool{}
				w := do(testRouter(called), path, auth)
				if shown == zone {
					if w.Code != http.StatusNoContent || !called[zone] {
						t.Errorf("status = %d, handler called = %v; want it to pass", w.Code, called[zone])
					}
					return
				}
				assertUnauthorized(t, w)
				if len(called) != 0 {
					t.Errorf("handlers called: %v; want none", called)
				}
			})
		}
	}
}

func TestSchemeIsCaseInsensitive(t *testing.T) {
	called := map[string]bool{}
	if w := do(testRouter(called), "/v1/probe", "bearer "+keys.Store.Reveal()); w.Code != http.StatusNoContent {
		t.Errorf("status = %d with a lowercase scheme, want 204", w.Code)
	}
}

func TestPublicRouteNeedsNoKey(t *testing.T) {
	called := map[string]bool{}
	if w := do(testRouter(called), "/v1/open", ""); w.Code != http.StatusNoContent || !called["public"] {
		t.Errorf("public route: status = %d, called = %v", w.Code, called["public"])
	}
}

// A path no route was added for is refused without a key, not left open or answered 404.
func TestPathsWithoutRouteNeedTheirZonesKey(t *testing.T) {
	cases := []struct{ path, auth string }{
		{"/v1/nothing", ""},
		{"/", ""},
		{"/v1/admin/nothing", "Bearer " + keys.Store.Reveal()},
		{"/v1/edge/nothing", "Bearer " + keys.Store.Reveal()},
		{"/v1/admin", "Bearer " + keys.Store.Reveal()},
		{"/v1/nothing", "Bearer " + keys.Admin.Reveal()},
		{"/v1/nothing", "Bearer " + keys.Edge.Reveal()},
	}
	for _, c := range cases {
		called := map[string]bool{}
		w := do(testRouter(called), c.path, c.auth)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s with %q: status = %d, want 401", c.path, c.auth, w.Code)
		}
	}
	if w := do(testRouter(map[string]bool{}), "/v1/nothing", "Bearer "+keys.Store.Reveal()); w.Code != http.StatusNotFound {
		t.Errorf("unknown store path with the store key: status = %d, want 404", w.Code)
	}
}

// Paths that are cleaned or escaped into another zone never reach that zone's handler with the wrong key.
func TestPathTricksDoNotCrossZones(t *testing.T) {
	for _, path := range []string{"/v1/x/../admin/probe", "/v1//admin/probe", "/v1/admin/./probe", "/v1/%61dmin/probe", "/v1/edge/../admin/probe"} {
		for _, key := range []secret.Value{keys.Store, keys.Edge} {
			called := map[string]bool{}
			w := do(testRouter(called), path, "Bearer "+key.Reveal())
			if called["admin"] {
				t.Errorf("%s: the admin handler ran with a non-admin key (status %d)", path, w.Code)
			}
		}
	}
}

func TestPublicCannotOpenAdminOrEdge(t *testing.T) {
	for _, p := range []string{"GET /admin/x", "/edge/x", "GET /admin", "GET /{zone}/probe", "/{rest...}", "/", "GET /{$}"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Public(%q) did not panic", p)
				}
			}()
			NewRouter(keys).Public(p, http.NotFoundHandler())
		}()
	}
}

// A wildcard after a literal first segment is allowed, needs no key, and cannot reach another zone.
func TestPublicWildcardAfterLiteralSegment(t *testing.T) {
	called := map[string]bool{}
	r := testRouter(called)
	r.Public("GET /links/{id}", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		called["link "+req.PathValue("id")] = true
		w.WriteHeader(http.StatusNoContent)
	}))
	if w := do(r, "/v1/links/lnk_123", ""); w.Code != http.StatusNoContent || !called["link lnk_123"] {
		t.Errorf("public wildcard route: status = %d, called = %v", w.Code, called)
	}
	for _, path := range []string{"/v1/admin/probe", "/v1/links/../admin/probe", "/v1/edge/probe"} {
		clear(called)
		w := do(r, path, "")
		if w.Code == http.StatusNoContent || len(called) != 0 {
			t.Errorf("%s without a key: status = %d, called = %v; want no handler", path, w.Code, called)
		}
	}
}

func TestErrorDetailsField(t *testing.T) {
	w := httptest.NewRecorder()
	WriteError(w, http.StatusBadRequest, CodeInvalidRequest, "bad", "BarPay-Staff-Id")
	want := `{"error":{"code":"invalid_request","message":"bad","details":{"field":"BarPay-Staff-Id"}}}` + "\n"
	if w.Body.String() != want || w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("body = %q, content type %q", w.Body, w.Header().Get("Content-Type"))
	}
}
