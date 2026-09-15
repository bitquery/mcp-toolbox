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

package clickhouse

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/googleapis/mcp-toolbox/internal/bitquerylabels"
	"github.com/googleapis/mcp-toolbox/internal/sources"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

// Tests for the Bitquery labels-query-service pre-process hook in Invoke.

const hookTestAddress = "0x28C6c06298d514Db089934071355E5743bf21d60"

// plainSource is a clickhouse-compatible source without the hook.
type plainSource struct {
	events *[]string
}

func (s *plainSource) SourceType() string             { return "clickhouse" }
func (s *plainSource) ToConfig() sources.SourceConfig { return nil }
func (s *plainSource) RunSQL(_ context.Context, statement string, _ parameters.ParamValues) (any, error) {
	*s.events = append(*s.events, "sql:"+statement)
	return []any{map[string]any{"ok": 1}}, nil
}

// hookSource is a clickhouse-compatible source carrying the hook.
type hookSource struct {
	plainSource
	preprocessor *bitquerylabels.Preprocessor
}

func (s *hookSource) BitqueryLabelsPreprocessor() *bitquerylabels.Preprocessor {
	return s.preprocessor
}

type recordingClient struct {
	events *[]string
}

func (c *recordingClient) Refresh(_ context.Context, req bitquerylabels.Request) {
	*c.events = append(*c.events, "refresh:"+strings.Join(req.Addresses, ",")+"@"+req.Chain+strings.Join(req.Chains, "+"))
}

func newHookTestTool(t *testing.T, statement string, names ...string) Tool {
	t.Helper()
	var params parameters.Parameters
	for _, n := range names {
		params = append(params, parameters.NewStringParameter(n, n))
	}
	tool, err := Config{
		ConfigBase:         tools.ConfigBase{Name: "labels_tool", Description: "d"},
		Type:               sqlType,
		Source:             "labels",
		Statement:          statement,
		TemplateParameters: params,
	}.Initialize(context.Background())
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return tool.(Tool)
}

func invokeHookTestTool(t *testing.T, tool Tool, src sources.Source, values parameters.ParamValues) any {
	t.Helper()
	if err := tool.ValidateSource(src); err != nil {
		t.Fatalf("ValidateSource: %v", err)
	}
	resp, terr := tool.Invoke(context.Background(), src, values, tools.AccessToken(""))
	if terr != nil {
		t.Fatalf("Invoke: %v", terr)
	}
	return resp
}

// The hook runs BEFORE the SQL, with the addresses and chain from the parameters.
func TestInvokeRunsLabelsHookBeforeSQL(t *testing.T) {
	var events []string
	src := &hookSource{
		plainSource:  plainSource{events: &events},
		preprocessor: bitquerylabels.NewPreprocessor(&recordingClient{events: &events}, nil, ""),
	}
	tool := newHookTestTool(t, "SELECT '{{.address}}', '{{.chain}}'", "address", "chain")

	invokeHookTestTool(t, tool, src, parameters.ParamValues{
		{Name: "address", Value: hookTestAddress},
		{Name: "chain", Value: "polygon"},
	})

	want := []string{
		"refresh:" + strings.ToLower(hookTestAddress) + "@polygon",
		"sql:SELECT '" + hookTestAddress + "', 'polygon'",
	}
	if diff := cmp.Diff(want, events); diff != "" {
		t.Fatalf("event order mismatch (-want +got):\n%s", diff)
	}
}

// A failing or hanging labels service never fails the call: the SQL still runs
// and its result is returned, within the hook's timeout.
func TestInvokeLabelsHookFailOpen(t *testing.T) {
	release := make(chan struct{})
	hanging := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer hanging.Close()
	defer close(release)
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failing.Close()

	for name, rawURL := range map[string]string{"timeout": hanging.URL, "http 500": failing.URL, "refused": "http://127.0.0.1:1"} {
		t.Run(name, func(t *testing.T) {
			u, _ := url.Parse(rawURL)
			svc, err := bitquerylabels.NewService(bitquerylabels.Config{Host: u.Host, TimeoutSec: 0.2})
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			var events []string
			src := &hookSource{
				plainSource:  plainSource{events: &events},
				preprocessor: bitquerylabels.NewPreprocessor(svc, nil, ""),
			}
			tool := newHookTestTool(t, "SELECT '{{.addresses}}'", "addresses")

			start := time.Now()
			resp := invokeHookTestTool(t, tool, src, parameters.ParamValues{{Name: "addresses", Value: hookTestAddress}})
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Errorf("Invoke took %s, want it bounded by the 200ms hook timeout", elapsed)
			}
			if len(events) != 1 || !strings.HasPrefix(events[0], "sql:") {
				t.Errorf("events = %v, want the SQL to run", events)
			}
			if diff := cmp.Diff([]any{map[string]any{"ok": 1}}, resp); diff != "" {
				t.Errorf("result mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// Tools on a source without the hook — or with it unconfigured — are untouched,
// and a hooked tool whose parameters name no address makes no call.
func TestInvokeWithoutLabelsHook(t *testing.T) {
	var events []string
	client := &recordingClient{events: &events}

	plain := &plainSource{events: &events}
	unconfigured := &hookSource{plainSource: plainSource{events: &events}}
	hooked := &hookSource{
		plainSource:  plainSource{events: &events},
		preprocessor: bitquerylabels.NewPreprocessor(client, nil, ""),
	}

	withAddress := newHookTestTool(t, "SELECT '{{.address}}'", "address")
	invokeHookTestTool(t, withAddress, plain, parameters.ParamValues{{Name: "address", Value: hookTestAddress}})
	invokeHookTestTool(t, withAddress, unconfigured, parameters.ParamValues{{Name: "address", Value: hookTestAddress}})

	noAddress := newHookTestTool(t, "SELECT '{{.query}}', '{{.chain}}'", "query", "chain")
	invokeHookTestTool(t, noAddress, hooked, parameters.ParamValues{{Name: "query", Value: "binance"}, {Name: "chain", Value: "ethereum"}})

	want := []string{
		"sql:SELECT '" + hookTestAddress + "'",
		"sql:SELECT '" + hookTestAddress + "'",
		"sql:SELECT 'binance', 'ethereum'",
	}
	if diff := cmp.Diff(want, events); diff != "" {
		t.Fatalf("events mismatch (-want +got):\n%s", diff)
	}
}
