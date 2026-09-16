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
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/googleapis/mcp-toolbox/internal/log"
	"github.com/googleapis/mcp-toolbox/internal/server/primitives"
	"github.com/googleapis/mcp-toolbox/internal/telemetry"
	"github.com/googleapis/mcp-toolbox/internal/testutils"
	"github.com/googleapis/mcp-toolbox/internal/util"
)

const testServerInstructions = "Multi-step workflows: call prompt_playbook.\n\n- Resolve a token first."

func withServerInstructions(text string) func(*Server) {
	return func(s *Server) {
		s.instructions.path = "_instructions.md"
		s.instructions.text.Store(&text)
	}
}

func writeInstructionsFile(t *testing.T, content []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "_instructions.md")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatalf("unable to write instructions file: %s", err)
	}
	return p
}

func instructionsTestContext(t *testing.T) (context.Context, log.Logger) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logger, err := log.NewStdLogger(io.Discard, io.Discard, "info")
	if err != nil {
		t.Fatalf("unable to initialize logger: %s", err)
	}
	ctx = util.WithLogger(ctx, logger)
	instrumentation, err := telemetry.CreateTelemetryInstrumentation(testutils.MockVersionString)
	if err != nil {
		t.Fatalf("unable to create instrumentation: %s", err)
	}
	ctx = util.WithInstrumentation(ctx, instrumentation)
	return ctx, logger
}

