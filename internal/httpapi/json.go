package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/sumiredc/barpay/internal/money"
)

// MaxBodyBytes is the largest request body any operation reads.
const MaxBodyBytes = 64 << 10

// Money is the spec's Money.
type Money struct {
	Value    int64          `json:"value"`
	Currency money.Currency `json:"currency"`
}

func ToMoney(a money.Amount) Money { return Money{Value: a.Value(), Currency: a.Currency()} }

// FormatTime writes t as the spec's date-time, in UTC.
func FormatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// WriteJSON writes v as the response body with status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Fields are the property names of a schema's object. A name whose value is an object, or an
// array of objects, maps to that object's Fields; any other name maps to nil.
type Fields map[string]Fields

// moneyFields are the property names of the spec's Money.
var moneyFields = Fields{"value": nil, "currency": nil}

// MoneyFields returns the Fields of the spec's Money, for a property that holds one.
func MoneyFields() Fields { return moneyFields }

// CheckKeys refuses an object key the schema does not have, compared exactly, and a key that
// appears twice, naming its path such as "floor.x" or "lines[0].name". encoding/json matches keys
// without regard to case and keeps the last of two equal keys, so "Floor" or a second "floor"
// would otherwise be read as the floor, and something in front of BarPay that reads the body
// might see another value than BarPay does. Anything that is not an object is left to the
// decoder to refuse.
func CheckKeys(data []byte, allowed Fields) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return "", nil
	}
	return checkObject(dec, "", allowed)
}

// checkObject reads the rest of an object whose opening brace was read. A syntax error ends the
// check without a field; the decoder then refuses the body.
func checkObject(dec *json.Decoder, prefix string, allowed Fields) (string, error) {
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return "", nil
		}
		key, _ := tok.(string)
		path := prefix + key
		inner, ok := allowed[key]
		switch {
		case !ok:
			return path, fmt.Errorf("%s is not a field of the schema (names are case-sensitive)", path)
		case seen[key]:
			return path, fmt.Errorf("%s appears more than once", path)
		}
		seen[key] = true
		tok, err = dec.Token()
		if err != nil {
			return "", nil
		}
		switch {
		case inner != nil && tok == json.Delim('{'):
			if field, err := checkObject(dec, path+".", inner); err != nil || field != "" {
				return field, err
			}
		case inner != nil && tok == json.Delim('['):
			if field, err := checkArray(dec, path, inner); err != nil || field != "" {
				return field, err
			}
		default:
			if err := skipRest(dec, tok); err != nil {
				return "", nil
			}
		}
	}
	_, _ = dec.Token() // the closing brace
	return "", nil
}

// checkArray reads the rest of an array whose opening bracket was read, checking each object in it.
func checkArray(dec *json.Decoder, path string, allowed Fields) (string, error) {
	for i := 0; dec.More(); i++ {
		tok, err := dec.Token()
		if err != nil {
			return "", nil
		}
		if tok != json.Delim('{') {
			if err := skipRest(dec, tok); err != nil {
				return "", nil
			}
			continue
		}
		if field, err := checkObject(dec, path+"["+strconv.Itoa(i)+"].", allowed); err != nil || field != "" {
			return field, err
		}
	}
	_, _ = dec.Token() // the closing bracket
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
