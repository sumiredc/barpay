// Package evaluation re-evaluates the rate when the store app asks (the PRD's Pricing model,
// Evaluation on request): it gives the signals and the goal to the local LLM, reads its answer
// strictly, and has the pricing package guard, publish and log the result.
package evaluation

import (
	"encoding/json"

	"github.com/sumiredc/barpay/internal/llm"
	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/pricing"
)

// Situation is everything the prompt states: the rate and its guardrails, and the inputs.
type Situation struct {
	Current, Floor, Ceiling, MaxStep money.Amount // per hour, in one currency
	Inputs                           pricing.Inputs
}

// systemPrompt is the PRD's Pricing model, Prompt: the role, what each input means, and the one
// answer format Parse reads. The guardrails are told as a hint; the pricing package enforces them.
const systemPrompt = `You set the cover charge of a bar: a rate per hour that each customer pays while they stay.

The user message is JSON describing the room now:
- occupancy: checked-in customers' phones divided by the room's capacity, from 0 (empty) to 1 (full).
- liveliness: how lively the room sounds, from 0 (quiet) to 1 (loudest), or null when there is no signal.
- goal: the owner's goal, from 0 "fill the room" (a lower rate draws customers in) to 100 "maximize revenue" (charge what the room will pay).
- current_rate_per_hour, floor, ceiling, max_step and rate_step: amounts per hour in the currency's smallest unit (unit). The rate you propose will be clamped between floor and ceiling and to within max_step of the current rate, then rounded to a multiple of rate_step.

Answer with one JSON object and nothing else: no text before or after it. It has exactly these three fields:
{"rate_per_hour": <the rate you propose, a whole number in the smallest unit, no decimals>, "reason": "<one short plain-English sentence about the room that justifies the rate, at most 120 characters, without numbers and without saying whether the rate goes up or down>", "reasoning": "<how this rate serves the owner's goal given the signals, at most 4000 characters>"}`

type userMessage struct {
	Currency   money.Currency `json:"currency"`
	Unit       string         `json:"unit"`
	Current    int64          `json:"current_rate_per_hour"`
	Floor      int64          `json:"floor"`
	Ceiling    int64          `json:"ceiling"`
	MaxStep    int64          `json:"max_step"`
	RateStep   int64          `json:"rate_step"`
	Goal       int            `json:"goal"`
	Occupancy  float64        `json:"occupancy"`
	Liveliness *float64       `json:"liveliness"`
}

var units = map[money.Currency]string{money.USD: "cents", money.JPY: "yen"}

// Messages builds the prompt for s.
func Messages(s Situation) []llm.Message {
	cur := s.Current.Currency()
	step, _ := money.RateStep(cur)
	u, _ := json.Marshal(userMessage{
		Currency: cur, Unit: units[cur], Current: s.Current.Value(), Floor: s.Floor.Value(),
		Ceiling: s.Ceiling.Value(), MaxStep: s.MaxStep.Value(), RateStep: step.Value(), Goal: s.Inputs.Goal,
		Occupancy: s.Inputs.Occupancy, Liveliness: s.Inputs.Liveliness,
	})
	return []llm.Message{{Role: "system", Content: systemPrompt}, {Role: "user", Content: string(u)}}
}
