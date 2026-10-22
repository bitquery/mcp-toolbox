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

package bitqueryresources

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFlowGraph(t *testing.T) {
	r, ok := Builtin("flow-graph")
	if !ok {
		t.Fatal("flow-graph is not a built-in resource")
	}
	if r.URI != "ui://bitquery/flow-graph" {
		t.Errorf("URI = %q", r.URI)
	}
	// MCP Apps 2026-01-26, "UI Resource Format".
	if r.MimeType != "text/html;profile=mcp-app" {
		t.Errorf("MimeType = %q", r.MimeType)
	}
	if !strings.HasPrefix(r.Text, "<!doctype html>") || !strings.Contains(r.Text, "</html>") {
		t.Errorf("Text is not an HTML page: %q", r.Text)
	}
	if strings.Contains(r.Text, "\r") {
		t.Error("Text has a carriage return")
	}
	// No outside origin (the host's restrictive policy stands); only the
	// clipboard is asked for, because exports are copied.
	b, err := json.Marshal(r.Meta())
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"ui":{"permissions":{"clipboardWrite":{}}}}`; string(b) != want {
		t.Errorf("Meta() = %s, want %s", b, want)
	}
	if byURI, ok := BuiltinByURI(r.URI); !ok || byURI.Name != r.Name {
		t.Errorf("BuiltinByURI(%q) = %+v, %v", r.URI, byURI, ok)
	}
	if got := BuiltinNames(); len(got) != 1 || got[0] != "flow-graph" {
		t.Errorf("BuiltinNames() = %v", got)
	}
}

func TestMeta(t *testing.T) {
	border := false
	r := Resource{UI: &UIMeta{
		CSP:           &CSP{ConnectDomains: []string{"https://api.example.com"}, ResourceDomains: []string{"https://*.cdn.example.com"}},
		Permissions:   &Permissions{ClipboardWrite: &Permission{}},
		Domain:        "a.example.com",
		PrefersBorder: &border,
	}}
	b, err := json.Marshal(r.Meta())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"ui":{"csp":{"connectDomains":["https://api.example.com"],"resourceDomains":["https://*.cdn.example.com"]},"permissions":{"clipboardWrite":{}},"domain":"a.example.com","prefersBorder":false}}`
	if string(b) != want {
		t.Errorf("Meta() = %s, want %s", b, want)
	}
	if (Resource{}).Meta() != nil || (Resource{UI: &UIMeta{}}).Meta() != nil {
		t.Error("an empty UIMeta must not produce _meta")
	}
}

func TestConfigResolve(t *testing.T) {
	if r, err := (Config{Name: "flow-graph"}).Resolve(); err != nil || r.URI != "ui://bitquery/flow-graph" {
		t.Errorf("Resolve(flow-graph) = %+v, %v", r, err)
	}
	if _, err := (Config{Name: "sankey"}).Resolve(); err == nil || !strings.Contains(err.Error(), "the built-in resources are: flow-graph") {
		t.Errorf("Resolve(sankey) err = %v", err)
	}
}
