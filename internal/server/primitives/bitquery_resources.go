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

package primitives

import (
	"slices"
	"strings"

	"github.com/googleapis/mcp-toolbox/internal/auth"
	"github.com/googleapis/mcp-toolbox/internal/bitqueryresources"
	"github.com/googleapis/mcp-toolbox/internal/embeddingmodels"
	"github.com/googleapis/mcp-toolbox/internal/group"
	"github.com/googleapis/mcp-toolbox/internal/prompts"
	"github.com/googleapis/mcp-toolbox/internal/sources"
	"github.com/googleapis/mcp-toolbox/internal/tools"
)

// Bitquery: the MCP resources the server serves. They are the same on every
// endpoint (/mcp and /mcp/<toolset>); a config reload replaces them together
// with the tools that point at them.

// SetResources replaces the resources, keyed by URI.
func (r *PrimitiveManager) SetResources(resourcesMap map[string]bitqueryresources.Resource) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resources = resourcesMap
}

// SetPrimitivesWithResources is SetPrimitives that also replaces the
// resources, under the same lock, so no request sees the tools of one config
// with the resources of another.
func (r *PrimitiveManager) SetPrimitivesWithResources(sourcesMap map[string]sources.Source, authServicesMap map[string]auth.AuthService, embeddingModelsMap map[string]embeddingmodels.EmbeddingModel, toolsMap map[string]tools.Tool, promptsMap map[string]prompts.Prompt, groupsMap map[string]group.Group, resourcesMap map[string]bitqueryresources.Resource) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sources = sourcesMap
	r.authServices = authServicesMap
	r.embeddingModels = embeddingModelsMap
	r.tools = toolsMap
	r.prompts = promptsMap
	r.groups = groupsMap
	r.resources = resourcesMap
}

// GetResource returns the resource with the given URI.
func (r *PrimitiveManager) GetResource(uri string) (bitqueryresources.Resource, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	res, ok := r.resources[uri]
	return res, ok
}

// ListResources returns the resources, sorted by URI.
func (r *PrimitiveManager) ListResources() []bitqueryresources.Resource {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list := make([]bitqueryresources.Resource, 0, len(r.resources))
	for _, res := range r.resources {
		list = append(list, res)
	}
	slices.SortFunc(list, func(a, b bitqueryresources.Resource) int { return strings.Compare(a.URI, b.URI) })
	return list
}

// HasResources reports whether at least one resource is served.
func (r *PrimitiveManager) HasResources() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.resources) > 0
}
