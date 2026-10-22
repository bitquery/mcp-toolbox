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

// Bitquery: a byte-for-byte record of what the MCP endpoint answers, request by
// request, in every protocol version, for a server whose cfg enables no
// resource and whose tools return plain results and carry no UI. The file was
// written from the tree before MCP Apps support (resources, structuredContent,
// tool _meta.ui) existed; that support must leave every one of these answers
// as it was. Regenerate it only on purpose:
//
//	UPDATE_COMPAT_GOLDEN=1 go test ./internal/server -run TestBitqueryCompatGolden

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/googleapis/mcp-toolbox/internal/group"
	"github.com/googleapis/mcp-toolbox/internal/prompts"
	"github.com/googleapis/mcp-toolbox/internal/sources"
	"github.com/googleapis/mcp-toolbox/internal/testutils"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

const compatGoldenFile = "testdata/bitquery_compat_golden.txt"

// compatMapTool returns one JSON object instead of a list.
type compatMapTool struct{ testutils.MockTool }

func (compatMapTool) Invoke(context.Context, sources.Source, parameters.ParamValues, tools.AccessToken) (any, util.ToolboxError) {
	return map[string]any{"rows": []any{1, 2}, "note": "a<b&c"}, nil
}

// compatRefusedTool fails the way a refused statement does: a result with
// isError set.
type compatRefusedTool struct{ testutils.MockTool }

func (compatRefusedTool) Invoke(context.Context, sources.Source, parameters.ParamValues, tools.AccessToken) (any, util.ToolboxError) {
	return nil, util.NewAgentError("the statement was refused", nil)
}

func compatPrimitives(t *testing.T) (map[string]tools.Tool, map[string]prompts.Prompt, map[string]group.Group) {
	t.Helper()
	authCfg := testutils.MockToolConfig{
		ConfigBase: tools.ConfigBase{Name: "auth_tool", Description: "needs auth", AuthRequired: []string{"my-auth"}},
		Type:       "mock-tool",
	}
	authTool, err := authCfg.Initialize(context.Background())
	if err != nil {
		t.Fatalf("auth tool: %s", err)
	}
	toolsMap := map[string]tools.Tool{
		"no_params":   testutils.MockTool1,
		"some_params": testutils.MockTool2,
		"array_param": testutils.MockTool3,
		"map_result":  compatMapTool{testutils.NewMockTool("map_result", "returns an object", "", nil, false, false)},
		"refused":     compatRefusedTool{testutils.NewMockTool("refused", "fails", "", nil, false, false)},
		"auth_tool":   authTool,
	}
	names := make([]string, 0, len(toolsMap))
	for name := range toolsMap {
		names = append(names, name)
	}
	sort.Strings(names)
	promptsMap := map[string]prompts.Prompt{
		"prompt1": testutils.MockPrompt1,
		"prompt2": testutils.MockPrompt2,
	}
	groups := map[string]group.Group{
		"":     group.NewGroup(group.GroupConfig{Name: "", ToolNames: names, PromptNames: []string{"prompt1", "prompt2"}}),
		"maps": group.NewGroup(group.GroupConfig{Name: "maps", ToolNames: []string{"map_result"}}),
	}
	return toolsMap, promptsMap, groups
}

type compatRequest struct {
	name    string
	url     string
	method  string
	params  map[string]any
	mcpName string
	notify  bool
}

func compatRequests(version string) []compatRequest {
	reqs := []compatRequest{
		{name: "tools/list", url: "/", method: "tools/list"},
		{name: "tools/list maps", url: "/maps", method: "tools/list"},
		{name: "tools/call no_params", url: "/", method: "tools/call", params: map[string]any{"name": "no_params"}},
		{name: "tools/call some_params", url: "/", method: "tools/call", params: map[string]any{"name": "some_params", "arguments": map[string]any{"param1": 1, "param2": 2}}},
		{name: "tools/call array_param", url: "/", method: "tools/call", params: map[string]any{"name": "array_param", "arguments": map[string]any{"my_array": []any{"a", "b"}}}},
		{name: "tools/call map_result", url: "/", method: "tools/call", params: map[string]any{"name": "map_result"}},
		{name: "tools/call map_result maps", url: "/maps", method: "tools/call", params: map[string]any{"name": "map_result"}},
		{name: "tools/call refused", url: "/", method: "tools/call", params: map[string]any{"name": "refused"}},
		{name: "tools/call auth_tool", url: "/", method: "tools/call", params: map[string]any{"name": "auth_tool"}},
		{name: "tools/call missing", url: "/", method: "tools/call", params: map[string]any{"name": "nope"}},
		{name: "tools/call outside group", url: "/maps", method: "tools/call", params: map[string]any{"name": "no_params"}},
		{name: "prompts/list", url: "/", method: "prompts/list"},
		{name: "prompts/get", url: "/", method: "prompts/get", params: map[string]any{"name": "prompt2", "arguments": map[string]any{"arg1": "x"}}},
		{name: "resources/list", url: "/", method: "resources/list"},
		{name: "resources/read", url: "/", method: "resources/read", params: map[string]any{"uri": "ui://bitquery/flow-graph"}},
		{name: "resources/templates/list", url: "/", method: "resources/templates/list"},
		{name: "unknown method", url: "/", method: "nothing/here"},
	}
	if version == protocolVersion20260728 {
		reqs = append(reqs, compatRequest{name: "server/discover", url: "/", method: "server/discover"})
	} else {
		reqs = append(reqs,
			compatRequest{name: "ping", url: "/", method: "ping"},
			compatRequest{name: "notifications/initialized", url: "/", method: "notifications/initialized", notify: true},
		)
	}
	for i := range reqs {
		switch name := reqs[i].params["name"]; {
		case name != nil:
			reqs[i].mcpName = name.(string)
		case reqs[i].params["uri"] != nil:
			reqs[i].mcpName = reqs[i].params["uri"].(string)
		}
	}
	return reqs
}

