// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server_test

// Bitquery: the cfg side of MCP Apps. `kind: bitqueryResource` and a tool's
// bitqueryUI decode strictly, and a tool may only point at a resource the cfg
// enables.

import (
	"context"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/googleapis/mcp-toolbox/internal/bitqueryresources"
	"github.com/googleapis/mcp-toolbox/internal/server"
	"github.com/googleapis/mcp-toolbox/internal/telemetry"
	"github.com/googleapis/mcp-toolbox/internal/testutils"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	_ "github.com/googleapis/mcp-toolbox/internal/tools/clickhouse/clickhousesql"
	_ "github.com/googleapis/mcp-toolbox/internal/tools/http"
	"github.com/googleapis/mcp-toolbox/internal/util"
)

const appsSourceDoc = `
kind: source
name: gw
type: http
baseUrl: http://127.0.0.1:1
timeout: 1s
allowPrivateNetworks: true
`

func appsToolDoc(ui string) string {
	return `
kind: tool
name: flow_edges
type: http
source: gw
description: edges
method: POST
path: /
requestBody: SELECT 1
` + ui
}

const appsResourceDoc = `
kind: bitqueryResource
name: flow-graph
`

func appsDocs(docs ...string) []byte {
	return []byte(strings.Join(docs, "\n---\n"))
}

func appsContext(t *testing.T) context.Context {
	t.Helper()
	ctx, err := testutils.ContextWithNewLogger()
	if err != nil {
		t.Fatalf("logger: %s", err)
	}
	instrumentation, err := telemetry.CreateTelemetryInstrumentation("0.0.0")
	if err != nil {
		t.Fatalf("instrumentation: %s", err)
	}
	return util.WithInstrumentation(ctx, instrumentation)
}

func TestBitqueryResourceDocument(t *testing.T) {
	ctx := appsContext(t)

	_, _, _, _, _, _, resources, err := server.UnmarshalConfigWithResources(ctx, []byte(appsResourceDoc))
	if err != nil {
		t.Fatalf("a bitqueryResource document must decode: %s", err)
	}
	if diff := cmp.Diff(server.ResourceConfigs{"flow-graph": {Name: "flow-graph"}}, resources); diff != "" {
		t.Errorf("resources (-want +got):\n%s", diff)
	}
	// The plain parser accepts the document and leaves it out.
	if _, _, _, _, _, _, err := server.UnmarshalPrimitiveConfig(ctx, []byte(appsResourceDoc)); err != nil {
		t.Errorf("UnmarshalPrimitiveConfig refused the document: %s", err)
	}
	// No document, no resource.
	_, _, _, _, _, _, resources, err = server.UnmarshalConfigWithResources(ctx, appsDocs(appsSourceDoc))
	if err != nil || resources != nil {
		t.Errorf("a cfg without the kind: resources %v, err %v", resources, err)
	}

	for name, tc := range map[string]struct {
		raw  string
		want string
	}{
		"unknown subkey":    {appsResourceDoc + "uri: ui://bitquery/flow-graph\n", `unknown field "uri"`},
		"not a built-in":    {"kind: bitqueryResource\nname: sankey\n", `"sankey" is not a built-in resource; the built-in resources are: flow-graph`},
		"declared twice":    {string(appsDocs(appsResourceDoc, appsResourceDoc)), `bitqueryResource "flow-graph" declared more than once`},
		"name not a string": {"kind: bitqueryResource\nname: 7\n", `missing 'name' field or it is not a string`},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, _, _, _, _, _, err := server.UnmarshalConfigWithResources(ctx, []byte(tc.raw))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			if _, _, _, _, _, _, err := server.UnmarshalPrimitiveConfig(ctx, []byte(tc.raw)); err == nil {
				t.Fatalf("UnmarshalPrimitiveConfig accepted it")
			}
		})
	}
}

