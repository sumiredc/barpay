package main

import (
	"cmp"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Orders: what customers ordered, until staff mark it served. Served is the store's own record,
// kept with the visit; BarPay has no such state.

// Served is who marked an order served, and when.
type Served struct {
	StaffID string    `json:"staff_id"`
	At      time.Time `json:"at"`
}

// servedPage is how many served orders the Served tab shows at a time.
const servedPage = 20

// orderRow is an order of a visit as the orders page draws it.
type orderRow struct {
	SessionID string
	Order
	Customer string
	Served   *Served
	// Sheet is the id of the order's dialog.
	Sheet string
}

// Summary is the first line of the order and how many more there are, such as
// "Draft beer (Pint) ×1 + 1 more".
func (o orderRow) Summary() string {
	if len(o.Lines) == 0 {
		return ""
	}
	s := fmt.Sprintf("%s ×%d", o.Lines[0].Name, o.Lines[0].Quantity)
	if n := len(o.Lines) - 1; n > 0 {
		s += fmt.Sprintf(" + %d more", n)
	}
	return s
}

// Amount is what a line of an order comes to.
func (l OrderLine) Amount() Money {
	return Money{Value: l.UnitPrice.Value * int64(l.Quantity), Currency: l.UnitPrice.Currency}
}

// cursor names a row's place in the newest-first order, for the page after it to start below it.
func (o orderRow) cursor() string {
	return strconv.FormatInt(o.RecordedAt.UnixNano(), 10) + ":" + o.OrderID + ":" + o.SessionID
}

// newerThan sorts the rows newest first; orders of the same instant keep one order across pages.
func newerThan(a, b orderRow) int {
	if c := b.RecordedAt.Compare(a.RecordedAt); c != 0 {
		return c
	}
	return cmp.Or(cmp.Compare(b.SessionID, a.SessionID), cmp.Compare(b.OrderID, a.OrderID))
}

// orderRows lists the orders of every visit, ended or not, newest first, served or not as asked.
// Only orders BarPay recorded are in a visit: one it refused, such as one sent after checkout,
// never is.
func (d *Data) orderRows(served bool) []orderRow {
	var rows []orderRow
	for _, v := range d.Visits {
		for _, o := range v.Orders {
			sv, ok := v.Served[o.OrderID]
			if ok != served {
				continue
			}
			row := orderRow{SessionID: v.SessionID, Order: o, Customer: d.customerOf(v)}
			if ok {
				row.Served = &sv
			}
			rows = append(rows, row)
		}
	}
	slices.SortFunc(rows, newerThan)
	return rows
}

// toServe counts the orders not served yet.
func (d *Data) toServe() int {
	n := 0
	for _, v := range d.Visits {
		for _, o := range v.Orders {
			if _, ok := v.Served[o.OrderID]; !ok {
				n++
			}
		}
	}
	return n
}

// customerOf names whom a visit is for: the member's name, or a guest by when they checked in,
// which tells guests apart at the counter.
func (d *Data) customerOf(v *Visit) string {
	if m := d.Members[v.MemberID]; m != nil && m.Name != "" {
		return m.Name
	}
	if v.Session != nil && !v.Session.CheckedInAt.IsZero() {
		return "Guest, checked in " + v.Session.CheckedInAt.Local().Format("15:04")
	}
	return "Guest"
}

// markServed records that staffID served the order. An order already served keeps who served it
// first and when, so a second press, from another screen or a resent form, changes nothing.
func (d *Data) markServed(sessionID, orderID, staffID string, at time.Time) bool {
	v := d.Visits[sessionID]
	if v == nil || !slices.ContainsFunc(v.Orders, func(o Order) bool { return o.OrderID == orderID }) {
		return false
	}
	if _, ok := v.Served[orderID]; ok {
		return true
	}
	if v.Served == nil {
		v.Served = map[string]Served{}
	}
	v.Served[orderID] = Served{StaffID: staffID, At: at}
	return true
}

// ordersBadge is the side menu's count of the orders to serve.
func (a *App) ordersBadge() (int, string) {
	var n int
	a.store.view(func(d *Data) { n = d.toServe() })
	return n, fmt.Sprintf("%d to serve", n)
}

func (a *App) adminOrders(w http.ResponseWriter, r *http.Request, id string, b Browser) {
	q := r.URL.Query()
	served := q.Get("tab") == "served"
	v := a.adminView(r, id, b, "Orders", "orders")
	data := struct {
		Served  bool
		ToServe int
		Rows    []orderRow
		Next    string
	}{Served: served}
	a.store.view(func(d *Data) {
		data.ToServe = d.toServe()
		data.Rows = d.orderRows(served)
	})
	if served {
		if c, ok := parseCursor(q.Get("before")); ok {
			// The rows after the one the cursor names; that row need not be there any more.
			i := slices.IndexFunc(data.Rows, func(o orderRow) bool { return newerThan(c, o) < 0 })
			if i < 0 {
				i = len(data.Rows)
			}
			data.Rows = data.Rows[i:]
		}
		if len(data.Rows) > servedPage {
			data.Rows = data.Rows[:servedPage]
			data.Next = "/admin/orders?" + url.Values{"tab": {"served"}, "before": {data.Rows[servedPage-1].cursor()}}.Encode()
		}
	}
	for i := range data.Rows {
		data.Rows[i].Sheet = "order-" + strconv.Itoa(i+1)
	}
	v.Data = data
	a.render(w, http.StatusOK, "admin_orders", v)
}

// parseCursor reads the place a cursor names, or false for none or one this app did not make.
func parseCursor(cursor string) (orderRow, bool) {
	parts := strings.SplitN(cursor, ":", 3)
	if len(parts) != 3 {
		return orderRow{}, false
	}
	nano, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return orderRow{}, false
	}
	return orderRow{SessionID: parts[2], Order: Order{OrderID: parts[1], RecordedAt: time.Unix(0, nano)}}, true
}

// adminMarkServed marks an order served from its dialog and goes back to the orders to serve.
// The order is named with its session, as an order ID is unique within its session only.
func (a *App) adminMarkServed(w http.ResponseWriter, r *http.Request, id string, b Browser) {
	sessionID, orderID := r.PathValue("session_id"), r.PathValue("order_id")
	found := false
	err := a.store.update(func(d *Data) error {
		found = d.markServed(sessionID, orderID, b.StaffID, time.Now())
		return nil
	})
	switch {
	case err != nil:
		http.Error(w, "The order could not be marked served. Try again.", http.StatusInternalServerError)
		return
	case !found:
		http.NotFound(w, r)
		return
	}
	redirect(w, r, "/admin/orders")
}
