package evaluation

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/pricing"
)

var answerFields = httpapi.Fields{"rate_per_hour": nil, "reason": nil, "reasoning": nil}

// wholeNumber is a JSON integer with no sign, fraction or exponent.
var wholeNumber = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

// Parse reads the AI's answer as the PRD's Pricing model, Prompt states it, and reports ok ==
// false for anything else, which is an answer that cannot be read as a rate. It never guesses:
// a number in prose, a rate with decimals, a missing or extra field, or a field of the wrong
// type is unreadable. The only leniency is one Markdown code fence around the object, which
// models add often and which carries no value of its own.
func Parse(answer string) (pricing.Proposal, bool) {
	data := []byte(unfence(strings.TrimSpace(answer)))
	if !bytes.HasPrefix(data, []byte("{")) {
		return pricing.Proposal{}, false
	}
	if _, err := httpapi.CheckKeys(data, answerFields); err != nil {
		return pricing.Proposal{}, false
	}
	var in struct {
		Rate      json.RawMessage `json:"rate_per_hour"`
		Reason    *string         `json:"reason"`
		Reasoning *string         `json:"reasoning"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&in); err != nil {
		return pricing.Proposal{}, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return pricing.Proposal{}, false
	}
	if in.Rate == nil || in.Reason == nil || in.Reasoning == nil || !wholeNumber.Match(in.Rate) {
		return pricing.Proposal{}, false
	}
	rate, err := strconv.ParseInt(string(in.Rate), 10, 64)
	if err != nil || rate > money.MaxValue {
		return pricing.Proposal{}, false
	}
	if !pricing.ReadableReason(*in.Reason) {
		return pricing.Proposal{}, false
	}
	if n := utf8.RuneCountInString(*in.Reasoning); strings.TrimSpace(*in.Reasoning) == "" || n > pricing.MaxReasoning {
		return pricing.Proposal{}, false
	}
	return pricing.Proposal{RatePerHour: rate, Reason: strings.TrimSpace(*in.Reason), Reasoning: *in.Reasoning}, true
}

// unfence removes one surrounding ``` or ```json fence, if s is exactly that.
func unfence(s string) string {
	if !strings.HasPrefix(s, "```") || !strings.HasSuffix(s, "```") || len(s) < 6 {
		return s
	}
	first, rest, ok := strings.Cut(s[3:len(s)-3], "\n")
	if first = strings.TrimSpace(first); !ok || (first != "" && first != "json") {
		return s
	}
	return strings.TrimSpace(rest)
}
