package secret

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const marker = "s3cr3t-MARKER-9f1c"

func TestContentDoesNotLeak(t *testing.T) {
	type holder struct {
		Key Value
		Ptr *Value
	}
	v := New(marker)
	h := holder{Key: v, Ptr: &v}

	var outs []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"} {
		outs = append(outs, fmt.Sprintf(verb, v), fmt.Sprintf(verb, h))
	}
	outs = append(outs, fmt.Sprint(v), fmt.Sprintln(h), v.String())

	js, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	outs = append(outs, string(js))

	var buf bytes.Buffer
	for _, hd := range []slog.Handler{slog.NewTextHandler(&buf, nil), slog.NewJSONHandler(&buf, nil)} {
		slog.New(hd).Info("start", "key", v, "holder", h, "ptr", &v)
	}
	outs = append(outs, buf.String())

	for _, out := range outs {
		if strings.Contains(out, marker) {
			t.Errorf("output contains the secret: %s", out)
		}
	}
}

func TestRevealAndDecode(t *testing.T) {
	var v Value
	if err := v.UnmarshalText([]byte(marker)); err != nil {
		t.Fatal(err)
	}
	if v.Reveal() != marker {
		t.Errorf("Reveal() = %q, want the decoded text", v.Reveal())
	}
	if v.IsZero() || !New("").IsZero() {
		t.Error("IsZero must be true only for an empty value")
	}
}

func TestEqual(t *testing.T) {
	if !New("a").Equal(New("a")) || !New("a").EqualString("a") {
		t.Error("equal values must compare equal")
	}
	for _, other := range []string{"b", "", "aa"} {
		if New("a").EqualString(other) {
			t.Errorf("%q compared equal to \"a\"", other)
		}
	}
}
