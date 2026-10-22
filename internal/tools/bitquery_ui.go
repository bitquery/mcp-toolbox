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

package tools

import (
	"fmt"
	"slices"
	"strings"
)

// Bitquery: a tool's MCP Apps metadata (MCP Apps 2026-01-26, "Resource
// Discovery"). The cfg sets it with the fork-only key bitqueryUI on any tool
// whose config embeds ConfigBase:
//
//	bitqueryUI:
//	  resourceUri: ui://bitquery/flow-graph
//	  visibility: [model, app]
//
// and tools/list sends it as the tool's `_meta.ui`, in every protocol version,
// next to the toolbox's own `_meta` keys. A host that renders MCP Apps reads
// the resource with resources/read and shows the tool's results in it.

const (
	// UIVisibilityModel lets the model see and call the tool.
	UIVisibilityModel = "model"
	// UIVisibilityApp lets a view of this server call the tool.
	UIVisibilityApp = "app"
)

// UIScheme is the URI scheme of an MCP Apps view.
const UIScheme = "ui://"

// ToolUI is a tool's `_meta.ui`.
type ToolUI struct {
	// ResourceURI names the ui:// resource the host renders the tool's results
	// with.
	ResourceURI string `yaml:"resourceUri,omitempty" json:"resourceUri,omitempty"`
	// Visibility says who may use the tool: "model", "app" or both. Omitted,
	// it is both (the spec's default) and stays omitted on the wire.
	//
	// The server lists every tool of a group whatever its visibility, and a
	// tool stays callable through tools/call by group membership: the spec
	// puts the filtering on the host ("Resource Discovery": the host MUST NOT
	// put a tool in the model's tool list when its visibility does not
	// include "model", and MUST reject a tools/call from an app for a tool
	// whose visibility does not include "app"). No sentence there asks the
	// server to leave a tool out of tools/list.
	Visibility []string `yaml:"visibility,omitempty" json:"visibility,omitempty"`
}

// Validate checks the block on its own; that the resource is served is checked
// against the cfg when it loads.
func (u ToolUI) Validate() error {
	if u.ResourceURI == "" && u.Visibility == nil {
		return fmt.Errorf("bitqueryUI sets neither resourceUri nor visibility")
	}
	if u.ResourceURI != "" && !strings.HasPrefix(u.ResourceURI, UIScheme) {
		return fmt.Errorf("bitqueryUI.resourceUri %q must start with %s", u.ResourceURI, UIScheme)
	}
	if u.Visibility != nil && len(u.Visibility) == 0 {
		return fmt.Errorf("bitqueryUI.visibility is empty; omit it for the default %q", []string{UIVisibilityModel, UIVisibilityApp})
	}
	for i, v := range u.Visibility {
		if v != UIVisibilityModel && v != UIVisibilityApp {
			return fmt.Errorf("bitqueryUI.visibility %q is not %q or %q", v, UIVisibilityModel, UIVisibilityApp)
		}
		if slices.Contains(u.Visibility[:i], v) {
			return fmt.Errorf("bitqueryUI.visibility lists %q twice", v)
		}
	}
	return nil
}

// GetBitqueryUI returns the bitqueryUI block, or nil.
func (c ConfigBase) GetBitqueryUI() *ToolUI { return c.BitqueryUI }

// BitqueryUIOf returns the bitqueryUI block of a tool config, or nil when the
// config has none or does not embed ConfigBase.
func BitqueryUIOf(cfg ToolConfig) *ToolUI {
	if c, ok := cfg.(interface{ GetBitqueryUI() *ToolUI }); ok {
		return c.GetBitqueryUI()
	}
	return nil
}
