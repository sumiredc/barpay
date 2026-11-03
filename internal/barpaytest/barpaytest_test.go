package barpaytest

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/secret"
)

// BarPay logs only what went wrong, so the store is closed to make it log. Neither os.Stderr nor
// the default logger may receive it: a log could hold a key or an exit token.
func TestLogsAreDiscarded(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := os.Stderr
	os.Stderr = w
	log.SetOutput(w)
	t.Cleanup(func() { os.Stderr = stderr; log.SetOutput(stderr) })

	b, err := New(context.Background(), Options{
		Dir:       t.TempDir(),
		Keys:      httpapi.Keys{Store: secret.New("k-store"), Admin: secret.New("k-admin"), Edge: secret.New("k-edge")},
		PublicURL: "http://127.0.0.1:1", StoreAppOrigin: "http://store.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Store.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/edge/signals", strings.NewReader(`{"liveliness":0.5}`))
	req.Header.Set("Authorization", "Bearer k-edge")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	b.Handler.ServeHTTP(rec, req)
	_ = b.Close()

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500 from the closed store", rec.Code)
	}
	w.Close()
	if out, _ := io.ReadAll(r); len(out) != 0 {
		// The length only: what was logged could hold a key.
		t.Errorf("logged %d bytes, want nothing", len(out))
	}
}
