// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package http

// Bitquery: the opt-in format=columnar parameter returns the rows as one
// columnar text block with no structured content; format=rows and an absent
// format keep today's decoded []any, and `format` is never forwarded upstream.

import (
	"context"
	nethttp "net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/googleapis/mcp-toolbox/internal/sources"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

// columnarBackend returns a fixed two-row body and records each request query.
type columnarBackend struct {
	server  *httptest.Server
	mu      sync.Mutex
	queries []string
}

func newColumnarBackend(t *testing.T, body string) *columnarBackend {
	t.Helper()
	b := &columnarBackend{}
	b.server = httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		b.mu.Lock()
		b.queries = append(b.queries, r.URL.RawQuery)
		b.mu.Unlock()
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(b.server.Close)
	return b
}

func columnarTool(t *testing.T, ctx context.Context, src sources.Source) Tool {
	t.Helper()
	cfg := Config{
		ConfigBase:  tools.ConfigBase{Name: "rows_tool", Description: "returns rows"},
		Type:        resourceType,
		Source:      "cs",
		Path:        "/",
		Method:      "POST",
		RequestBody: "SELECT 1",
	}
	tl, err := cfg.Initialize(ctx)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return tl.(Tool)
}

func TestColumnarHTTPTool(t *testing.T) {
	ctx := bitqueryRoutesContext(t)
	backend := newColumnarBackend(t, `[{"Amount":"1000","Status":"OK"},{"Amount":"2000","Status":"OK"}]`)
	src := bitqueryRoutesSource(t, ctx, "cs", backend.server.URL, nil, "")
	tl := columnarTool(t, ctx, src)

	rowsValue := []any{
		map[string]any{"Amount": "1000", "Status": "OK"},
		map[string]any{"Amount": "2000", "Status": "OK"},
	}

	// format absent: today's decoded rows, unchanged.
	got, terr := tl.Invoke(ctx, src, parameters.ParamValues{}, "")
	if terr != nil {
		t.Fatalf("invoke (absent): %v", terr)
	}
	if !reflect.DeepEqual(got, rowsValue) {
		t.Fatalf("absent format = %#v, want the decoded rows %#v", got, rowsValue)
	}

	// format=rows: identical to absent.
	got, terr = tl.Invoke(ctx, src, parameters.ParamValues{{Name: "format", Value: "rows"}}, "")
	if terr != nil {
		t.Fatalf("invoke (rows): %v", terr)
	}
	if !reflect.DeepEqual(got, rowsValue) {
		t.Fatalf("format=rows = %#v, want the decoded rows", got)
	}

	// format=columnar: one verbatim TextResult, no structured content.
	got, terr = tl.Invoke(ctx, src, parameters.ParamValues{{Name: "format", Value: "columnar"}}, "")
	if terr != nil {
		t.Fatalf("invoke (columnar): %v", terr)
	}
	tr, ok := got.(tools.TextResult)
	if !ok {
		t.Fatalf("columnar result is %T, want tools.TextResult", got)
	}
	if want := `{"columns":["Amount","Status"],"rows":[["1000","OK"],["2000","OK"]]}`; tr.Text != want {
		t.Errorf("columnar text = %s, want %s", tr.Text, want)
	}
	items, structured := tools.SplitResult(got)
	if structured != nil {
		t.Errorf("columnar carried structuredContent: %v", structured)
	}
	if len(items) != 1 {
		t.Errorf("columnar split into %d items, want 1", len(items))
	}

	// `format` was never sent to the backend on any of the calls.
	backend.mu.Lock()
	defer backend.mu.Unlock()
	for i, q := range backend.queries {
		if strings.Contains(q, "format") {
			t.Errorf("call %d forwarded format upstream: query %q", i, q)
		}
	}
}
