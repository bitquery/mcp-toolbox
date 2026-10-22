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

package util

import "github.com/googleapis/mcp-toolbox/internal/tools"

// Bitquery: MCP Apps support shared by every protocol version.

// RESOURCES_LIST, RESOURCES_READ and RESOURCES_TEMPLATES_LIST are the resources
// methods. A server answers them only while it serves a resource; otherwise
// they are unknown methods, as they always were.
const (
	RESOURCES_LIST           = "resources/list"
	RESOURCES_READ           = "resources/read"
	RESOURCES_TEMPLATES_LIST = "resources/templates/list"
)

// WithToolUIMeta adds a tool's `_meta.ui` to its `_meta` and keeps every key
// already there. A tool without one keeps its `_meta` as it is, nil included,
// so its manifest does not change.
func WithToolUIMeta(meta map[string]any, ui *tools.ToolUI) map[string]any {
	if ui == nil {
		return meta
	}
	if meta == nil {
		meta = make(map[string]any, 1)
	}
	meta["ui"] = *ui
	return meta
}
