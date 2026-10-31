// Package llm asks the local LLM for a chat completion over the OpenAI-compatible HTTP API,
// POST <address>/chat/completions, which llama.cpp's server, Ollama, vLLM and LM Studio all
// serve, so the owner can run the model on any of them.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Message is one chat message.
type Message struct {
	Role    string `json:"role"` // "system" or "user"
	Content string `json:"content"`
}

// ErrUnreadable is returned when the server answered, but not with a chat completion: an HTTP
// status other than 2xx (a redirect included), a body that is not the API's response, or one larger than maxResponseBytes.
// Every other error means no answer came.
var ErrUnreadable = errors.New("llm: the answer is not a chat completion")

// maxResponseBytes bounds what is read from the server; a pricing answer is far smaller.
const maxResponseBytes = 1 << 20

// Client calls one model at one address. It is safe for concurrent use.
type Client struct {
	url   string
	model string
	http  *http.Client
}

// New returns a Client for the model at address, the API's base URL such as
// "http://127.0.0.1:11434/v1". It sets no time limit: the caller's context does. It follows no
// redirect: the prompt goes only to the configured address, and a 3xx is an answer that is not
// a completion.
func New(address, model string) *Client {
	h := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Client{url: strings.TrimSuffix(address, "/") + "/chat/completions", model: model, http: h}
}

type request struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	// Temperature 0 asks for the same answer to the same messages, as far as the server can give one.
	Temperature float64 `json:"temperature"`
	Stream      bool    `json:"stream"`
}

type response struct {
	Choices []struct {
		Message struct {
			Content *string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// Chat sends messages and returns the content of the first choice.
func (c *Client) Chat(ctx context.Context, messages []Message) (string, error) {
	body, err := json.Marshal(request{Model: c.model, Messages: messages})
	if err != nil {
		return "", fmt.Errorf("llm: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("llm: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return "", fmt.Errorf("llm: read the answer: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("%w: HTTP status %d", ErrUnreadable, resp.StatusCode)
	}
	if len(data) > maxResponseBytes {
		return "", fmt.Errorf("%w: larger than %d bytes", ErrUnreadable, maxResponseBytes)
	}
	var r response
	if err := json.Unmarshal(data, &r); err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnreadable, err)
	}
	if len(r.Choices) == 0 || r.Choices[0].Message.Content == nil {
		return "", fmt.Errorf("%w: no message content", ErrUnreadable)
	}
	return *r.Choices[0].Message.Content, nil
}
