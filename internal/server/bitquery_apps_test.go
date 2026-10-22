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

package server

// Bitquery: MCP Apps end to end, in every protocol version: the resources
// capability, resources/list, resources/read, resources/templates/list, a
// tool's `_meta.ui` next to the toolbox's own `_meta` keys, an app-only tool,
// and structuredContent.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/googleapis/mcp-toolbox/internal/bitqueryresources"
	"github.com/googleapis/mcp-toolbox/internal/group"
	"github.com/googleapis/mcp-toolbox/internal/prompts"
	"github.com/googleapis/mcp-toolbox/internal/sources"
	"github.com/googleapis/mcp-toolbox/internal/testutils"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

const flowGraphURI = "ui://bitquery/flow-graph"

// appsStructuredTool returns text for the model and an object for the view.
type appsStructuredTool struct {
	testutils.MockTool
	content []any
}

func (t appsStructuredTool) Invoke(context.Context, sources.Source, parameters.ParamValues, tools.AccessToken) (any, util.ToolboxError) {
	return tools.StructuredResult{
		Content:    t.content,
		Structured: map[string]any{"edges": []any{map[string]any{"from": "a", "to": "b", "value": 2.5}}},
	}, nil
}

func appsMockTool(t *testing.T, cfg testutils.MockToolConfig) testutils.MockTool {
	t.Helper()
	cfg.Type = "mock-tool"
	tool, err := cfg.Initialize(context.Background())
	if err != nil {
		t.Fatalf("tool %q: %s", cfg.Name, err)
	}
	return tool.(testutils.MockTool)
}

func appsPrimitives(t *testing.T) (map[string]tools.Tool, map[string]prompts.Prompt, map[string]group.Group) {
	t.Helper()
	flowUI := &tools.ToolUI{ResourceURI: flowGraphURI, Visibility: []string{tools.UIVisibilityModel, tools.UIVisibilityApp}}
	toolsMap := map[string]tools.Tool{
		"no_params": testutils.MockTool1,
		"flow_edges": appsStructuredTool{
			MockTool: appsMockTool(t, testutils.MockToolConfig{ConfigBase: tools.ConfigBase{Name: "flow_edges", Description: "edges", BitqueryUI: flowUI}}),
			content:  []any{"1 edge"},
		},
		"flow_auth": appsMockTool(t, testutils.MockToolConfig{ConfigBase: tools.ConfigBase{Name: "flow_auth", AuthRequired: []string{"my-auth"}, BitqueryUI: flowUI}}),
		"flow_page": appsStructuredTool{
			MockTool: appsMockTool(t, testutils.MockToolConfig{ConfigBase: tools.ConfigBase{Name: "flow_page", Description: "next page", BitqueryUI: &tools.ToolUI{Visibility: []string{tools.UIVisibilityApp}}}}),
		},
	}
	groups := map[string]group.Group{
		"":     group.NewGroup(group.GroupConfig{Name: "", ToolNames: []string{"flow_auth", "flow_edges", "flow_page", "no_params"}}),
		"maps": group.NewGroup(group.GroupConfig{Name: "maps", ToolNames: []string{"flow_edges"}}),
	}
	return toolsMap, map[string]prompts.Prompt{}, groups
}

func appsResources(t *testing.T) map[string]bitqueryresources.Resource {
	t.Helper()
	flow, ok := bitqueryresources.Builtin("flow-graph")
	if !ok {
		t.Fatal("flow-graph is not a built-in resource")
	}
	border := true
	return map[string]bitqueryresources.Resource{
		flow.URI: flow,
		"ui://test/csp": {
			Name:     "csp",
			URI:      "ui://test/csp",
			MimeType: bitqueryresources.AppMimeType,
			UI: &bitqueryresources.UIMeta{
				CSP:           &bitqueryresources.CSP{ConnectDomains: []string{"https://api.example.com"}},
				PrefersBorder: &border,
			},
			Text: "<p>csp</p>",
		},
	}
}

func withResources(m map[string]bitqueryresources.Resource) func(*Server) {
	return func(s *Server) { s.PrimitiveMgr.SetResources(m) }
}

type appsClient struct {
	t       *testing.T
	url     string
	version string
	post    func(url string, body []byte, header map[string]string) (int, map[string]any)
	id      int
}

