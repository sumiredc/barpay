package main

import (
	"slices"
	"strconv"
	"strings"
)

type menuItem struct {
	ID      string       `json:"-"`
	Name    string       `json:"name"`
	Price   int64        `json:"price"` // cents
	Kind    string       `json:"-"`
	Options []menuOption `json:"options"`
}

// menuOption is a choice the customer makes for an item, such as its size. The first choice is
// picked until the customer picks another.
type menuOption struct {
	Label   string       `json:"label"`
	Choices []menuChoice `json:"choices"`
}

type menuChoice struct {
	Label string `json:"label"`
	Extra int64  `json:"extra"` // cents added to the item's price
}

var menuItems = []menuItem{
	{"beer", "Draft beer", 700, "Drinks", []menuOption{{"Size", []menuChoice{{"Glass", 0}, {"Pint", 200}}}}},
	{"highball", "Whisky highball", 800, "Drinks", []menuOption{{"Strength", []menuChoice{{"Regular", 0}, {"Strong", 100}}}}},
	{"gin", "Gin and tonic", 900, "Drinks", nil},
	{"soda", "Ginger ale", 400, "Drinks", nil},
	{"fries", "French fries", 600, "Food", []menuOption{{"Sauce", []menuChoice{{"Ketchup", 0}, {"Mayo", 0}}}}},
	{"edamame", "Edamame", 500, "Food", nil},
	{"karaage", "Fried chicken", 950, "Food", []menuOption{{"Portion", []menuChoice{{"5 pcs", 0}, {"8 pcs", 300}}}}},
}

const menuCurrency = "USD"

// maxQuantity is the most of one item with the same choices that one order takes.
const maxQuantity = 99

// orderLines turns the lines a phone sent into the lines reported to BarPay, or false when one of
// them is not on the menu. A line is "<item ID>:<choice indexes, comma separated>:<quantity>":
// the phone sends no amount, as it is not trusted with one, and the unit price is the menu's.
// BarPay records only a name and an amount per line, so the name carries every choice, also one
// that costs nothing, for the store to know what to serve. Lines of the same item and choices
// become one.
func orderLines(sent []string) ([]OrderLine, bool) {
	var lines []OrderLine
	at := map[string]int{}
	for _, raw := range sent {
		// The parts are counted before they are split, so a long line costs no more than its length.
		parts := strings.SplitN(raw, ":", 4)
		if len(parts) != 3 {
			return nil, false
		}
		i := slices.IndexFunc(menuItems, func(m menuItem) bool { return m.ID == parts[0] })
		if i < 0 {
			return nil, false
		}
		item := menuItems[i]
		var picks []string
		if parts[1] != "" {
			if strings.Count(parts[1], ",")+1 != len(item.Options) {
				return nil, false
			}
			picks = strings.Split(parts[1], ",")
		}
		if len(picks) != len(item.Options) {
			return nil, false
		}
		name, price := item.Name, item.Price
		var labels []string
		// The key is the item and the index of each choice, not the name: two choices may share a
		// label and not a price.
		key := item.ID
		for k, p := range picks {
			n, err := strconv.Atoi(p)
			if err != nil || n < 0 || n >= len(item.Options[k].Choices) {
				return nil, false
			}
			labels = append(labels, item.Options[k].Choices[n].Label)
			price += item.Options[k].Choices[n].Extra
			key += ":" + strconv.Itoa(n)
		}
		if len(labels) > 0 {
			name += " (" + strings.Join(labels, ", ") + ")"
		}
		q, err := strconv.Atoi(parts[2])
		if err != nil || q < 1 || q > maxQuantity {
			return nil, false
		}
		if j, ok := at[key]; ok {
			if lines[j].Quantity += q; lines[j].Quantity > maxQuantity {
				return nil, false
			}
			continue
		}
		at[key] = len(lines)
		lines = append(lines, OrderLine{Name: name, Quantity: q, UnitPrice: Money{price, menuCurrency}})
	}
	return lines, true
}

// menuSection is a heading of the menu with its items, in the order of menuItems.
type menuSection struct {
	Kind  string
	Items []menuItem
}

func menuSections() []menuSection {
	var sections []menuSection
	for _, item := range menuItems {
		if n := len(sections); n == 0 || sections[n-1].Kind != item.Kind {
			sections = append(sections, menuSection{Kind: item.Kind})
		}
		sections[len(sections)-1].Items = append(sections[len(sections)-1].Items, item)
	}
	return sections
}

// customerStateLabel names a session state for the customer, who is told what is happening to the
// visit; the staff pages name the state itself with stateLabel.
func customerStateLabel(state string) string {
	switch state {
	case "checking_out":
		return "Paying with PayPal"
	case "at_register":
		return "At the register"
	case "payment_failed":
		return "Payment failed"
	case "closed":
		return "Paid"
	case "closed_unpaid":
		return "Unpaid"
	}
	return state
}
