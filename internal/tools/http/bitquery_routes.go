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

package http

// Bitquery: bitquerySourceRoutes sends each call of an http tool to one of a
// FIXED set of sources, chosen by the exact value of one string parameter:
//
//	source: clickhouse-eth-http       # must be one of the route targets
//	bitquerySourceRoutes:
//	  param: database
//	  sources:
//	    eth_api: clickhouse-eth-http
//	    bsc_api: clickhouse-bsc-http
//	    bitcoin: ch-btc1-http
//
// The keys are the allow-list. A value that is not a key is refused before any
// request is built. The match is exact and case-sensitive, with no trimming:
// the gateways behind these sources fold case and trim whitespace themselves,
// and they route database names no tool may reach, so the only safe comparison
// is byte equality against a list written in the config.
//
// Base URL, headers (including any routing header), query params and the
// billing query_id format all come from the chosen source. The parameter value
// itself reaches the request only through the tool's own body template.

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

// maxEchoedRouteValue caps how much of a refused value the error repeats.
const maxEchoedRouteValue = 64

// SourceRoutes is the YAML form of bitquerySourceRoutes.
type SourceRoutes struct {
	// Param is the name of a string parameter of the tool.
	Param string `yaml:"param"`
	// Sources maps each allowed value of Param to the name of an http source.
	Sources map[string]string `yaml:"sources"`
}

type sourceRoutes struct {
	param   string
	targets map[string]string
	allowed string
	// lookup is nil when the tools were initialized without sources (offline
	// flows that never invoke a tool); pick refuses every call then.
	lookup tools.SourceManager
}

// initSourceRoutes validates the routes against the tool's parameters and, when
// the server supplied its sources, checks every target exists and is an http
// source — so a typo fails at startup like a wrong `source:` does, not on the
// first call.
func (cfg Config) initSourceRoutes(ctx context.Context, params parameters.Parameters) (*sourceRoutes, error) {
	r := cfg.SourceRoutes
	if r == nil {
		return nil, nil
	}
	if r.Param == "" {
		return nil, fmt.Errorf("tool %q: bitquerySourceRoutes.param is required", cfg.Name)
	}
	if len(r.Sources) == 0 {
		return nil, fmt.Errorf("tool %q: bitquerySourceRoutes.sources is empty", cfg.Name)
	}
	var param parameters.Parameter
	for _, p := range params {
		if p.GetName() == r.Param {
			param = p
			break
		}
	}
	if param == nil {
		return nil, fmt.Errorf("tool %q: bitquerySourceRoutes.param %q is not a parameter of the tool", cfg.Name, r.Param)
	}
	if param.GetType() != "string" {
		return nil, fmt.Errorf("tool %q: bitquerySourceRoutes.param %q must be a string parameter, not %s", cfg.Name, r.Param, param.GetType())
	}
	// The escape is applied while the value is parsed, so an escaped parameter
	// reaches the route lookup already wrapped in quotes and would match no key:
	// every call would be refused. Quote it in the body template instead — the
	// value can only ever be one of the keys.
	if sp, ok := param.(*parameters.StringParameter); ok && sp.Escape != nil {
		return nil, fmt.Errorf("tool %q: bitquerySourceRoutes.param %q must not declare escape (quote it in the template)", cfg.Name, r.Param)
	}

	keys := make([]string, 0, len(r.Sources))
	names := make([]string, 0, len(r.Sources))
	for value, source := range r.Sources {
		if value == "" || source == "" {
			return nil, fmt.Errorf("tool %q: bitquerySourceRoutes.sources has an empty value or source name", cfg.Name)
		}
		keys = append(keys, value)
		if !slices.Contains(names, source) {
			names = append(names, source)
		}
	}
	slices.Sort(keys)
	slices.Sort(names)
	if !slices.Contains(names, cfg.Source) {
		return nil, fmt.Errorf("tool %q: source %q must be one of the bitquerySourceRoutes targets", cfg.Name, cfg.Source)
	}
	if d := param.GetDefault(); d != nil {
		if s, ok := d.(string); !ok || r.Sources[s] == "" {
			return nil, fmt.Errorf("tool %q: default of %q is not a bitquerySourceRoutes value", cfg.Name, r.Param)
		}
	}

	routes := &sourceRoutes{
		param:   r.Param,
		targets: r.Sources,
		allowed: strings.Join(keys, ", "),
	}
	if lookup, ok := tools.SourceLookupFromContext(ctx); ok {
		for _, name := range names {
			s, found := lookup.GetSource(name)
			if !found {
				return nil, fmt.Errorf("tool %q: unable to retrieve source %s named in bitquerySourceRoutes", cfg.Name, name)
			}
			if _, compatible := s.(compatibleSource); !compatible {
				return nil, fmt.Errorf("tool %q: source %s named in bitquerySourceRoutes is not an http source", cfg.Name, name)
			}
		}
		routes.lookup = lookup
	}
	return routes, nil
}

// pick returns the source for this call's parameter value.
func (r *sourceRoutes) pick(paramsMap map[string]any) (compatibleSource, error) {
	value, _ := paramsMap[r.param].(string)
	name, ok := r.targets[value]
	if !ok {
		echo := value
		if len(echo) > maxEchoedRouteValue {
			echo = echo[:maxEchoedRouteValue] + "..."
		}
		return nil, fmt.Errorf("%s %q is not available; use one of: %s", r.param, echo, r.allowed)
	}
	if r.lookup == nil {
		return nil, fmt.Errorf("%s routing is not initialized", r.param)
	}
	s, found := r.lookup.GetSource(name)
	if !found {
		return nil, fmt.Errorf("%s routing is not initialized", r.param)
	}
	source, compatible := s.(compatibleSource)
	if !compatible {
		return nil, fmt.Errorf("%s routing is not initialized", r.param)
	}
	return source, nil
}
