// Package httpapi serves BarPay's HTTP API and decides which key each path needs (the PRD's Guarantees, Access).
package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/sumiredc/barpay/internal/secret"
	"github.com/sumiredc/barpay/internal/staff"
)

// Path prefixes: the spec's servers (openapi.yaml, edge.yaml).
const (
	BasePath  = "/v1"
	AdminPath = BasePath + "/admin"
	EdgePath  = BasePath + "/edge"
)

// Keys are the three keys from the config file. They are different from one another.
type Keys struct {
	Store secret.Value
	Admin secret.Value
	Edge  secret.Value
}

type zone int

const (
	storeZone zone = iota
	adminZone
	edgeZone
)

// Router requires a key on every path unless a route was added with Public: the admin API key
// under AdminPath, the edge device's shared secret under EdgePath, and the store API key
// everywhere else, including paths with no route. No key passes outside its own zone.
type Router struct {
	keys   Keys
	mux    *http.ServeMux
	public *http.ServeMux
}

// NewRouter returns a Router with no routes.
func NewRouter(keys Keys) *Router {
	return &Router{keys: keys, mux: http.NewServeMux(), public: http.NewServeMux()}
}

// Store adds a route under BasePath for the store API key. pattern is a ServeMux pattern
// relative to BasePath, such as "GET /rate".
func (r *Router) Store(pattern string, h http.Handler) { r.add(storeZone, BasePath, pattern, h) }

// Admin adds a route under AdminPath for the admin API key, such as "GET /settings".
func (r *Router) Admin(pattern string, h http.Handler) { r.add(adminZone, AdminPath, pattern, h) }

// Edge adds a route under EdgePath for the edge device's shared secret.
func (r *Router) Edge(pattern string, h http.Handler) { r.add(edgeZone, EdgePath, pattern, h) }

// Public adds a route under BasePath that needs no key, such as the browser's return from
// PayPal. It panics for a path under AdminPath or EdgePath, which always need their key.
func (r *Router) Public(pattern string, h http.Handler) {
	full := join(BasePath, pattern)
	if zoneOf(routePath(full)) != storeZone {
		panic(fmt.Sprintf("httpapi: %q needs a key and cannot be public", full))
	}
	// The first segment after BasePath decides the zone. A wildcard there (or an empty one, the
	// catch-all "/") could match "admin" or "edge" and serve those paths without their key.
	// Wildcards in later segments stay inside the literal first segment.
	first, _, _ := strings.Cut(strings.TrimPrefix(routePath(pattern), "/"), "/")
	if first == "" || strings.HasPrefix(first, "{") {
		panic(fmt.Sprintf("httpapi: public route %q needs a literal first segment", full))
	}
	r.public.Handle(full, h)
}

func (r *Router) add(z zone, prefix, pattern string, h http.Handler) {
	full := join(prefix, pattern)
	if zoneOf(routePath(full)) != z {
		panic(fmt.Sprintf("httpapi: %q is outside %s", full, prefix))
	}
	if z == adminZone {
		h = requireStaff(h)
	}
	// The zone is checked again on the route, so the handler never runs without its key
	// even if ServeHTTP's check and the mux ever disagree about a path.
	r.mux.Handle(full, r.requireKey(z, h))
}

type staffKey struct{}

// requireStaff refuses an admin request whose staff ID is missing or malformed. It runs after
// the key check, so a caller without the admin API key learns nothing about the header.
func requireStaff(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		id, err := staff.ParseID(req.Header.Get(staff.Header))
		if err != nil {
			WriteError(w, http.StatusBadRequest, CodeInvalidRequest,
				staff.Header+" must be 1 to 64 letters, digits, '_' or '-'.", staff.Header)
			return
		}
		next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), staffKey{}, id)))
	})
}

// StaffID is the staff member acting on an admin request. It is set on every route added with Admin.
func StaffID(req *http.Request) staff.ID {
	id, _ := req.Context().Value(staffKey{}).(staff.ID)
	return id
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if _, pattern := r.public.Handler(req); pattern != "" {
		r.public.ServeHTTP(w, req)
		return
	}
	r.requireKey(zoneOf(req.URL.Path), r.mux).ServeHTTP(w, req)
}

func (r *Router) requireKey(z zone, next http.Handler) http.Handler {
	want := map[zone]secret.Value{storeZone: r.keys.Store, adminZone: r.keys.Admin, edgeZone: r.keys.Edge}[z]
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		got, ok := bearer(req)
		if !ok || want.IsZero() || !want.EqualString(got) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			WriteError(w, http.StatusUnauthorized, CodeUnauthorized,
				"The API key is missing, wrong, or not the key for this operation.", "")
			return
		}
		next.ServeHTTP(w, req)
	})
}

func bearer(req *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(req.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return token, true
}

func zoneOf(p string) zone {
	p = path.Clean("/" + p)
	switch {
	case p == AdminPath || strings.HasPrefix(p, AdminPath+"/"):
		return adminZone
	case p == EdgePath || strings.HasPrefix(p, EdgePath+"/"):
		return edgeZone
	}
	return storeZone
}

// join puts prefix in front of the path of a pattern that may start with a method.
func join(prefix, pattern string) string {
	method, p, ok := strings.Cut(pattern, " ")
	if !ok {
		return prefix + pattern
	}
	return method + " " + prefix + p
}

func routePath(pattern string) string {
	if _, p, ok := strings.Cut(pattern, " "); ok {
		return p
	}
	return pattern
}
