package session

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sumiredc/barpay/internal/alert"
	"github.com/sumiredc/barpay/internal/httpapi"
	"github.com/sumiredc/barpay/internal/money"
	"github.com/sumiredc/barpay/internal/store"
)

// Limits of the spec's OrderRequest and OrderLine.
const (
	maxOrderIDLen = 128
	maxLines      = 100
	maxNameLen    = 200
	maxQuantity   = 99

	// maxOrderBytes holds the largest OrderRequest the schema allows, every character of its
	// names written as a JSON escape.
	maxOrderBytes = 1 << 20
)

// maxOrdersTotal is the spec's Total description: the most a session's total may be when an order
// is recorded. It leaves room under money.MaxTotal for the cover charge that accrues afterwards.
const maxOrdersTotal = 9_000_000_000_000_000

var orderIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)

// Line is the spec's OrderLine.
type Line struct {
	Name      string        `json:"name"`
	Quantity  int64         `json:"quantity"`
	UnitPrice httpapi.Money `json:"unit_price"`
}

// orderJSON is the spec's Order.
type orderJSON struct {
	OrderID    string          `json:"order_id"`
	Lines      json.RawMessage `json:"lines"`
	Amount     httpapi.Money   `json:"amount"`
	RecordedAt string          `json:"recorded_at"`
}

// order is an order as stored. lines is the JSON of its lines, written by encodeLines, so two
// reports of an order are the same order exactly when their lines encode the same.
type order struct {
	id         string
	lines      []byte
	amount     int64
	recordedAt time.Time
}

func (o order) toJSON(cur money.Currency) orderJSON {
	return orderJSON{OrderID: o.id, Lines: o.lines, Amount: httpapi.ToMoney(money.New(o.amount, cur)),
		RecordedAt: httpapi.FormatTime(o.recordedAt)}
}

func encodeLines(lines []Line) []byte {
	b, _ := json.Marshal(lines) // a []Line always encodes
	return b
}

// amountOf is the sum of quantity × unit price over lines: at most maxLines × maxQuantity ×
// money.MaxValue, far inside int64.
func amountOf(lines []Line) int64 {
	var sum int64
	for _, l := range lines {
		sum += l.Quantity * l.UnitPrice.Value
	}
	return sum
}

// Outcomes of reportOrder that are not a stored order.
var (
	errOrderConflict    = errors.New("session: the order ID was recorded with other lines")
	errCurrencyMismatch = errors.New("session: an amount is not in the store's currency")
	errNotActive        = errors.New("session: the session is not active")
	errTotalTooLarge    = errors.New("session: the total would be above the largest total")
)

