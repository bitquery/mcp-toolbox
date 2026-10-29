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

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/googleapis/mcp-toolbox/internal/testutils"
)

// cardTestServer builds the root composition the real server has: the card
// routes first, then /mcp mounted. Set the BITQUERY_SERVER_CARD_* environment
// before calling it — the identity is read when the routes are registered.
func cardTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	mockTools := []testutils.MockTool{testutils.MockTool1, testutils.MockTool2}
	toolsMap, promptsMap, groups := testutils.SetUpResources(t, mockTools, nil)
	var srv *Server
	mcpR, shutdown := setUpServer(t, "mcp", toolsMap, promptsMap, groups, func(s *Server) { srv = s })
	root := chi.NewRouter()
	registerBitqueryServerCard(context.Background(), root, srv, false)
	root.Mount("/mcp", mcpR)
	ts := runServer(root, false)
	t.Cleanup(func() { ts.Close(); shutdown() })
	return ts
}

// toolsListOverMCP is what a connected client sees at 2025-06-18.
func toolsListOverMCP(t *testing.T, ts *httptest.Server) []map[string]any {
	t.Helper()
	post := func(body map[string]any, header map[string]string) (*http.Response, []byte) {
		raw, _ := json.Marshal(body)
		resp, out, err := runRequest(ts, http.MethodPost, "/mcp/", bytes.NewBuffer(raw), header)
		if err != nil {
			t.Fatalf("request failed: %s", err)
		}
		return resp, out
	}
	resp, _ := post(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": protocolVersion20250618}}, nil)
	header := map[string]string{"MCP-Protocol-Version": protocolVersion20250618}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		header["Mcp-Session-Id"] = sid
	}
	post(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}, header)
	resp, out := post(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"}, header)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/list: status %d: %s", resp.StatusCode, out)
	}
	var got struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("tools/list: %s: %s", err, out)
	}
	if len(got.Result.Tools) == 0 {
		t.Fatalf("tools/list returned no tools: %s", out)
	}
	return got.Result.Tools
}

func getCard(t *testing.T, ts *httptest.Server, method, path string, header map[string]string) (*http.Response, []byte) {
	t.Helper()
	resp, body, err := runRequest(ts, method, path, nil, header)
	if err != nil {
		t.Fatalf("%s %s: %s", method, path, err)
	}
	return resp, body
}

