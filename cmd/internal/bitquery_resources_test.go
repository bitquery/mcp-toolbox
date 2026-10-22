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

package internal

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/googleapis/mcp-toolbox/internal/server"
	"github.com/googleapis/mcp-toolbox/internal/testutils"
)

// Bitquery: `kind: bitqueryResource` in both cfg formats, and across files.

func TestParseConfigBitqueryResources(t *testing.T) {
	ctx, err := testutils.ContextWithNewLogger()
	if err != nil {
		t.Fatal(err)
	}
	want := server.ResourceConfigs{"flow-graph": {Name: "flow-graph"}}
	for name, raw := range map[string]string{
		"flat":   "kind: bitqueryResource\nname: flow-graph\n",
		"nested": "bitqueryResources:\n  flow-graph: {}\n",
	} {
		t.Run(name, func(t *testing.T) {
			parser := ConfigParser{}
			cfg, err := parser.ParseConfig(ctx, []byte(raw))
			if err != nil {
				t.Fatalf("ParseConfig: %s", err)
			}
			if diff := cmp.Diff(want, cfg.Resources); diff != "" {
				t.Errorf("resources (-want +got):\n%s", diff)
			}
		})
	}

	parser := ConfigParser{}
	if _, err := parser.ParseConfig(ctx, []byte("bitqueryResources:\n  flow-graph:\n    uri: ui://bitquery/flow-graph\n")); err == nil || !strings.Contains(err.Error(), `unknown field "uri"`) {
		t.Errorf("a stray key in the nested format: err = %v", err)
	}
}

func TestMergeConfigsBitqueryResources(t *testing.T) {
	one := Config{Resources: server.ResourceConfigs{"flow-graph": {Name: "flow-graph"}}}
	merged, err := mergeConfigs(one, Config{})
	if err != nil {
		t.Fatalf("mergeConfigs: %s", err)
	}
	if diff := cmp.Diff(one.Resources, merged.Resources); diff != "" {
		t.Errorf("merged resources (-want +got):\n%s", diff)
	}
	if _, err := mergeConfigs(one, one); err == nil || !strings.Contains(err.Error(), "bitqueryResource 'flow-graph' (file #2)") {
		t.Errorf("a resource enabled in two files: err = %v", err)
	}
}
