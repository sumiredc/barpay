// Package signals keeps the signals a rate evaluation reads (the PRD's Pricing model, Signals):
// liveliness from the edge device, and occupancy.
package signals

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/sumiredc/barpay/internal/clock"
	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/store"
)

// API serves the signals' operation: sendSignals (edge.yaml).
type API struct {
	Store *store.Store
	Clock clock.Clock
	Log   *slog.Logger
}

// Register adds the operation to r.
func (a API) Register(r *httpapi.Router) {
	r.Edge("POST /signals", http.HandlerFunc(a.sendSignals))
}

var signalsFields = httpapi.Fields{"liveliness": nil}

// sendSignals takes numbers only (the PRD's Guarantees, Privacy): a field edge.yaml does not
// define, or a value that is not a number, is refused and nothing is kept.
func (a API) sendSignals(w http.ResponseWriter, req *http.Request) {
	v, field, msg := decodeSignals(http.MaxBytesReader(w, req.Body, httpapi.MaxBodyBytes))
	if msg != "" {
		httpapi.WriteError(w, http.StatusBadRequest, httpapi.CodeInvalidRequest, msg, field)
		return
	}
	if err := RecordLiveliness(req.Context(), a.Store, v, a.Clock.Now()); err != nil {
		a.Log.Error("liveliness not recorded", "error", err)
		httpapi.WriteInternal(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decodeSignals(body io.Reader) (v float64, field, msg string) {
	data, err := io.ReadAll(body)
	if err != nil {
		return 0, "", "The body cannot be read or is too large."
	}
	if f, err := httpapi.CheckKeys(data, signalsFields); err != nil {
		return 0, f, err.Error() + "."
	}
	var in struct {
		Liveliness json.RawMessage `json:"liveliness"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&in); err != nil {
		return 0, "", "The body is not a JSON Signals object."
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return 0, "", "The body has more than one JSON value."
	}
	if in.Liveliness == nil {
		return 0, "liveliness", "liveliness is required."
	}
	// A JSON number starts with a digit or a minus sign; strings, null, booleans, arrays and
	// objects do not.
	if c := in.Liveliness[0]; c != '-' && (c < '0' || c > '9') {
		return 0, "liveliness", "liveliness must be a number."
	}
	if err := json.Unmarshal(in.Liveliness, &v); err != nil || v < 0 || v > 1 {
		return 0, "liveliness", "liveliness must be a number from 0 to 1."
	}
	return v, "", ""
}
