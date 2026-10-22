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

package primitives_test

import (
	"testing"

	"github.com/googleapis/mcp-toolbox/internal/bitqueryresources"
	"github.com/googleapis/mcp-toolbox/internal/server/primitives"
	"github.com/googleapis/mcp-toolbox/internal/tools"
)

func TestResources(t *testing.T) {
	mgr := primitives.NewPrimitiveManager(nil, nil, nil, nil, nil, nil)
	if mgr.HasResources() || len(mgr.ListResources()) != 0 {
		t.Fatal("a new manager serves resources")
	}
	b := bitqueryresources.Resource{Name: "b", URI: "ui://x/b"}
	a := bitqueryresources.Resource{Name: "a", URI: "ui://x/a"}
	mgr.SetResources(map[string]bitqueryresources.Resource{b.URI: b, a.URI: a})
	if !mgr.HasResources() {
		t.Fatal("HasResources() = false")
	}
	if list := mgr.ListResources(); len(list) != 2 || list[0].URI != a.URI || list[1].URI != b.URI {
		t.Errorf("ListResources() = %+v, want sorted by URI", list)
	}
	if got, ok := mgr.GetResource(b.URI); !ok || got.Name != "b" {
		t.Errorf("GetResource(%q) = %+v, %v", b.URI, got, ok)
	}

	// A reload replaces the tools and the resources together.
	toolsMap := map[string]tools.Tool{"t": nil}
	mgr.SetPrimitivesWithResources(nil, nil, nil, toolsMap, nil, nil, nil)
	if mgr.HasResources() {
		t.Error("the reload kept the old resources")
	}
	if _, ok := mgr.GetTool("t"); !ok {
		t.Error("the reload dropped the tools")
	}
}