func TestResolveServerInstructionsFile(t *testing.T) {
	envWith := func(v string) func(string) string {
		return func(k string) string {
			if k == ServerInstructionsFileEnv {
				return v
			}
			return ""
		}
	}
	tcs := []struct {
		desc, flag, env, want string
	}{
		{desc: "not configured", flag: "", env: "", want: ""},
		{desc: "environment fallback", flag: "", env: "/env.md", want: "/env.md"},
		{desc: "flag only", flag: "/flag.md", env: "", want: "/flag.md"},
		{desc: "flag beats environment", flag: "/flag.md", env: "/env.md", want: "/flag.md"},
	}
	for _, tc := range tcs {
		t.Run(tc.desc, func(t *testing.T) {
			if got := ResolveServerInstructionsFile(tc.flag, envWith(tc.env)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
	if got := ResolveServerInstructionsFile("", nil); got != "" {
		t.Errorf("nil getenv: got %q, want empty", got)
	}
}

func TestLoadServerInstructions(t *testing.T) {
	atCap := strings.Repeat("a", MaxServerInstructionsBytes)
	tcs := []struct {
		desc    string
		content []byte
		path    string
		want    string
		wantErr string
	}{
		{desc: "empty path is not configured", path: "-", want: ""},
		{desc: "trailing whitespace trimmed, CRLF normalized, leading text kept",
			content: []byte("  # Title\r\nline two \r\n\r\n\t \n"), want: "  # Title\nline two"},
		{desc: "exactly at the cap", content: []byte(atCap + "\n\n"), want: atCap},
		{desc: "trailing whitespace does not count against the cap",
			content: []byte("a" + strings.Repeat(" ", 3*MaxServerInstructionsBytes)), want: "a"},
		{desc: "one byte over the cap", content: []byte(atCap + "b"), wantErr: "the limit is 8192 bytes"},
		{desc: "far over the read bound", content: bytes.Repeat([]byte("a"), maxServerInstructionsReadBytes+1), wantErr: "is larger than"},
		{desc: "empty file", content: []byte{}, wantErr: "is empty"},
		{desc: "whitespace only", content: []byte(" \n\t\r\n"), wantErr: "is empty"},
		{desc: "invalid UTF-8", content: []byte{'o', 'k', 0xff, 0xfe}, wantErr: "not valid UTF-8"},
		{desc: "missing file", path: filepath.Join(t.TempDir(), "missing.md"), wantErr: "unable to read server instructions file"},
		{desc: "directory", path: t.TempDir(), wantErr: "unable to read server instructions file"},
	}
	for _, tc := range tcs {
		t.Run(tc.desc, func(t *testing.T) {
			p := tc.path
			switch {
			case p == "-":
				p = ""
			case p == "":
				p = writeInstructionsFile(t, tc.content)
			}
			got, err := LoadServerInstructions(p)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %v, want one containing %q", err, tc.wantErr)
				}
				if got != "" {
					t.Errorf("text %q returned together with an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNewServerServerInstructions(t *testing.T) {
	good := writeInstructionsFile(t, []byte(testServerInstructions+"\n"))
	other := writeInstructionsFile(t, []byte("from the environment\n"))
	missing := filepath.Join(t.TempDir(), "missing.md")
	empty := writeInstructionsFile(t, []byte("\n  \n"))
	oversized := writeInstructionsFile(t, bytes.Repeat([]byte("x"), MaxServerInstructionsBytes+1))

	tcs := []struct {
		desc, flag, env, want, wantErr string
	}{
		{desc: "not configured", want: ""},
		{desc: "flag", flag: good, want: testServerInstructions},
		{desc: "environment fallback", env: other, want: "from the environment"},
		{desc: "flag beats environment", flag: good, env: missing, want: testServerInstructions},
		{desc: "missing file fails the start", env: missing, wantErr: "unable to read server instructions file"},
		{desc: "empty file fails the start", flag: empty, wantErr: "is empty"},
		{desc: "oversized file fails the start", flag: oversized, wantErr: "the limit is 8192 bytes"},
	}
	for _, tc := range tcs {
		t.Run(tc.desc, func(t *testing.T) {
			t.Setenv(ServerInstructionsFileEnv, tc.env)
			ctx, _ := instructionsTestContext(t)
			cfg := ServerConfig{
				Version:                testutils.MockVersionString,
				Address:                "127.0.0.1",
				AllowedHosts:           []string{"*"},
				ServerInstructionsFile: tc.flag,
			}
			s, err := NewServer(ctx, cfg)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			if got := s.ServerInstructions(); got != tc.want {
				t.Errorf("instructions %q, want %q", got, tc.want)
			}
		})
	}
}

// initializeResult posts one initialize (or, on the draft protocol, server/discover) request
// and returns the decoded `result` object.
func initializeResult(t *testing.T, post func(t *testing.T, path string, body []byte, header map[string]string) []byte, path, protocol string) map[string]any {
	t.Helper()
	var req map[string]any
	var header map[string]string
	if protocol == protocolVersion20260728 {
		req = map[string]any{
			"jsonrpc": jsonrpcVersion,
			"id":      "discover",
			"method":  "server/discover",
			"params": map[string]any{
				"_meta": map[string]any{
					"io.modelcontextprotocol/protocolVersion":    protocol,
					"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "client-name", "version": "client-version"},
					"io.modelcontextprotocol/clientCapabilities": map[string]any{},
				},
			},
		}
		header = map[string]string{"Mcp-Protocol-Version": protocol, "Mcp-Method": "server/discover"}
	} else {
		req = map[string]any{
			"jsonrpc": jsonrpcVersion,
			"id":      "initialize",
			"method":  "initialize",
			"params":  map[string]any{"protocolVersion": protocol},
		}
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("unable to marshal request: %s", err)
	}
	var got map[string]any
	raw := post(t, path, body, header)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unable to decode response %s: %s", raw, err)
	}
	result, ok := got["result"].(map[string]any)
	if !ok {
		t.Fatalf("response has no result: %s", raw)
	}
	if protocol != protocolVersion20260728 && result["protocolVersion"] != protocol {
		t.Fatalf("negotiated protocol %v, want %s", result["protocolVersion"], protocol)
	}
	return result
}

func TestMcpInitializeServerInstructions(t *testing.T) {
	protocols := []string{
		protocolVersion20241105,
		protocolVersion20250326,
		protocolVersion20250618,
		protocolVersion20251125,
		protocolVersion20260728, // draft: server/discover
	}
	mockTools := []testutils.MockTool{testutils.MockTool1, testutils.MockTool2}
	toolsMap, promptsMap, groups := testutils.SetUpResources(t, mockTools, nil)

	for _, configured := range []bool{true, false} {
		opts := []func(*Server){withEnableDraftSpecs()}
		state := "not configured"
		if configured {
			opts = append(opts, withServerInstructions(testServerInstructions))
			state = "configured"
		}
		r, shutdown := setUpServer(t, "mcp", toolsMap, promptsMap, groups, opts...)
		ts := runServer(r, false)
		post := func(t *testing.T, path string, body []byte, header map[string]string) []byte {
			t.Helper()
			resp, raw, err := runRequest(ts, http.MethodPost, path, bytes.NewBuffer(body), header)
			if err != nil {
				t.Fatalf("request failed: %s", err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d: %s", resp.StatusCode, raw)
			}
			return raw
		}
		// the whole server and a single toolset endpoint get the same text
		for _, path := range []string{"/", "/tool1_only"} {
			for _, protocol := range protocols {
				t.Run(state+" "+path+" "+protocol, func(t *testing.T) {
					result := initializeResult(t, post, path, protocol)
					got, present := result["instructions"]
					if !configured {
						if present {
							t.Fatalf("instructions present without configuration: %v", got)
						}
						return
					}
					if got != testServerInstructions {
						t.Fatalf("instructions %v, want %q", got, testServerInstructions)
					}
				})
			}
		}
		ts.Close()
		shutdown()
	}
}

// TestStdioInitializeServerInstructions drives the message path the stdio session uses (no
// header, no group).
func TestStdioInitializeServerInstructions(t *testing.T) {
	ctx, logger := instructionsTestContext(t)
	instrumentation, err := util.InstrumentationFromContext(ctx)
	if err != nil {
		t.Fatalf("no instrumentation: %s", err)
	}
	s := &Server{
		version:         testutils.MockVersionString,
		logger:          logger,
		instrumentation: instrumentation,
		PrimitiveMgr:    primitives.NewPrimitiveManager(nil, nil, nil, nil, nil, nil),
	}
	withServerInstructions(testServerInstructions)(s)

	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	_, res, err := processMcpMessage(ctx, body, s, "", "", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("unable to marshal response: %s", err)
	}
	var got struct {
		Result struct {
			Instructions *string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unable to decode %s: %s", raw, err)
	}
	if got.Result.Instructions == nil || *got.Result.Instructions != testServerInstructions {
		t.Fatalf("unexpected response: %s", raw)
	}
}

func TestReloadServerInstructions(t *testing.T) {
	ctx, logger := instructionsTestContext(t)

	unconfigured := &Server{}
	if err := unconfigured.ReloadServerInstructions(ctx); err != nil {
		t.Fatalf("reload without a configured file: %s", err)
	}
	if unconfigured.ServerInstructions() != "" || unconfigured.IsServerInstructionsFile("_instructions.md") {
		t.Fatal("an unconfigured server reports instructions")
	}

	p := writeInstructionsFile(t, []byte("first\n"))
	s := &Server{logger: logger}
	s.setServerInstructions(ctx, p, "first")

	write := func(content []byte) {
		t.Helper()
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatalf("unable to write: %s", err)
		}
	}
	write([]byte("second\n"))
	if err := s.ReloadServerInstructions(ctx); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if got := s.ServerInstructions(); got != "second" {
		t.Fatalf("after reload got %q, want %q", got, "second")
	}

	for desc, content := range map[string][]byte{
		"emptied":   {},
		"oversized": bytes.Repeat([]byte("x"), MaxServerInstructionsBytes+1),
	} {
		write(content)
		if err := s.ReloadServerInstructions(ctx); err == nil {
			t.Fatalf("%s: expected an error", desc)
		}
		if got := s.ServerInstructions(); got != "second" {
			t.Fatalf("%s: a failed reload replaced the text with %q", desc, got)
		}
	}
	if err := os.Remove(p); err != nil {
		t.Fatalf("unable to remove: %s", err)
	}
	if err := s.ReloadServerInstructions(ctx); err == nil {
		t.Fatal("removed file: expected an error")
	}
	if got := s.ServerInstructions(); got != "second" {
		t.Fatalf("removed file: a failed reload replaced the text with %q", got)
	}

	if !s.IsServerInstructionsFile(filepath.Join(filepath.Dir(p), ".", "_instructions.md")) {
		t.Error("the configured file is not recognized through a non-clean path")
	}
	if s.IsServerInstructionsFile(filepath.Join(filepath.Dir(p), "tools.yaml")) {
		t.Error("another file is recognized as the instructions file")
	}
}
