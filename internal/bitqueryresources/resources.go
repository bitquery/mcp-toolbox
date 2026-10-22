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

// Package bitqueryresources holds the MCP resources the server can serve with
// no source behind them: pages compiled into the binary. None is served by
// default. A cfg enables each one with a document of its own:
//
//	kind: bitqueryResource
//	name: flow-graph
//
// or, in the nested format, `bitqueryResources: {flow-graph: {}}`. Only enabled
// resources are listed and read, and the server advertises the `resources`
// capability only when at least one is enabled. The kind is fork-only: an
// image that does not know it refuses the whole cfg, so the image ships first.
package bitqueryresources

import (
	"embed"
	"fmt"
	"slices"
	"strings"
)

//go:embed static
var static embed.FS

// Kind is the cfg document kind that enables a built-in resource.
const Kind = "bitqueryResource"

// AppMimeType is the MIME type of an MCP Apps view, an HTML page the host
// renders in a sandboxed iframe (MCP Apps 2026-01-26, "UI Resource Format": a
// view's `resources/read` content MUST carry it).
const AppMimeType = "text/html;profile=mcp-app"

// CSP lists the origins a view may reach beyond its own document (MCP Apps
// 2026-01-26, "UI Resource Format", UIResourceMeta.csp). An omitted list means
// none: no network requests, no external scripts, styles or images, no nested
// frames, and a base URI of the view's own origin.
type CSP struct {
	// ConnectDomains are the origins of fetch, XHR and WebSocket requests.
	ConnectDomains []string `json:"connectDomains,omitempty"`
	// ResourceDomains are the origins of scripts, images, styles, fonts and media.
	ResourceDomains []string `json:"resourceDomains,omitempty"`
	// FrameDomains are the origins of nested iframes.
	FrameDomains []string `json:"frameDomains,omitempty"`
	// BaseURIDomains are the origins the document's base URI may take.
	BaseURIDomains []string `json:"baseUriDomains,omitempty"`
}

// Permission is the empty object that asks the host for one sandbox permission.
type Permission struct{}

// Permissions are the sandbox permissions a view asks for. A host may refuse
// any of them.
type Permissions struct {
	Camera         *Permission `json:"camera,omitempty"`
	Microphone     *Permission `json:"microphone,omitempty"`
	Geolocation    *Permission `json:"geolocation,omitempty"`
	ClipboardWrite *Permission `json:"clipboardWrite,omitempty"`
}

// UIMeta is a view's `_meta.ui`. Every field is optional and its zero value is
// the safe default: no csp lets the host apply its restrictive policy, no
// permission is asked for, no domain leaves the host's default sandbox origin,
// and no prefersBorder leaves the border to the host.
type UIMeta struct {
	CSP           *CSP         `json:"csp,omitempty"`
	Permissions   *Permissions `json:"permissions,omitempty"`
	Domain        string       `json:"domain,omitempty"`
	PrefersBorder *bool        `json:"prefersBorder,omitempty"`
}

// Resource is one built-in resource.
type Resource struct {
	// Name is the name a cfg enables it by and its MCP `name`.
	Name string
	// URI identifies it in resources/list, resources/read and a tool's
	// `_meta.ui.resourceUri`.
	URI         string
	Title       string
	Description string
	MimeType    string
	// UI is the resource's `_meta.ui`, sent in resources/list and with the
	// content in resources/read (the read takes precedence for a host).
	UI *UIMeta
	// Text is the content.
	Text string
	file string
}

// Meta returns the resource's `_meta`, or nil when it carries nothing.
func (r Resource) Meta() map[string]any {
	if r.UI == nil || *r.UI == (UIMeta{}) {
		return nil
	}
	return map[string]any{"ui": r.UI}
}

var builtins = load([]Resource{
	{
		Name:        "flow-graph",
		URI:         "ui://bitquery/flow-graph",
		Title:       "Flow Graph",
		Description: "Interactive graph of the value flows between addresses.",
		MimeType:    AppMimeType,
		UI:          &UIMeta{},
		file:        "static/flow-graph.html",
	},
})

func load(resources []Resource) map[string]Resource {
	m := make(map[string]Resource, len(resources))
	for _, r := range resources {
		b, err := static.ReadFile(r.file)
		if err != nil {
			panic(fmt.Sprintf("built-in resource %q: %s", r.Name, err))
		}
		r.Text = string(b)
		m[r.Name] = r
	}
	return m
}

// Builtin returns the built-in resource of the given name.
func Builtin(name string) (Resource, bool) {
	r, ok := builtins[name]
	return r, ok
}

// BuiltinByURI returns the built-in resource with the given URI.
func BuiltinByURI(uri string) (Resource, bool) {
	for _, r := range builtins {
		if r.URI == uri {
			return r, true
		}
	}
	return Resource{}, false
}

// BuiltinNames returns the names of the built-in resources, sorted.
func BuiltinNames() []string {
	names := make([]string, 0, len(builtins))
	for name := range builtins {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Config is a `kind: bitqueryResource` document. Its name selects the built-in
// resource; it takes no other key.
type Config struct {
	Name string `yaml:"name" validate:"required"`
}

// Resolve returns the built-in resource the document enables.
func (c Config) Resolve() (Resource, error) {
	r, ok := Builtin(c.Name)
	if !ok {
		return Resource{}, fmt.Errorf("%q is not a built-in resource; the built-in resources are: %s", c.Name, strings.Join(BuiltinNames(), ", "))
	}
	return r, nil
}
