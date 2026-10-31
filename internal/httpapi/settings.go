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
	maxBodyBytes    = 64 << 10
	maxBlockMinutes = 240
	maxPauseMinutes = 240
	maxGoal         = 100
)

type settingsJSON struct {
	Floor        moneyJSON `json:"floor"`
	Ceiling      moneyJSON `json:"ceiling"`
	BlockMinutes int       `json:"block_minutes"`
	PauseMinutes int       `json:"pause_minutes"`
	CapPerVisit  moneyJSON `json:"cap_per_visit"`
	Goal         int       `json:"goal"`
}

func toSettings(s settings.Settings) settingsJSON {
	return settingsJSON{
		Floor: toMoney(s.Floor), Ceiling: toMoney(s.Ceiling), BlockMinutes: s.BlockMinutes,
		PauseMinutes: s.PauseMinutes, CapPerVisit: toMoney(s.CapPerVisit), Goal: s.Goal,
	}
}

func (p Pricing) getSettings(w http.ResponseWriter, req *http.Request) {
	s, ok, err := settings.Load(req.Context(), p.Store.Read())
	if err != nil || !ok {
		writeInternal(w)
		return
	}
	writeJSON(w, http.StatusOK, toSettings(s))
}

func (p Pricing) updateSettings(w http.ResponseWriter, req *http.Request) {
	s, field, err := decodeSettings(http.MaxBytesReader(w, req.Body, maxBodyBytes))
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
		writeInternal(w)
		return
	}
	writeJSON(w, http.StatusOK, toSettings(saved))
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
	if field, err := checkKeys(data); err != nil {
		return settings.Settings{}, field, err
	}
	var in settingsIn
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields() // checkKeys refuses them first, with their path
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

// Property names of the spec's Settings, and of Money for the fields that hold one.
var (
	settingsKeys = []string{"floor", "ceiling", "block_minutes", "pause_minutes", "cap_per_visit", "goal"}
	moneyFields  = []string{"floor", "ceiling", "cap_per_visit"}
	moneyKeys    = []string{"value", "currency"}
)

// checkKeys refuses an object key the schema does not have, compared exactly, and a key that
// appears twice, naming its path such as "floor.x". encoding/json matches keys without regard to
// case and keeps the last of two equal keys, so "Floor" or a second "floor" would otherwise be
// read as the floor, and something in front of BarPay that reads the body might see another
// value than BarPay does. Anything that is not an object is left to the decoder to refuse.
func checkKeys(data []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return "", nil
	}
	return checkObject(dec, "", settingsKeys, true)
}

// checkObject reads the rest of an object whose opening brace was read. A syntax error ends the
// check without a field; the decoder then refuses the body.
func checkObject(dec *json.Decoder, prefix string, allowed []string, top bool) (string, error) {
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return "", nil
		}
		key, _ := tok.(string)
		path := prefix + key
		switch {
		case !slices.Contains(allowed, key):
			return path, fmt.Errorf("%s is not a field of the schema (names are case-sensitive)", path)
		case seen[key]:
			return path, fmt.Errorf("%s appears more than once", path)
		}
		seen[key] = true
		tok, err = dec.Token()
		if err != nil {
			return "", nil
		}
		if top && tok == json.Delim('{') && slices.Contains(moneyFields, key) {
			if field, err := checkObject(dec, path+".", moneyKeys, false); err != nil || field != "" {
				return field, err
			}
			continue
		}
		if err := skipRest(dec, tok); err != nil {
			return "", nil
		}
	}
	_, _ = dec.Token() // the closing brace
	return "", nil
}

// skipRest skips what is left of a value that started with tok.
func skipRest(dec *json.Decoder, tok json.Token) error {
	if d, ok := tok.(json.Delim); !ok || (d != '{' && d != '[') {
		return nil
	}
	for depth := 1; depth > 0; {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
			} else {
				depth--
			}
		}
	}
	return nil
}
