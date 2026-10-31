package signals

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"unicode/utf8"

	"github.com/sumiredc/barpay/internal/clock"
	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/store"
)

// API serves the signals' operations: sendStaffEvent (admin API).
type API struct {
	Store *store.Store
	Clock clock.Clock
	Log   *slog.Logger
}

// Register adds the operations to r.
func (a API) Register(r *httpapi.Router) {
	r.Admin("POST /staff-events", http.HandlerFunc(a.sendStaffEvent))
}

type staffEventJSON struct {
	Label      string `json:"label"`
	Phase      Phase  `json:"phase"`
	RecordedAt string `json:"recorded_at"`
}

var staffEventFields = httpapi.Fields{"label": nil, "phase": nil}

func (a API) sendStaffEvent(w http.ResponseWriter, req *http.Request) {
	label, phase, field, msg := decodeStaffEvent(http.MaxBytesReader(w, req.Body, httpapi.MaxBodyBytes))
	if msg != "" {
		httpapi.WriteError(w, http.StatusBadRequest, httpapi.CodeInvalidRequest, msg, field)
		return
	}
	e, err := RecordStaffEvent(req.Context(), a.Store, httpapi.StaffID(req), label, phase, a.Clock.Now())
	if err != nil {
		a.Log.Error("staff event not recorded", "error", err)
		httpapi.WriteInternal(w)
		return
	}
	httpapi.WriteJSON(w, http.StatusCreated, staffEventJSON{Label: e.Label, Phase: e.Phase, RecordedAt: httpapi.FormatTime(e.RecordedAt)})
}

// decodeStaffEvent checks the body against the spec's StaffEvent. recorded_at is set by BarPay,
// so a body that sends it is refused like any field the request does not have.
func decodeStaffEvent(body io.Reader) (label string, phase Phase, field, msg string) {
	data, err := io.ReadAll(body)
	if err != nil {
		return "", "", "", "The body cannot be read or is too large."
	}
	if f, err := httpapi.CheckKeys(data, staffEventFields); err != nil {
		return "", "", f, err.Error() + "."
	}
	var in struct {
		Label *string `json:"label"`
		Phase *string `json:"phase"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&in); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) && te.Field != "" {
			return "", "", te.Field, te.Field + " must be a string."
		}
		return "", "", "", "The body is not a JSON StaffEvent object."
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", "", "", "The body has more than one JSON value."
	}
	switch {
	case in.Label == nil:
		return "", "", "label", "label is required."
	case utf8.RuneCountInString(*in.Label) < 1 || utf8.RuneCountInString(*in.Label) > MaxLabel:
		return "", "", "label", "label must be 1 to 100 characters."
	case in.Phase == nil:
		return "", "", "phase", "phase is required."
	case *in.Phase != string(Starts) && *in.Phase != string(Ends):
		return "", "", "phase", "phase must be starts or ends."
	}
	return *in.Label, Phase(*in.Phase), "", ""
}
