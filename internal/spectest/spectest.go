// Package spectest checks, in tests, that what BarPay sends fits the API spec's schemas
// (docs/api/openapi.yaml, and edge.yaml for the edge device's API), read from the specs
// themselves so a test cannot drift from them.
package spectest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"
)

const (
	specURL = "file:///openapi.json"
	edgeURL = "file:///edge.json"
)

var (
	once     sync.Once
	compiler *jsonschema.Compiler
	loadErr  error

	mu       sync.Mutex
	compiled = map[string]*jsonschema.Schema{}
)

func load() {
	compiler = jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020) // OpenAPI 3.1's dialect
	compiler.AssertFormat()
	if loadErr = add("openapi.yaml", specURL); loadErr == nil {
		loadErr = add("edge.yaml", edgeURL)
	}
}

// add reads the spec docs/api/file and adds it to the compiler as url.
func add(file, url string) error {
	_, here, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "..", "docs", "api", file))
	if err != nil {
		return err
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return err
	}
	// Through JSON, so the validator gets the numbers and maps it expects.
	j, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	spec, err := jsonschema.UnmarshalJSON(bytes.NewReader(j))
	if err != nil {
		return err
	}
	return compiler.AddResource(url, spec)
}

func schema(url, name string) (*jsonschema.Schema, error) {
	once.Do(load)
	if loadErr != nil {
		return nil, loadErr
	}
	mu.Lock()
	defer mu.Unlock()
	ref := url + "#/components/schemas/" + name
	if s, ok := compiled[ref]; ok {
		return s, nil
	}
	s, err := compiler.Compile(ref)
	if err != nil {
		return nil, err
	}
	compiled[ref] = s
	return s, nil
}

// Valid fails t unless body is JSON that fits the spec's components/schemas/<name>.
func Valid(t testing.TB, name string, body []byte) {
	t.Helper()
	valid(t, specURL, name, body)
}

// ValidEdge is Valid for the edge device's API (edge.yaml).
func ValidEdge(t testing.TB, name string, body []byte) {
	t.Helper()
	valid(t, edgeURL, name, body)
}

func valid(t testing.TB, url, name string, body []byte) {
	t.Helper()
	s, err := schema(url, name)
	if err != nil {
		t.Fatalf("spec schema %s: %v", name, err)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("not JSON: %v\n%s", err, body)
	}
	if err := s.Validate(v); err != nil {
		t.Errorf("does not fit the spec's %s: %v\n%s", name, err, body)
	}
}
