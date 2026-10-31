package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"

	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/pricing"
	"github.com/sumiredc/barpay/internal/settings"
)

// Limits of the spec's Settings and Money schemas: a value outside them does not fit the
// schema and is a 400; the rules the schema cannot state are settings.Validate's, a 422.
const (
	maxMoney        = money.MaxValue
	maxBlockMinutes = 240
	maxPauseMinutes = 240
	maxGoal         = 100
)

type settingsJSON struct {
	Floor        Money `json:"floor"`
	Ceiling      Money `json:"ceiling"`
	BlockMinutes int   `json:"block_minutes"`
	PauseMinutes int   `json:"pause_minutes"`
	CapPerVisit  Money `json:"cap_per_visit"`
	Goal         int   `json:"goal"`
}

func toSettings(s settings.Settings) settingsJSON {
	return settingsJSON{
		Floor: ToMoney(s.Floor), Ceiling: ToMoney(s.Ceiling), BlockMinutes: s.BlockMinutes,
		PauseMinutes: s.PauseMinutes, CapPerVisit: ToMoney(s.CapPerVisit), Goal: s.Goal,
	}
}

func (p Pricing) getSettings(w http.ResponseWriter, req *http.Request) {
	s, ok, err := settings.Load(req.Context(), p.Store.Read())
	if err != nil || !ok {
		WriteInternal(w)
		return
	}
	WriteJSON(w, http.StatusOK, toSettings(s))
}

func (p Pricing) updateSettings(w http.ResponseWriter, req *http.Request) {
	s, field, err := decodeSettings(http.MaxBytesReader(w, req.Body, MaxBodyBytes))
	if err != nil {
		WriteError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error(), field)
		return
	}
	saved, err := pricing.UpdateSettings(req.Context(), p.Store, StaffID(req), s, p.Clock.Now())
	var es settings.Errors
	switch {
	case errors.As(err, &es):
		WriteError(w, http.StatusUnprocessableEntity, CodeInvalidSettings, es.Error(), es[0].Field)
		return
	case err != nil:
		WriteInternal(w)
		return
	}
	WriteJSON(w, http.StatusOK, toSettings(saved))
}

// The request body with every field optional, so that a missing one is told from a zero.
type moneyIn struct {
	Value    *int64  `json:"value"`
	Currency *string `json:"currency"`
}

type settingsIn struct {
	Floor        *moneyIn `json:"floor"`
	Ceiling      *moneyIn `json:"ceiling"`
	BlockMinutes *int     `json:"block_minutes"`
	PauseMinutes *int     `json:"pause_minutes"`
	CapPerVisit  *moneyIn `json:"cap_per_visit"`
	Goal         *int     `json:"goal"`
}

// decodeSettings checks the body against the spec's Settings schema. On a mismatch it returns
// the first bad field, in the order of the schema's properties.
func decodeSettings(body io.Reader) (settings.Settings, string, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return settings.Settings{}, "", errors.New("the body cannot be read or is too large")
	}
	if field, err := CheckKeys(data, settingsFields); err != nil {
		return settings.Settings{}, field, err
	}
	var in settingsIn
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields() // CheckKeys refuses them first, with their path
	if err := dec.Decode(&in); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) && te.Field != "" {
			return settings.Settings{}, te.Field, fmt.Errorf("%s has the wrong type", te.Field)
		}
		return settings.Settings{}, "", errors.New("the body is not a JSON Settings object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return settings.Settings{}, "", errors.New("the body has more than one JSON value")
	}

	var s settings.Settings
	var problem error
	var field string
	fail := func(f, format string, args ...any) {
		if problem == nil {
			field, problem = f, fmt.Errorf("%s %s", f, fmt.Sprintf(format, args...))
		}
	}
	amount := func(name string, m *moneyIn) money.Amount {
		switch {
		case m == nil:
			fail(name, "is required")
		case m.Value == nil:
			fail(name+".value", "is required")
		case m.Currency == nil:
			fail(name+".currency", "is required")
		case *m.Value < 0 || *m.Value > maxMoney:
			fail(name+".value", "must be from 0 to %d", maxMoney)
		case !slices.Contains(money.Known, money.Currency(*m.Currency)):
			fail(name+".currency", "must be one of JPY, USD")
		default:
			return money.New(*m.Value, money.Currency(*m.Currency))
		}
		return money.Amount{}
	}
	integer := func(name string, v *int, lo, hi int) int {
		switch {
		case v == nil:
			fail(name, "is required")
		case *v < lo || *v > hi:
			fail(name, "must be from %d to %d", lo, hi)
		default:
			return *v
		}
		return 0
	}
	s.Floor = amount("floor", in.Floor)
	s.Ceiling = amount("ceiling", in.Ceiling)
	s.BlockMinutes = integer("block_minutes", in.BlockMinutes, 1, maxBlockMinutes)
	s.PauseMinutes = integer("pause_minutes", in.PauseMinutes, 0, maxPauseMinutes)
	s.CapPerVisit = amount("cap_per_visit", in.CapPerVisit)
	s.Goal = integer("goal", in.Goal, 0, maxGoal)
	return s, field, problem
}

// settingsFields are the property names of the spec's Settings, and of Money for the fields that hold one.
var settingsFields = Fields{
	"floor": moneyFields, "ceiling": moneyFields, "block_minutes": nil,
	"pause_minutes": nil, "cap_per_visit": moneyFields, "goal": nil,
}