// reportOrder adds an order to a session (the PRD's UC4). The order is checked against the
// session and recorded in one transaction, so it either lands before the total is fixed and is
// in the total, or is refused: it is never added to a total already fixed. The same order_id is
// looked up before the state, so a repeat is answered as recorded even after the session left
// active, and raises no alert.
func (a API) reportOrder(w http.ResponseWriter, req *http.Request) {
	lines, orderID, field, err := decodeOrder(http.MaxBytesReader(w, req.Body, maxOrderBytes))
	if err != nil {
		httpapi.WriteError(w, http.StatusBadRequest, httpapi.CodeInvalidRequest, err.Error(), field)
		return
	}
	ctx := req.Context()
	encoded, amount := encodeLines(lines), amountOf(lines)
	var rec order
	var created bool
	var cur money.Currency
	var refused error // a refusal that changed something, so the transaction commits
	err = a.Store.Tx(ctx, func(tx store.Querier) error {
		// Read inside the transaction, which waits for the write lock, so an order recorded
		// before the total is fixed has a time before it.
		now := a.Clock.Now()
		s, err := get(ctx, tx, req.PathValue("session_id"))
		if err != nil {
			return err
		}
		cur = s.Currency
		var ok bool
		if rec, ok, err = getOrder(ctx, tx, s.ID, orderID); err != nil || ok {
			if ok && !bytes.Equal(rec.lines, encoded) {
				return errOrderConflict
			}
			return err
		}
		for _, l := range lines {
			if l.UnitPrice.Currency != s.Currency {
				return errCurrencyMismatch
			}
		}
		if s.State != Active {
			_, err := alert.Raise(ctx, tx, alert.Alert{
				Kind: alert.LateOrderRejected, RaisedAt: now, SessionID: s.ID,
				Message: fmt.Sprintf("Order %s was reported for a session that is %s, so it was not recorded. "+
					"Collect anything served for it outside BarPay.", orderID, strings.ReplaceAll(string(s.State), "_", " ")),
			})
			refused = errNotActive
			return err
		}
		v, err := view(ctx, tx, s, now)
		if err != nil {
			return err
		}
		if v.Total.Value > maxOrdersTotal-amount {
			return errTotalTooLarge
		}
		rec = order{id: orderID, lines: encoded, amount: amount, recordedAt: now}
		created = true
		return insertOrder(ctx, tx, s.ID, rec)
	})
	if err == nil {
		err = refused
	}
	switch {
	case errors.Is(err, errNotFound):
		writeNotFound(w)
	case errors.Is(err, errOrderConflict):
		httpapi.WriteError(w, http.StatusConflict, codeOrderConflict,
			"This order_id was already recorded with different lines; the recorded order is unchanged.", "")
	case errors.Is(err, errCurrencyMismatch):
		httpapi.WriteError(w, http.StatusUnprocessableEntity, codeCurrencyMismatch,
			"Every unit_price must be in the store's currency ("+string(cur)+").", "")
	case errors.Is(err, errNotActive):
		httpapi.WriteError(w, http.StatusConflict, codeSessionNotActive,
			"The session is not active, so the order was not recorded.", "")
	case errors.Is(err, errTotalTooLarge):
		httpapi.WriteError(w, http.StatusUnprocessableEntity, codeAmountTooLarge,
			"With this order the session's total would be above the largest total BarPay holds.", "")
	case err != nil:
		httpapi.WriteInternal(w)
	case created:
		httpapi.WriteJSON(w, http.StatusCreated, rec.toJSON(cur))
	default:
		httpapi.WriteJSON(w, http.StatusOK, rec.toJSON(cur))
	}
}

func getOrder(ctx context.Context, q store.Querier, sessionID, orderID string) (order, bool, error) {
	o := order{id: orderID}
	var at int64
	err := q.QueryRowContext(ctx, `SELECT lines, amount, recorded_at FROM orders WHERE session_id = ? AND order_id = ?`,
		sessionID, orderID).Scan(&o.lines, &o.amount, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return order{}, false, nil
	}
	if err != nil {
		return order{}, false, fmt.Errorf("session: get order: %w", err)
	}
	o.recordedAt = time.Unix(0, at).UTC()
	return o, true, nil
}

func insertOrder(ctx context.Context, tx store.Querier, sessionID string, o order) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO orders (session_id, order_id, lines, amount, recorded_at) VALUES (?, ?, ?, ?, ?)`,
		sessionID, o.id, o.lines, o.amount, o.recordedAt.UnixNano())
	if err != nil {
		return fmt.Errorf("session: record order: %w", err)
	}
	return nil
}

// orders returns the session's orders' times, oldest first, and their total.
func orders(ctx context.Context, q store.Querier, sessionID string) (times []time.Time, total int64, err error) {
	rows, err := q.QueryContext(ctx, `SELECT amount, recorded_at FROM orders WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, 0, fmt.Errorf("session: orders: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var amount, at int64
		if err := rows.Scan(&amount, &at); err != nil {
			return nil, 0, fmt.Errorf("session: orders: %w", err)
		}
		times = append(times, time.Unix(0, at).UTC())
		total += amount
	}
	return times, total, rows.Err()
}

var orderFields = httpapi.Fields{
	"order_id": nil,
	"lines":    httpapi.Fields{"name": nil, "quantity": nil, "unit_price": httpapi.MoneyFields()},
}

