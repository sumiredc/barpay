package httpapi

import (
	"errors"
	"net/http"

	"github.com/sumiredc/barpay/internal/pricing"
)

// decisionsList names the decision log in its cursors.
const decisionsList = "decisions"

type inputsJSON struct {
	Occupancy   float64  `json:"occupancy"`
	Liveliness  *float64 `json:"liveliness"`
	StaffEvents []string `json:"staff_events"`
	Goal        int      `json:"goal"`
}

type decisionJSON struct {
	DecidedAt   string            `json:"decided_at"`
	Trigger     pricing.Trigger   `json:"trigger"`
	StaffID     string            `json:"staff_id,omitempty"`
	Inputs      *inputsJSON       `json:"inputs"`
	Proposal    *Money            `json:"proposal"`
	Reasoning   *string           `json:"reasoning"`
	Published   ratePointJSON     `json:"published"`
	Explanation string            `json:"explanation"`
	Fallback    *pricing.Fallback `json:"fallback"`
}

func toDecision(d pricing.Decision) decisionJSON {
	j := decisionJSON{
		DecidedAt: FormatTime(d.DecidedAt), Trigger: d.Trigger, StaffID: d.Staff, Reasoning: d.Reasoning,
		Published: toRatePoint(d.Published), Explanation: d.Explanation,
	}
	if in := d.Inputs; in != nil {
		events := in.StaffEvents
		if events == nil {
			events = []string{}
		}
		j.Inputs = &inputsJSON{Occupancy: in.Occupancy, Liveliness: in.Liveliness, StaffEvents: events, Goal: in.Goal}
	}
	if d.Proposal != nil {
		m := ToMoney(*d.Proposal)
		j.Proposal = &m
	}
	if d.Fallback != "" {
		fb := d.Fallback
		j.Fallback = &fb
	}
	return j
}

// listDecisions serves the decision log, newest first.
func (p Pricing) listDecisions(w http.ResponseWriter, req *http.Request) {
	page, ok := ParsePage(w, req, decisionsList)
	if !ok {
		return
	}
	ds, more, err := pricing.ListDecisions(req.Context(), p.Store.Read(), page.After, page.Limit)
	if errors.Is(err, pricing.ErrUnknownCursor) {
		InvalidCursor(w)
		return
	}
	if err != nil {
		WriteInternal(w)
		return
	}
	body := struct {
		Decisions  []decisionJSON `json:"decisions"`
		NextCursor *string        `json:"next_cursor"`
	}{Decisions: make([]decisionJSON, len(ds))}
	for i, d := range ds {
		body.Decisions[i] = toDecision(d)
	}
	if more {
		c := Cursor(decisionsList, ds[len(ds)-1].Seq)
		body.NextCursor = &c
	}
	WriteJSON(w, http.StatusOK, body)
}
