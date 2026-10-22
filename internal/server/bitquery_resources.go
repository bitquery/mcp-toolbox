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

// Bitquery-specific: MCP Apps. A cfg enables built-in resources with
// `kind: bitqueryResource` documents and points a tool at one with the tool key
// bitqueryUI (internal/tools/bitquery_ui.go). Both keys are fork-only, so the
// image that knows them ships before the cfg that uses them.
//
// A tool's bitqueryUI is checked when the cfg loads (at start-up and on every
// reload): its resourceUri must name a resource the cfg enables, wherever in
// the config folder the two documents sit. A server whose cfg enables no
// resource answers every request exactly as before: no `resources`
// capability, and the resources methods stay unknown methods.

package server

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/googleapis/mcp-toolbox/internal/bitqueryresources"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/util"
)

// ResourceConfigs are the `kind: bitqueryResource` documents of a cfg, by name.
type ResourceConfigs map[string]bitqueryresources.Config

// UnmarshalConfigWithResources is UnmarshalPrimitiveConfig that also returns the
// `kind: bitqueryResource` documents.
func UnmarshalConfigWithResources(ctx context.Context, raw []byte) (SourceConfigs, AuthServiceConfigs, EmbeddingModelConfigs, ToolConfigs, PromptConfigs, GroupConfigs, ResourceConfigs, error) {
	var resourceConfigs ResourceConfigs
	s, a, e, t, p, g, err := unmarshalPrimitiveConfig(ctx, raw, &resourceConfigs)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, err
	}
	return s, a, e, t, p, g, resourceConfigs, nil
}

// UnmarshalYAMLBitqueryResourceConfig decodes a `kind: bitqueryResource`
// document. Its name must be a built-in resource, and it takes no other key.
func UnmarshalYAMLBitqueryResourceConfig(ctx context.Context, name string, r map[string]any) (bitqueryresources.Config, error) {
	dec, err := util.NewStrictDecoder(r)
	if err != nil {
		return bitqueryresources.Config{}, fmt.Errorf("error creating decoder: %s", err)
	}
	c := bitqueryresources.Config{Name: name}
	if err := dec.DecodeContext(ctx, &c); err != nil {
		return bitqueryresources.Config{}, fmt.Errorf("unable to parse as %q: %w", name, err)
	}
	if _, err := c.Resolve(); err != nil {
		return bitqueryresources.Config{}, err
	}
	return c, nil
}

// InitializeResources returns the built-in resources the cfg enables, keyed by
// URI.
func InitializeResources(cfg ServerConfig) (map[string]bitqueryresources.Resource, error) {
	resourcesMap := make(map[string]bitqueryresources.Resource, len(cfg.ResourceConfigs))
	for name, rc := range cfg.ResourceConfigs {
		r, err := rc.Resolve()
		if err != nil {
			return nil, fmt.Errorf("unable to initialize %s %q: %w", bitqueryresources.Kind, name, err)
		}
		resourcesMap[r.URI] = r
	}
	return resourcesMap, nil
}

// validateBitqueryUI checks every tool's bitqueryUI against the resources the
// cfg enables.
func validateBitqueryUI(cfg ServerConfig) error {
	resourcesMap, err := InitializeResources(cfg)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(cfg.ToolConfigs))
	for name := range cfg.ToolConfigs {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		ui := tools.BitqueryUIOf(cfg.ToolConfigs[name])
		if ui == nil {
			continue
		}
		if err := ui.Validate(); err != nil {
			return fmt.Errorf("tool %q: %w", name, err)
		}
		if ui.ResourceURI == "" {
			continue
		}
		if _, ok := resourcesMap[ui.ResourceURI]; ok {
			continue
		}
		if r, ok := bitqueryresources.BuiltinByURI(ui.ResourceURI); ok {
			return fmt.Errorf("tool %q: bitqueryUI.resourceUri %q is not enabled; enable it with a document of kind %s and name %s", name, ui.ResourceURI, bitqueryresources.Kind, r.Name)
		}
		known := make([]string, 0)
		for _, n := range bitqueryresources.BuiltinNames() {
			r, _ := bitqueryresources.Builtin(n)
			known = append(known, r.URI)
		}
		return fmt.Errorf("tool %q: bitqueryUI.resourceUri %q is not a built-in resource; the built-in resources are: %s", name, ui.ResourceURI, strings.Join(known, ", "))
	}
	return nil
}

// logResources reports the resources the server serves; it stays silent when
// there are none, so a server without resources logs as before.
func logResources(ctx context.Context, resourcesMap map[string]bitqueryresources.Resource) {
	if len(resourcesMap) == 0 {
		return
	}
	l, err := util.LoggerFromContext(ctx)
	if err != nil {
		return
	}
	uris := make([]string, 0, len(resourcesMap))
	for uri := range resourcesMap {
		uris = append(uris, uri)
	}
	slices.Sort(uris)
	l.InfoContext(ctx, fmt.Sprintf("Initialized %d resources: %s", len(uris), strings.Join(uris, ", ")))
}
