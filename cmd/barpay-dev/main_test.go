package main

import (
	"context"
	"strings"
	"testing"
)

// The tools read no credential but the edge device's shared secret, and take no flag for a key
// or for BarPay's config file.
func TestToolsReadOnlyTheEdgeSecret(t *testing.T) {
	b := newBarPay(t)
	for _, tool := range []string{"gate", "signals"} {
		t.Run(tool, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var read []string
			getenv := func(k string) string {
				read = append(read, k)
				if k == secretEnv {
					cancel() // stop the tool as soon as it starts
					return edgeSecret
				}
				return ""
			}
			var out syncBuffer
			if c := run(ctx, []string{tool, "-barpay", b.devURL}, getenv, strings.NewReader(""), &out, &out); c != 0 {
				t.Fatalf("exit code %d:\n%s", c, &out)
			}
			if len(read) != 1 || read[0] != secretEnv {
				t.Errorf("read the environment variables %v, want only %s", read, secretEnv)
			}
			for _, flag := range []string{"-config", "-store-api-key", "-admin-api-key", "-key"} {
				var out syncBuffer
				if c := run(context.Background(), []string{tool, flag, "x"}, getenv, strings.NewReader(""), &out, &out); c != 2 {
					t.Errorf("%s: exit code %d, want 2 for a flag the tool does not have", flag, c)
				}
			}
		})
	}
}

func TestToolsDoNotStartWithoutTheSecret(t *testing.T) {
	b := newBarPay(t)
	for _, tool := range []string{"gate", "signals"} {
		var out syncBuffer
		c := run(context.Background(), []string{tool, "-barpay", b.devURL}, func(string) string { return "" }, strings.NewReader(""), &out, &out)
		if c != 2 || !strings.Contains(out.String(), secretEnv) {
			t.Errorf("%s: exit code %d, %q; want 2 naming %s", tool, c, out.String(), secretEnv)
		}
	}
	if r := b.devRequests(); len(r) != 0 {
		t.Errorf("sent %d requests without the secret", len(r))
	}
}