// decodeOrder checks the body against the spec's OrderRequest. On a mismatch it returns the
// first bad field, such as "lines[2].unit_price.value", when there is one.
func decodeOrder(body io.Reader) (lines []Line, orderID, field string, err error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, "", "", errors.New("the body cannot be read or is too large")
	}
	if field, err := httpapi.CheckKeys(data, orderFields); err != nil {
		return nil, "", field, err
	}
	top, err := object(data)
	if err != nil {
		return nil, "", "", errors.New("the body is not a JSON OrderRequest object")
	}
	fail := func(f, format string, args ...any) ([]Line, string, string, error) {
		return nil, "", f, fmt.Errorf("%s %s", f, fmt.Sprintf(format, args...))
	}
	raw, ok := top["order_id"]
	if !ok {
		return fail("order_id", "is required")
	}
	if orderID, ok = str(raw); !ok || len(orderID) > maxOrderIDLen || !orderIDPattern.MatchString(orderID) {
		return fail("order_id", "must be 1 to %d letters, digits, '_', '.', ':' or '-'", maxOrderIDLen)
	}
	raw, ok = top["lines"]
	if !ok {
		return fail("lines", "is required")
	}
	var items []json.RawMessage
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) || json.Unmarshal(raw, &items) != nil {
		return fail("lines", "must be an array")
	}
	if len(items) < 1 || len(items) > maxLines {
		return fail("lines", "must have 1 to %d lines", maxLines)
	}
	for i, item := range items {
		path := "lines[" + strconv.Itoa(i) + "]"
		line, err := object(item)
		if err != nil {
			return fail(path, "must be an object")
		}
		var l Line
		raw, ok := line["name"]
		if !ok {
			return fail(path+".name", "is required")
		}
		if l.Name, ok = str(raw); !ok || l.Name == "" || utf8.RuneCountInString(l.Name) > maxNameLen {
			return fail(path+".name", "must be a string of 1 to %d characters", maxNameLen)
		}
		raw, ok = line["quantity"]
		if !ok {
			return fail(path+".quantity", "is required")
		}
		if l.Quantity, ok = integer(raw); !ok || l.Quantity < 1 || l.Quantity > maxQuantity {
			return fail(path+".quantity", "must be an integer from 1 to %d", maxQuantity)
		}
		raw, ok = line["unit_price"]
		if !ok {
			return fail(path+".unit_price", "is required")
		}
		price, err := object(raw)
		if err != nil {
			return fail(path+".unit_price", "must be a Money object")
		}
		raw, ok = price["value"]
		if !ok {
			return fail(path+".unit_price.value", "is required")
		}
		if l.UnitPrice.Value, ok = integer(raw); !ok || l.UnitPrice.Value < 0 || l.UnitPrice.Value > money.MaxValue {
			return fail(path+".unit_price.value", "must be an integer from 0 to %d", money.MaxValue)
		}
		raw, ok = price["currency"]
		if !ok {
			return fail(path+".unit_price.currency", "is required")
		}
		c, ok := str(raw)
		if !ok || !slices.Contains(money.Known, money.Currency(c)) {
			return fail(path+".unit_price.currency", "must be one of JPY, USD")
		}
		l.UnitPrice.Currency = money.Currency(c)
		lines = append(lines, l)
	}
	return lines, orderID, "", nil
}

// object decodes a JSON object, refusing null, any other value and anything after it.
func object(data []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	var m map[string]json.RawMessage
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("not an object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("more than one JSON value")
	}
	return m, nil
}

// str decodes a JSON string, refusing any other value.
func str(raw json.RawMessage) (string, bool) {
	var s string
	if !bytes.HasPrefix(raw, []byte(`"`)) || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// integer decodes a JSON number whose value is an integer, as the schema's type integer counts
// one: 2, 2.0, 2e0 and 0.2e1 are all 2. The value is read exactly, so 2.0000000000000001 is a
// fraction and refused, as is any other value. A number beyond int64 is refused; every bound it
// is checked against is far inside.
func integer(raw json.RawMessage) (int64, bool) {
	if n, err := strconv.ParseInt(string(raw), 10, 64); err == nil {
		return n, true
	}
	var num json.Number
	if !bytes.ContainsAny(raw[:1], "-0123456789") || json.Unmarshal(raw, &num) != nil {
		return 0, false
	}
	// An exponent far beyond int64 would make the exact value huge to compute.
	if _, exp, ok := strings.Cut(strings.ToLower(num.String()), "e"); ok {
		if e, err := strconv.Atoi(exp); err != nil || e < -maxExponent || e > maxExponent {
			return 0, false
		}
	}
	r, ok := new(big.Rat).SetString(num.String())
	if !ok || !r.IsInt() || !r.Num().IsInt64() {
		return 0, false
	}
	return r.Num().Int64(), true
}

// maxExponent bounds the exponent integer reads, far beyond what any int64 written with an
// exponent needs.
const maxExponent = 40