func (c *appsClient) call(method string, params map[string]any, mutate ...func(map[string]string)) (int, map[string]any) {
	c.t.Helper()
	c.id++
	req := compatRequest{method: method, params: params}
	if name, ok := params["name"].(string); ok {
		req.mcpName = name
	} else if uri, ok := params["uri"].(string); ok {
		req.mcpName = uri
	}
	header := compatHeaders(req, c.version)
	for _, m := range mutate {
		m(header)
	}
	return c.post(c.url, compatBody(c.t, c.id, req, c.version), header)
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func listedTool(t *testing.T, res map[string]any, name string) map[string]any {
	t.Helper()
	result, _ := res["result"].(map[string]any)
	list, _ := result["tools"].([]any)
	for _, item := range list {
		tool := item.(map[string]any)
		if tool["name"] == name {
			return tool
		}
	}
	t.Fatalf("tool %q is not listed in %s", name, asJSON(t, res))
	return nil
}

func TestBitqueryAppsEveryVersion(t *testing.T) {
	toolsMap, promptsMap, groups := appsPrimitives(t)
	r, shutdown := setUpServer(t, "mcp", toolsMap, promptsMap, groups, withResources(appsResources(t)))
	defer shutdown()
	ts := runServer(r, false)
	defer ts.Close()
	post := func(url string, body []byte, header map[string]string) (int, map[string]any) {
		resp, raw, err := runRequest(ts, http.MethodPost, url, bytes.NewReader(body), header)
		if err != nil {
			t.Fatalf("request: %s", err)
		}
		var got map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("response %q: %s", raw, err)
		}
		return resp.StatusCode, got
	}

	versions := []string{protocolVersion20241105, protocolVersion20250326, protocolVersion20250618, protocolVersion20251125, protocolVersion20260728}
	for _, v := range versions {
		t.Run(v, func(t *testing.T) {
			c := &appsClient{t: t, url: "/", version: v, post: post}
			hasStructured := v != protocolVersion20241105 && v != protocolVersion20250326
			hasTitle := hasStructured
			authKey := "toolbox/authInvoke"
			if v == protocolVersion20260728 {
				authKey = "com.google.cloud/authInvoke"
			}

			// The capability.
			var capabilities map[string]any
			if v == protocolVersion20260728 {
				_, res := c.call("server/discover", nil)
				capabilities = res["result"].(map[string]any)["capabilities"].(map[string]any)
			} else {
				_, res := post("/", initializeBody(t, 1, v), nil)
				capabilities = res["result"].(map[string]any)["capabilities"].(map[string]any)
			}
			if got := asJSON(t, capabilities["resources"]); got != `{"listChanged":false}` {
				t.Errorf("resources capability = %s", got)
			}

			// tools/list: _meta.ui next to the auth key, nothing on a plain tool.
			_, res := c.call("tools/list", nil)
			if got, want := asJSON(t, listedTool(t, res, "flow_auth")["_meta"]), `{"`+authKey+`":["my-auth"],"ui":{"resourceUri":"ui://bitquery/flow-graph","visibility":["model","app"]}}`; got != want {
				t.Errorf("flow_auth _meta = %s, want %s", got, want)
			}
			if got, want := asJSON(t, listedTool(t, res, "flow_edges")["_meta"]), `{"ui":{"resourceUri":"ui://bitquery/flow-graph","visibility":["model","app"]}}`; got != want {
				t.Errorf("flow_edges _meta = %s, want %s", got, want)
			}
			// An app-only tool is listed: the host keeps it from the model.
			if got, want := asJSON(t, listedTool(t, res, "flow_page")["_meta"]), `{"ui":{"visibility":["app"]}}`; got != want {
				t.Errorf("flow_page _meta = %s, want %s", got, want)
			}
			if meta, ok := listedTool(t, res, "no_params")["_meta"]; ok {
				t.Errorf("a tool without bitqueryUI got _meta %s", asJSON(t, meta))
			}

			// tools/call: structuredContent where the version has it.
			_, res = c.call("tools/call", map[string]any{"name": "flow_edges"})
			result := res["result"].(map[string]any)
			if got := asJSON(t, result["content"]); got != `[{"text":"\"1 edge\"","type":"text"}]` {
				t.Errorf("content = %s", got)
			}
			structured, ok := result["structuredContent"]
			if ok != hasStructured {
				t.Errorf("structuredContent present = %v, want %v", ok, hasStructured)
			}
			if ok && asJSON(t, structured) != `{"edges":[{"from":"a","to":"b","value":2.5}]}` {
				t.Errorf("structuredContent = %s", asJSON(t, structured))
			}

			// The app-only tool is callable by group membership; with no
			// content of its own, its text is the structured object.
			_, res = c.call("tools/call", map[string]any{"name": "flow_page"})
			result = res["result"].(map[string]any)
			if got := asJSON(t, result["content"]); got != `[{"text":"{\"edges\":[{\"from\":\"a\",\"to\":\"b\",\"value\":2.5}]}","type":"text"}]` {
				t.Errorf("app-only tool content = %s", got)
			}

			// resources/list, the same on a group endpoint.
			for _, url := range []string{"/", "/maps"} {
				c.url = url
				_, res = c.call("resources/list", nil)
				list := res["result"].(map[string]any)["resources"].([]any)
				if len(list) != 2 {
					t.Fatalf("%s resources/list = %s", url, asJSON(t, res))
				}
				flow := list[0].(map[string]any)
				want := map[string]any{"uri": flowGraphURI, "name": "flow-graph", "description": "Interactive graph of the value flows between addresses.", "mimeType": "text/html;profile=mcp-app"}
				if hasTitle {
					want["title"] = "Flow Graph"
				}
				if !reflect.DeepEqual(flow, want) {
					t.Errorf("%s flow-graph listed as %s", url, asJSON(t, flow))
				}
				if got := asJSON(t, list[1].(map[string]any)["_meta"]); got != `{"ui":{"csp":{"connectDomains":["https://api.example.com"]},"prefersBorder":true}}` {
					t.Errorf("%s csp resource _meta = %s", url, got)
				}
			}
			c.url = "/"

			// resources/read.
			_, res = c.call("resources/read", map[string]any{"uri": flowGraphURI})
			contents := res["result"].(map[string]any)["contents"].([]any)
			item := contents[0].(map[string]any)
			if item["uri"] != flowGraphURI || item["mimeType"] != "text/html;profile=mcp-app" || !strings.HasPrefix(item["text"].(string), "<!doctype html>") {
				t.Errorf("resources/read = %s", asJSON(t, res))
			}
			if _, ok := item["_meta"]; ok {
				t.Errorf("flow-graph content carries _meta %s", asJSON(t, item["_meta"]))
			}
			_, res = c.call("resources/read", map[string]any{"uri": "ui://test/csp"})
			item = res["result"].(map[string]any)["contents"].([]any)[0].(map[string]any)
			if got := asJSON(t, item["_meta"]); got != `{"ui":{"csp":{"connectDomains":["https://api.example.com"]},"prefersBorder":true}}` {
				t.Errorf("csp content _meta = %s", got)
			}
			if v == protocolVersion20260728 {
				result := res["result"].(map[string]any)
				if result["resultType"] != "complete" || result["cacheScope"] != "public" || result["_meta"] == nil {
					t.Errorf("resources/read result = %s", asJSON(t, result))
				}
			}

			// An unknown URI.
			code := float64(-32002)
			if v == protocolVersion20260728 {
				code = -32602
			}
			_, res = c.call("resources/read", map[string]any{"uri": "ui://bitquery/nothing"})
			wantErr := map[string]any{"code": code, "message": "Resource not found", "data": map[string]any{"uri": "ui://bitquery/nothing"}}
			if !reflect.DeepEqual(res["error"], wantErr) {
				t.Errorf("unknown resource = %s", asJSON(t, res))
			}

			// resources/templates/list.
			_, res = c.call("resources/templates/list", nil)
			if got := asJSON(t, res["result"].(map[string]any)["resourceTemplates"]); got != `[]` {
				t.Errorf("resourceTemplates = %s", got)
			}

			if v == protocolVersion20260728 {
				status, res := c.call("resources/read", map[string]any{"uri": flowGraphURI}, func(h map[string]string) { h["Mcp-Name"] = "ui://bitquery/other" })
				if status != http.StatusBadRequest || res["error"].(map[string]any)["code"] != float64(-32020) {
					t.Errorf("Mcp-Name mismatch: status %d, %s", status, asJSON(t, res))
				}
			}
		})
	}
}

