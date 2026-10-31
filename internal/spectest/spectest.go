// Package spectest checks, in tests, that what BarPay sends fits the API spec's schemas
// (docs/api/openapi.yaml), read from the spec itself so a test cannot drift from it.
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

const specURL = "file:///openapi.json"

var (
	once     sync.Once
	compiler *jsonschema.Compiler
	loadErr  error

	mu       sync.Mutex
	compiled = map[string]*jsonschema.Schema{}
)

func load() {
	_, here, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "..", "docs", "api", "openapi.yaml"))
	if err != nil {
		loadErr = err
		return
	}
	var doc any
	if loadErr = yaml.Unmarshal(raw, &doc); loadErr != nil {
		return
	}
	// Through JSON, so the validator gets the numbers and maps it expects.
	j, err := json.Marshal(doc)
	if err != nil {
		loadErr = err
		return
	}
	spec, err := jsonschema.UnmarshalJSON(bytes.NewReader(j))
	if err != nil {
		loadErr = err
		return
	}
	compiler = jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020) // OpenAPI 3.1's dialect
	compiler.AssertFormat()
	loadErr = compiler.AddResource(specURL, spec)
}

func schema(name string) (*jsonschema.Schema, error) {
	once.Do(load)
	if loadErr != nil {
		return nil, loadErr
	}
	mu.Lock()
	defer mu.Unlock()
	if s, ok := compiled[name]; ok {
		return s, nil
	}
	s, err := compiler.Compile(specURL + "#/components/schemas/" + name)
	if err != nil {
		return nil, err
	}
	compiled[name] = s
	return s, nil
}

// Valid fails t unless body is JSON that fits the spec's components/schemas/<name>.
func Valid(t testing.TB, name string, body []byte) {
	t.Helper()
	s, err := schema(name)
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
