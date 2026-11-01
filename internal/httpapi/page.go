package httpapi

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// The spec's Limit parameter.
const (
	DefaultLimit = 50
	MaxLimit     = 100
)

// maxCursorLen is the spec's Cursor maxLength.
const maxCursorLen = 256

// Page is where a list request starts and how much it returns (the spec's Limit and Cursor).
// Lists are ordered by a positive storing order, newest first.
type Page struct {
	Limit int
	// After is the storing order of the last item of the previous page; the page holds the items
	// stored before it. 0 means the first page.
	After int64
}

// Cursor returns the spec's next_cursor for the list named list, continuing after the item
// stored at seq. A cursor names its list, so one list's cursor is refused by another.
func Cursor(list string, seq int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(list + ":" + strconv.FormatInt(seq, 10)))
}

// ParsePage reads limit and cursor for the list named list. When the query string cannot be read
// (see Query), or either is repeated or malformed, it writes the 400 and returns false. Whether
// After names an item of the list is for the list to check, answering with InvalidCursor when it
// does not.
func ParsePage(w http.ResponseWriter, req *http.Request, list string) (Page, bool) {
	q, ok := Query(w, req)
	if !ok {
		return Page{}, false
	}
	return PageFrom(w, q, list)
}

// PageFrom is ParsePage for a list that reads other parameters too, from q as Query returned it.
func PageFrom(w http.ResponseWriter, q url.Values, list string) (Page, bool) {
	p := Page{Limit: DefaultLimit}
	limit, hasLimit, ok := single(w, q, "limit")
	if !ok {
		return Page{}, false
	}
	if hasLimit {
		n, err := strconv.Atoi(limit)
		if err != nil || n < 1 || n > MaxLimit {
			WriteError(w, http.StatusBadRequest, CodeInvalidRequest,
				"limit must be an integer from 1 to "+strconv.Itoa(MaxLimit)+".", "limit")
			return Page{}, false
		}
		p.Limit = n
	}
	cursor, hasCursor, ok := single(w, q, "cursor")
	if !ok {
		return Page{}, false
	}
	if hasCursor {
		seq, ok := parseCursor(list, cursor)
		if !ok {
			InvalidCursor(w)
			return Page{}, false
		}
		p.After = seq
	}
	return p, true
}

// InvalidCursor writes the 400 for a cursor that is malformed, from another list, or names no item.
func InvalidCursor(w http.ResponseWriter) {
	WriteError(w, http.StatusBadRequest, CodeInvalidRequest,
		"cursor must be a next_cursor this operation returned, passed unchanged.", "cursor")
}

func parseCursor(list, c string) (int64, bool) {
	if c == "" || len(c) > maxCursorLen {
		return 0, false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(c)
	if err != nil {
		return 0, false
	}
	name, n, ok := strings.Cut(string(b), ":")
	if !ok || name != list {
		return 0, false
	}
	seq, err := strconv.ParseInt(n, 10, 64)
	// Only the form Cursor writes: no sign, no leading zeros.
	if err != nil || seq < 1 || strconv.FormatInt(seq, 10) != n {
		return 0, false
	}
	return seq, true
}