// TestBitqueryAppsStdio covers the transport without headers.
func TestBitqueryAppsStdio(t *testing.T) {
	toolsMap, promptsMap, groups := appsPrimitives(t)
	var srv *Server
	_, shutdown := setUpServer(t, "mcp", toolsMap, promptsMap, groups, withResources(appsResources(t)), func(s *Server) { srv = s })
	defer shutdown()
	ctx := util.WithLogger(context.Background(), srv.logger)

	for _, v := range []string{"", protocolVersion20251125, protocolVersion20260728} {
		body := compatBody(t, 1, compatRequest{method: "resources/read", params: map[string]any{"uri": flowGraphURI}}, v)
		_, res, err := processMcpMessage(ctx, body, srv, v, "", nil, "")
		if err != nil {
			t.Fatalf("%q: %s", v, err)
		}
		got := asJSON(t, res)
		if !strings.Contains(got, `"mimeType":"text/html;profile=mcp-app"`) {
			t.Errorf("%q resources/read = %s", v, got)
		}
	}
}

// TestBitqueryCompatGoldenDetectsResources shows the golden record notices a
// served resource: the same requests answer differently once one is enabled.
func TestBitqueryCompatGoldenDetectsResources(t *testing.T) {
	plain := recordCompatAnswers(t)
	enabled := recordCompatAnswers(t, withResources(appsResources(t)))
	if plain == enabled {
		t.Fatal("enabling a resource changed no answer")
	}
	for _, want := range []string{`"resources":{"listChanged":false}`, `"mimeType":"text/html;profile=mcp-app"`, `"resourceTemplates":[]`} {
		if !strings.Contains(enabled, want) || strings.Contains(plain, want) {
			t.Errorf("%s should appear only once a resource is served", want)
		}
	}
}
