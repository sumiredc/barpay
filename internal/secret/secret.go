// Package secret holds values that must never reach logs, notifications, error bodies or API responses.
package secret

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"log/slog"
)

const redacted = "[redacted]"

// Value hides its content from fmt, encoding/json and log/slog; only Reveal returns it.
// It is decoded from text (encoding.TextUnmarshaler), so a config decoder can fill it.
type Value struct {
	v string
}

// New wraps s.
func New(s string) Value { return Value{v: s} }

// Reveal returns the content. Call it only where the value is sent to whom it belongs.
func (s Value) Reveal() string { return s.v }

// IsZero reports whether the value is empty.
func (s Value) IsZero() bool { return s.v == "" }

// Equal compares in constant time with respect to the content of both values.
func (s Value) Equal(o Value) bool {
	// Hashing first makes the comparison independent of the lengths too.
	a, b := sha256.Sum256([]byte(s.v)), sha256.Sum256([]byte(o.v))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// EqualString is Equal against a value received from outside, such as a request header.
func (s Value) EqualString(o string) bool { return s.Equal(New(o)) }

func (s Value) String() string   { return redacted }
func (s Value) GoString() string { return redacted }

// Format covers every verb, including %#v and %x, which would otherwise print the content.
func (s Value) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redacted)) }

func (s Value) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }
func (s Value) MarshalText() ([]byte, error) { return []byte(redacted), nil }
func (s Value) LogValue() slog.Value         { return slog.StringValue(redacted) }

func (s *Value) UnmarshalText(b []byte) error {
	s.v = string(b)
	return nil
}
