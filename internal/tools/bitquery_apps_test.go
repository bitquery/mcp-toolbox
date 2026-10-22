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

package tools_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/googleapis/mcp-toolbox/internal/tools"
)

func TestSplitResult(t *testing.T) {
	structured := map[string]any{"edges": 2}
	for name, tc := range map[string]struct {
		result         any
		wantItems      []any
		wantStructured map[string]any
	}{
		"list":                 {[]any{"a", 1}, []any{"a", 1}, nil},
		"object":               {map[string]any{"a": 1}, []any{map[string]any{"a": 1}}, nil},
		"nil":                  {nil, []any{nil}, nil},
		"structured":           {tools.StructuredResult{Content: []any{"two edges"}, Structured: structured}, []any{"two edges"}, structured},
		"structured pointer":   {&tools.StructuredResult{Content: []any{"two edges"}, Structured: structured}, []any{"two edges"}, structured},
		"structured, no text":  {tools.StructuredResult{Structured: structured}, []any{structured}, structured},
		"nil structured ptr":   {(*tools.StructuredResult)(nil), []any{(*tools.StructuredResult)(nil)}, nil},
		"structured, no parts": {tools.StructuredResult{}, nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			items, got := tools.SplitResult(tc.result)
			if diff := cmp.Diff(tc.wantItems, items); diff != "" {
				t.Errorf("items (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.wantStructured, got); diff != "" {
				t.Errorf("structured (-want +got):\n%s", diff)
			}
		})
	}
}

func TestPlainResult(t *testing.T) {
	plain := map[string]any{"a": 1}
	if diff := cmp.Diff(plain, tools.PlainResult(plain)); diff != "" {
		t.Errorf("a plain result changed (-want +got):\n%s", diff)
	}
	got, err := json.Marshal(tools.PlainResult(tools.StructuredResult{Content: []any{"x"}, Structured: plain}))
	if err != nil || string(got) != `["x"]` {
		t.Errorf("PlainResult(structured) = %s, %v", got, err)
	}
}

func TestToolUIValidate(t *testing.T) {
	for name, tc := range map[string]struct {
		ui   tools.ToolUI
		want string
	}{
		"resource":            {tools.ToolUI{ResourceURI: "ui://bitquery/flow-graph"}, ""},
		"resource and both":   {tools.ToolUI{ResourceURI: "ui://bitquery/flow-graph", Visibility: []string{"model", "app"}}, ""},
		"app only":            {tools.ToolUI{Visibility: []string{"app"}}, ""},
		"nothing":             {tools.ToolUI{}, "sets neither"},
		"other scheme":        {tools.ToolUI{ResourceURI: "https://example.com"}, "must start with ui://"},
		"empty visibility":    {tools.ToolUI{Visibility: []string{}}, "is empty"},
		"unknown visibility":  {tools.ToolUI{Visibility: []string{"user"}}, `"user" is not`},
		"repeated visibility": {tools.ToolUI{Visibility: []string{"model", "model"}}, "twice"},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.ui.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %s", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

type configWithoutBase struct{}

func (configWithoutBase) ToolConfigType() string { return "none" }

func (configWithoutBase) Initialize(context.Context) (tools.Tool, error) { return nil, nil }

type configWithBase struct {
	tools.ConfigBase
}

func (configWithBase) ToolConfigType() string { return "base" }

func (configWithBase) Initialize(context.Context) (tools.Tool, error) { return nil, nil }

func TestBitqueryUIOf(t *testing.T) {
	ui := &tools.ToolUI{ResourceURI: "ui://bitquery/flow-graph"}
	if got := tools.BitqueryUIOf(configWithBase{tools.ConfigBase{BitqueryUI: ui}}); got != ui {
		t.Errorf("BitqueryUIOf = %v", got)
	}
	if got := tools.BitqueryUIOf(configWithBase{}); got != nil {
		t.Errorf("BitqueryUIOf without the block = %v", got)
	}
	if got := tools.BitqueryUIOf(configWithoutBase{}); got != nil {
		t.Errorf("BitqueryUIOf of a config without ConfigBase = %v", got)
	}
	b, err := json.Marshal(tools.ToolUI{Visibility: []string{"app"}})
	if err != nil || string(b) != `{"visibility":["app"]}` {
		t.Errorf("ToolUI JSON = %s, %v", b, err)
	}
}