func TestBitqueryUIDecodesStrictly(t *testing.T) {
	ctx := appsContext(t)
	for name, tc := range map[string]struct {
		ui   string
		want string
	}{
		"unknown subkey":        {"bitqueryUI:\n  resourceUri: ui://bitquery/flow-graph\n  bogus: 1\n", `unknown field "bogus"`},
		"misspelt key":          {"bitqueryUi:\n  resourceUri: ui://bitquery/flow-graph\n", `unknown field "bitqueryUi"`},
		"visibility not a list": {"bitqueryUI:\n  visibility: app\n", "visibility"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, _, _, _, _, _, err := server.UnmarshalConfigWithResources(ctx, appsDocs(appsSourceDoc, appsToolDoc(tc.ui), appsResourceDoc))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}

	// clickhouse-sql keeps the block through Initialize and ToConfig.
	_, _, _, toolConfigs, _, _, err := server.UnmarshalPrimitiveConfig(ctx, []byte(`
kind: tool
name: token_price
type: clickhouse-sql
source: trading
description: price
statement: SELECT 1
bitqueryUI:
  resourceUri: ui://bitquery/flow-graph
`))
	if err != nil {
		t.Fatalf("decode: %s", err)
	}
	tool, err := toolConfigs["token_price"].Initialize(ctx)
	if err != nil {
		t.Fatalf("initialize: %s", err)
	}
	if ui := tools.BitqueryUIOf(tool.ToConfig()); ui == nil || ui.ResourceURI != "ui://bitquery/flow-graph" {
		t.Fatalf("clickhouse-sql tool lost bitqueryUI: %+v", ui)
	}
}

func TestBitqueryUIValidatedAtLoad(t *testing.T) {
	ctx := appsContext(t)
	load := func(docs ...string) (map[string]tools.Tool, server.ServerConfig, error) {
		s, a, e, tc, p, g, r, err := server.UnmarshalConfigWithResources(ctx, appsDocs(docs...))
		if err != nil {
			t.Fatalf("decode: %s", err)
		}
		cfg := server.ServerConfig{Version: "0.0.0", SourceConfigs: s, AuthServiceConfigs: a, EmbeddingModelConfigs: e, ToolConfigs: tc, PromptConfigs: p, GroupConfigs: g, ResourceConfigs: r}
		_, _, _, toolsMap, _, _, err := server.InitializeConfigs(ctx, cfg)
		return toolsMap, cfg, err
	}

	ui := "bitqueryUI:\n  resourceUri: ui://bitquery/flow-graph\n  visibility: [model, app]\n"
	toolsMap, cfg, err := load(appsSourceDoc, appsToolDoc(ui), appsResourceDoc)
	if err != nil {
		t.Fatalf("a tool pointing at an enabled resource must load: %s", err)
	}
	want := &tools.ToolUI{ResourceURI: "ui://bitquery/flow-graph", Visibility: []string{"model", "app"}}
	if diff := cmp.Diff(want, tools.BitqueryUIOf(toolsMap["flow_edges"].ToConfig())); diff != "" {
		t.Errorf("the http tool's bitqueryUI (-want +got):\n%s", diff)
	}
	resources, err := server.InitializeResources(cfg)
	if err != nil {
		t.Fatalf("InitializeResources: %s", err)
	}
	if r, ok := resources["ui://bitquery/flow-graph"]; !ok || r.MimeType != bitqueryresources.AppMimeType || r.Text == "" {
		t.Errorf("InitializeResources = %+v", resources)
	}
	// An app-only tool needs no resource.
	if _, _, err := load(appsSourceDoc, appsToolDoc("bitqueryUI:\n  visibility: [app]\n")); err != nil {
		t.Errorf("an app-only tool without resourceUri must load: %s", err)
	}

	for name, tc := range map[string]struct {
		ui   string
		docs []string
		want string
	}{
		"resource not enabled": {ui: ui, want: `tool "flow_edges": bitqueryUI.resourceUri "ui://bitquery/flow-graph" is not enabled; enable it with a document of kind bitqueryResource and name flow-graph`},
		"unknown resource":     {ui: "bitqueryUI:\n  resourceUri: ui://bitquery/sankey\n", docs: []string{appsResourceDoc}, want: `bitqueryUI.resourceUri "ui://bitquery/sankey" is not a built-in resource; the built-in resources are: ui://bitquery/flow-graph`},
		"not a ui uri":         {ui: "bitqueryUI:\n  resourceUri: https://example.com/page\n", docs: []string{appsResourceDoc}, want: `must start with ui://`},
		"unknown visibility":   {ui: "bitqueryUI:\n  visibility: [user]\n", want: `bitqueryUI.visibility "user" is not "model" or "app"`},
		"visibility twice":     {ui: "bitqueryUI:\n  visibility: [app, app]\n", want: `lists "app" twice`},
		"empty visibility":     {ui: "bitqueryUI:\n  visibility: []\n", want: `bitqueryUI.visibility is empty`},
		"empty block":          {ui: "bitqueryUI: {}\n", want: `bitqueryUI sets neither resourceUri nor visibility`},
	} {
		t.Run(name, func(t *testing.T) {
			docs := append([]string{appsSourceDoc, appsToolDoc(tc.ui)}, tc.docs...)
			_, _, err := load(docs...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}