func compatBody(t *testing.T, id int, req compatRequest, version string) []byte {
	t.Helper()
	params := map[string]any{}
	for k, v := range req.params {
		params[k] = v
	}
	if version == protocolVersion20260728 {
		params["_meta"] = map[string]any{
			"io.modelcontextprotocol/protocolVersion":    version,
			"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "compat-client", "version": "1.0.0"},
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		}
	}
	msg := map[string]any{"jsonrpc": "2.0", "method": req.method}
	if !req.notify {
		msg["id"] = id
	}
	if len(params) > 0 {
		msg["params"] = params
	}
	body, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal request: %s", err)
	}
	return body
}

func compatHeaders(req compatRequest, version string) map[string]string {
	header := map[string]string{}
	switch version {
	case protocolVersion20241105:
	case protocolVersion20250326:
		header["Mcp-Session-Id"] = "compat-session"
	default:
		header["Mcp-Protocol-Version"] = version
	}
	if version == protocolVersion20260728 {
		header["Mcp-Method"] = req.method
		if req.mcpName != "" {
			header["Mcp-Name"] = req.mcpName
		}
	}
	return header
}

func initializeBody(t *testing.T, id int, version string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "compat-client", "version": "1.0.0"},
		},
	})
	if err != nil {
		t.Fatalf("marshal initialize: %s", err)
	}
	return body
}

// recordCompatAnswers drives the server through every request and returns the
// transcript the golden file holds.
func recordCompatAnswers(t *testing.T, opts ...func(*Server)) string {
	t.Helper()
	toolsMap, promptsMap, groups := compatPrimitives(t)
	var out strings.Builder
	versions := []string{protocolVersion20241105, protocolVersion20250326, protocolVersion20250618, protocolVersion20251125, protocolVersion20260728}

	for _, draft := range []bool{false, true} {
		var srv *Server
		serverOpts := append([]func(*Server){}, opts...)
		serverOpts = append(serverOpts, func(s *Server) { srv = s })
		if draft {
			serverOpts = append(serverOpts, withEnableDraftSpecs())
		}
		r, shutdown := setUpServer(t, "mcp", toolsMap, promptsMap, groups, serverOpts...)
		ts := runServer(r, false)
		label := "http"
		if draft {
			label = "http-draft"
		}
		id := 0

		for _, v := range append(append([]string{}, versions...), "1999-01-01") {
			id++
			resp, body, err := runRequest(ts, http.MethodPost, "/", bytes.NewReader(initializeBody(t, id, v)), nil)
			if err != nil {
				t.Fatalf("initialize %s: %s", v, err)
			}
			fmt.Fprintf(&out, "=== %s initialize %s\nstatus %d\n%s", label, v, resp.StatusCode, body)
		}
		for _, v := range versions {
			for _, req := range compatRequests(v) {
				id++
				resp, body, err := runRequest(ts, http.MethodPost, req.url, bytes.NewReader(compatBody(t, id, req, v)), compatHeaders(req, v))
				if err != nil {
					t.Fatalf("%s %s: %s", v, req.name, err)
				}
				fmt.Fprintf(&out, "=== %s %s %s\nstatus %d\n%s", label, v, req.name, resp.StatusCode, body)
			}
		}

		// stdio carries no header; the protocol is the one initialize agreed on.
		if !draft {
			ctx := util.WithLogger(context.Background(), srv.logger)
			for _, v := range []string{"", protocolVersion20251125, protocolVersion20260728} {
				for _, req := range compatRequests(v) {
					if req.url != "/" || req.notify {
						continue
					}
					id++
					_, res, _ := processMcpMessage(ctx, compatBody(t, id, req, v), srv, v, "", nil, "")
					var buf bytes.Buffer
					if err := NewStdioSession(srv, nil, &buf).write(ctx, res); err != nil {
						t.Fatalf("stdio %s %s: %s", v, req.name, err)
					}
					fmt.Fprintf(&out, "=== stdio %q %s\n%s", v, req.name, buf.String())
				}
			}
		}
		ts.Close()
		shutdown()
	}
	return out.String()
}

func TestBitqueryCompatGolden(t *testing.T) {
	got := recordCompatAnswers(t)
	if os.Getenv("UPDATE_COMPAT_GOLDEN") != "" {
		if err := os.MkdirAll(filepath.Dir(compatGoldenFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(compatGoldenFile, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", compatGoldenFile, len(got))
		return
	}
	want, err := os.ReadFile(compatGoldenFile)
	if err != nil {
		t.Fatalf("read golden: %s", err)
	}
	if got == string(want) {
		return
	}
	gotSections := strings.Split(got, "=== ")
	wantSections := strings.Split(string(want), "=== ")
	for i := 0; i < len(gotSections) && i < len(wantSections); i++ {
		if gotSections[i] != wantSections[i] {
			t.Fatalf("answer %d differs from the golden record:\n--- want\n%s\n--- got\n%s", i, wantSections[i], gotSections[i])
		}
	}
	t.Fatalf("the transcript has %d answers, the golden record %d", len(gotSections)-1, len(wantSections)-1)
}
