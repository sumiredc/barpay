package evaluation

import (
	"net/http"

	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/pricing"
)

// API serves requestRateEvaluation (store API).
type API struct {
	Evaluator *Evaluator
}

// Register adds the operation to r.
func (a API) Register(r *httpapi.Router) {
	r.Store("POST /rate-evaluations", http.HandlerFunc(a.requestRateEvaluation))
}

type rateEvaluationJSON struct {
	Rate      httpapi.RatePoint `json:"rate"`
	Evaluated bool              `json:"evaluated"`
	Fallback  *pricing.Fallback `json:"fallback"`
}

func (a API) requestRateEvaluation(w http.ResponseWriter, req *http.Request) {
	res, err := a.Evaluator.Request(req.Context())
	if err != nil {
		httpapi.WriteInternal(w)
		return
	}
	body := rateEvaluationJSON{Rate: httpapi.ToRatePoint(res.Rate), Evaluated: res.Evaluated}
	if res.Fallback != "" {
		fb := res.Fallback
		body.Fallback = &fb
	}
	httpapi.WriteJSON(w, http.StatusOK, body)
}
