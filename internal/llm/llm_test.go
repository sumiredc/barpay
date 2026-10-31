package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func server(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

// The request is the OpenAI-compatible chat completion: the model, the messages in order,
// temperature 0 and no streaming, posted to <address>/chat/completions.
func TestChatSendsAChatCompletionRequest(t *testing.T) {
	var got map[string]any
	var path, contentType string
	s := server(t, func(w http.ResponseWriter, r *http.Request) {
		path, contentType = r.Method+" "+r.URL.Path, r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hello"}}]}`)
	})
	for _, address := range []string{s.URL + "/v1", s.URL + "/v1/"} {
		out, err := New(address, "m-1").Chat(context.Background(), []Message{{"system", "s"}, {"user", "u"}})
		if err != nil || out != "hello" {
			t.Fatalf("%s: %q, %v", address, out, err)
		}
		if path != "POST /v1/chat/completions" || contentType != "application/json" {
			t.Errorf("%s: sent %s with %q", address, path, contentType)
		}
		want := `{"messages":[{"content":"s","role":"system"},{"content":"u","role":"user"}],"model":"m-1","stream":false,"temperature":0}`
		if b, _ := json.Marshal(got); string(b) != want {
			t.Errorf("body = %s, want %s", b, want)
		}
	}
}

// An answer that is not a chat completion is ErrUnreadable; an empty content is still content.
func TestChatUnreadableAnswers(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"server error":  {500, `{"choices":[{"message":{"content":"x"}}]}`},
		"not found":     {404, ``},
		"not JSON":      {200, `rate: 1200`},
		"no choices":    {200, `{"choices":[]}`},
		"null content":  {200, `{"choices":[{"message":{"content":null}}]}`},
		"no message":    {200, `{"choices":[{}]}`},
		"wrong type":    {200, `{"choices":[{"message":{"content":12}}]}`},
		"too large":     {200, `{"choices":[{"message":{"content":"` + strings.Repeat("a", maxResponseBytes) + `"}}]}`},
		"empty body":    {200, ``},
		"choices a map": {200, `{"choices":{}}`},
	} {
		t.Run(name, func(t *testing.T) {
			s := server(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			out, err := New(s.URL, "m").Chat(context.Background(), nil)
			if !errors.Is(err, ErrUnreadable) {
				t.Errorf("%q, %v; want ErrUnreadable", out, err)
			}
		})
	}
	s := server(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":""}}]}`)
	})
	if out, err := New(s.URL, "m").Chat(context.Background(), nil); err != nil || out != "" {
		t.Errorf("empty content: %q, %v", out, err)
	}
}

// A redirect is not followed: the prompt is sent to the configured address only.
func TestChatDoesNotFollowRedirects(t *testing.T) {
	var followed bool
	s := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed = true
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"x"}}]}`)
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	})
	if out, err := New(s.URL, "m").Chat(context.Background(), nil); !errors.Is(err, ErrUnreadable) || followed {
		t.Errorf("%q, %v, followed %v; want ErrUnreadable without following", out, err, followed)
	}
}

// No answer — nothing listening, or the caller's time limit passing — is not ErrUnreadable.
func TestChatNoAnswer(t *testing.T) {
	release := make(chan struct{})
	s := server(t, func(w http.ResponseWriter, r *http.Request) { <-release })
	defer close(release) // before the server's cleanup, which waits for this handler
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := New(s.URL, "m").Chat(ctx, nil)
	if err == nil || errors.Is(err, ErrUnreadable) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("hung server: %v; want the deadline, not ErrUnreadable", err)
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	if _, err := New(closed.URL, "m").Chat(context.Background(), nil); err == nil || errors.Is(err, ErrUnreadable) {
		t.Errorf("nothing listening: %v; want an error that is not ErrUnreadable", err)
	}
}
