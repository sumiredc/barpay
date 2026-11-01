package httpapi

import (
	"net/http"
	"net/url"
	"strings"
)

// Query reads the request's query string. When a pair in it cannot be read, even one of a
// parameter the operation does not have, it writes the 400 and returns false: req.URL.Query drops
// such a pair, and the caller would get the answer to a request it did not send.
func Query(w http.ResponseWriter, req *http.Request) (url.Values, bool) {
	q, err := url.ParseQuery(req.URL.RawQuery)
	if err == nil {
		return q, true
	}
	name := unreadableName(req.URL.RawQuery)
	msg := "the query string has a pair that cannot be read."
	if name != "" {
		msg = name + " cannot be read: the query string must be percent-encoded and have no ';'."
	}
	WriteError(w, http.StatusBadRequest, CodeInvalidRequest, msg, name)
	return nil, false
}

// unreadableName returns the decoded name of the first pair url.ParseQuery cannot read, or "" when
// that name is empty or cannot be read itself.
func unreadableName(raw string) string {
	for raw != "" {
		var pair string
		pair, raw, _ = strings.Cut(raw, "&")
		name, value, _ := strings.Cut(pair, "=")
		n, nerr := url.QueryUnescape(name)
		_, verr := url.QueryUnescape(value)
		if nerr == nil && verr == nil && !strings.Contains(pair, ";") {
			continue
		}
		if nerr != nil || strings.Contains(name, ";") {
			return ""
		}
		return n
	}
	return ""
}

// single returns the value of the parameter name in q, and whether it is there. When it is given
// more than once it writes the 400 and returns false for ok, since which one counts is not for
// BarPay to guess.
func single(w http.ResponseWriter, q url.Values, name string) (v string, present, ok bool) {
	vs := q[name]
	switch len(vs) {
	case 0:
		return "", false, true
	case 1:
		return vs[0], true, true
	}
	WriteError(w, http.StatusBadRequest, CodeInvalidRequest, name+" must not be given more than once.", name)
	return "", false, false
}
