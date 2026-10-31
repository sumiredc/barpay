package spectest

import "testing"

// failed records whether a check failed, without failing the test that runs it.
type failed struct {
	testing.TB
	failed bool
}

func (f *failed) Errorf(string, ...any) { f.failed = true }
func (f *failed) Helper()               {}

// The checker follows the spec, $refs included: it accepts a Money and refuses what the
// schema does not allow.
func TestValidFollowsTheSpec(t *testing.T) {
	cases := []struct {
		body string
		ok   bool
	}{
		{`{"value":1250,"currency":"USD"}`, true},
		{`{"value":-1,"currency":"USD"}`, false},
		{`{"value":1250,"currency":"EUR"}`, false},
		{`{"value":1250}`, false},
		{`{"value":1250,"currency":"USD","x":1}`, false},
	}
	for _, c := range cases {
		f := &failed{TB: t}
		Valid(f, "Money", []byte(c.body))
		if f.failed == c.ok {
			t.Errorf("%s: fits = %v, want %v", c.body, !f.failed, c.ok)
		}
	}
}

// Formats are asserted, not only annotated.
func TestValidAssertsFormats(t *testing.T) {
	f := &failed{TB: t}
	Valid(f, "EventBase", []byte(`{"id":"evt_aaaaaaaaaaaaaaaa","type":"x","created_at":"yesterday","sequence":1}`))
	if !f.failed {
		t.Error("a created_at that is not a date-time fits")
	}
}
