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

package tools

import (
	"context"

	"github.com/googleapis/mcp-toolbox/internal/sources"
)

// Bitquery: a tool normally talks to the ONE source it names, resolved by the
// server before every call. A tool that routes each call to one of a fixed set
// of sources (the http tool's bitquerySourceRoutes) needs the other sources too.
// The server hands them over once, at tool initialization, through the context,
// so none of the per-call source resolution paths has to change.

type sourceLookupKey struct{}

// SourceMap adapts the server's initialized sources to SourceManager.
type SourceMap map[string]sources.Source

// GetSource implements SourceManager.
func (m SourceMap) GetSource(name string) (sources.Source, bool) {
	s, ok := m[name]
	return s, ok
}

// WithSourceLookup returns a context that carries the initialized sources for
// tools that resolve more than their own source during Initialize.
func WithSourceLookup(ctx context.Context, m SourceManager) context.Context {
	return context.WithValue(ctx, sourceLookupKey{}, m)
}

// SourceLookupFromContext returns the sources set by WithSourceLookup. It is
// absent when the tools are initialized without sources (offline flows such as
// skills generation, which never invoke a tool).
func SourceLookupFromContext(ctx context.Context) (SourceManager, bool) {
	m, ok := ctx.Value(sourceLookupKey{}).(SourceManager)
	return m, ok && m != nil
}