func TestBitqueryServerCardListsWhatToolsListServes(t *testing.T) {
	t.Setenv(envServerCardName, "Bitquery: Crypto & Blockchain Data")
	t.Setenv(envServerCardVersion, "v9.9.9")
	t.Setenv(envServerCardAuthSchemes, "oauth2")
	ts := cardTestServer(t)

	resp, body := getCard(t, ts, http.MethodGet, bitqueryServerCardPath, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	for k, want := range map[string]string{
		"Content-Type":                  "application/json",
		"Access-Control-Allow-Origin":   "*",
		"Access-Control-Expose-Headers": "ETag",
		"Cache-Control":                 bitqueryServerCardCacheHeader,
	} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if resp.Header.Get("ETag") == "" {
		t.Errorf("no ETag")
	}
	if bytes.Contains(body, []byte(`"_meta"`)) {
		t.Errorf("the card must not carry _meta: %s", body)
	}

	var card struct {
		ServerInfo     map[string]any   `json:"serverInfo"`
		Authentication map[string]any   `json:"authentication"`
		Tools          []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(body, &card); err != nil {
		t.Fatalf("card is not JSON: %s", err)
	}
	if card.ServerInfo["name"] != "Bitquery: Crypto & Blockchain Data" || card.ServerInfo["version"] != "v9.9.9" {
		t.Errorf("serverInfo = %v", card.ServerInfo)
	}
	if card.Authentication["required"] != true || !reflect.DeepEqual(card.Authentication["schemes"], []any{"oauth2"}) {
		t.Errorf("authentication = %v", card.Authentication)
	}

	listed := toolsListOverMCP(t, ts)
	if len(card.Tools) != len(listed) {
		t.Fatalf("card lists %d tools, tools/list %d", len(card.Tools), len(listed))
	}
	for i, want := range listed {
		got := card.Tools[i]
		for _, k := range []string{"name", "title", "description", "inputSchema", "annotations"} {
			if !reflect.DeepEqual(got[k], want[k]) {
				t.Errorf("tool %v: %s differs: card %v, tools/list %v", want["name"], k, got[k], want[k])
			}
		}
	}
}

func TestBitqueryServerCardNoAuthBlockUnlessConfigured(t *testing.T) {
	ts := cardTestServer(t)
	resp, body := getCard(t, ts, http.MethodGet, bitqueryServerCardPath, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if bytes.Contains(body, []byte(`"authentication"`)) {
		t.Errorf("no auth schemes configured and no MCP auth, yet the card claims authentication: %s", body)
	}
	if !bytes.Contains(body, []byte(`"name":"Toolbox"`)) {
		t.Errorf("an unconfigured card falls back to the server name: %s", body)
	}
}

func TestBitqueryServerCardConditionalHeadAndMethods(t *testing.T) {
	ts := cardTestServer(t)
	resp, body := getCard(t, ts, http.MethodGet, bitqueryServerCardPath, nil)
	etag := resp.Header.Get("ETag")

	resp, notModified := getCard(t, ts, http.MethodGet, bitqueryServerCardPath, map[string]string{"If-None-Match": etag})
	if resp.StatusCode != http.StatusNotModified || len(notModified) != 0 {
		t.Errorf("If-None-Match %s: status %d, %d body bytes, want 304 and none", etag, resp.StatusCode, len(notModified))
	}

	resp, headBody := getCard(t, ts, http.MethodHead, bitqueryServerCardPath, nil)
	if resp.StatusCode != http.StatusOK || len(headBody) != 0 {
		t.Errorf("HEAD: status %d, %d body bytes", resp.StatusCode, len(headBody))
	}
	if resp.ContentLength != int64(len(body)) {
		t.Errorf("HEAD Content-Length %d, GET body %d", resp.ContentLength, len(body))
	}

	resp, _ = getCard(t, ts, http.MethodPost, bitqueryServerCardPath, nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST: status %d, want 405", resp.StatusCode)
	}
}

func TestBitquerySEP2127Card(t *testing.T) {
	t.Setenv(envServerCardName, "Bitquery: Crypto & Blockchain Data")
	t.Setenv(envServerCardID, "io.bitquery/mcp-server")
	t.Setenv(envServerCardDescription, "Blockchain data for AI agents")
	t.Setenv(envServerCardURL, "https://mcp.example.test/mcp")
	t.Setenv(envServerCardVersion, "v9.9.9")
	ts := cardTestServer(t)

	resp, body := getCard(t, ts, http.MethodGet, bitquerySEP2127CardPath, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != bitquerySEP2127CardMediaType {
		t.Errorf("Content-Type %q", ct)
	}
	var card map[string]any
	if err := json.Unmarshal(body, &card); err != nil {
		t.Fatalf("not JSON: %s", err)
	}
	for k, want := range map[string]any{
		"$schema": bitquerySEP2127SchemaURL, "name": "io.bitquery/mcp-server", "version": "v9.9.9",
		"description": "Blockchain data for AI agents", "title": "Bitquery: Crypto & Blockchain Data",
	} {
		if card[k] != want {
			t.Errorf("%s = %v, want %v", k, card[k], want)
		}
	}
	for _, k := range []string{"tools", "authentication", "serverInfo"} {
		if _, ok := card[k]; ok {
			t.Errorf("SEP-2127 has no %q", k)
		}
	}
	remotes, _ := card["remotes"].([]any)
	if len(remotes) != 1 {
		t.Fatalf("remotes = %v", card["remotes"])
	}
	remote := remotes[0].(map[string]any)
	versions, _ := remote["supportedProtocolVersions"].([]any)
	if remote["type"] != "streamable-http" || remote["url"] != "https://mcp.example.test/mcp" || !slices.Contains(versions, any(protocolVersion20250618)) {
		t.Errorf("remote = %v", remote)
	}

	// the static route beats the /mcp mount, and the mount still serves MCP
	if len(toolsListOverMCP(t, ts)) == 0 {
		t.Errorf("MCP behind the card route broke")
	}
}

func TestBitquerySEP2127CardNeedsItsIdentity(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"nothing set": {},
		"bad id":      {envServerCardID: "io.bitquery/mcp/server", envServerCardDescription: "x", envServerCardURL: "https://x.test/mcp"},
		"long text":   {envServerCardID: "io.bitquery/mcp", envServerCardDescription: strings.Repeat("x", 101), envServerCardURL: "https://x.test/mcp"},
	} {
		t.Run(name, func(t *testing.T) {
			for k, v := range env {
				t.Setenv(k, v)
			}
			ts := cardTestServer(t)
			resp, body := getCard(t, ts, http.MethodGet, bitquerySEP2127CardPath, nil)
			if resp.StatusCode == http.StatusOK || resp.Header.Get("Content-Type") == bitquerySEP2127CardMediaType {
				t.Errorf("an invalid SEP-2127 card was served: %d %s", resp.StatusCode, body)
			}
			// the Smithery card does not depend on it
			if resp, _ := getCard(t, ts, http.MethodGet, bitqueryServerCardPath, nil); resp.StatusCode != http.StatusOK {
				t.Errorf("static card: status %d", resp.StatusCode)
			}
		})
	}
}
