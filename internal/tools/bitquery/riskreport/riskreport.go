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

// Package riskreport is the bitquery-address-risk-report tool kind: one call
// returns an address's risk report — a summary, the entities it is connected
// to with amounts, the share of funds by entity type, a score with its formula,
// and flags — computed in Go from the chain's transfer tables and the labels
// source. The tool's configuration carries the whole method (risk profile,
// label map, thresholds), so recalibrating is a configuration change.
package riskreport

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	yaml "github.com/goccy/go-yaml"
	"github.com/googleapis/mcp-toolbox/internal/sources"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

const resourceType string = "bitquery-address-risk-report"

func init() {
	if !tools.Register(resourceType, newConfig) {
		panic(fmt.Sprintf("tool type %q already registered", resourceType))
	}
}

func newConfig(ctx context.Context, name string, decoder *yaml.Decoder) (tools.ToolConfig, error) {
	actual := Config{ConfigBase: tools.ConfigBase{Name: name}}
	if err := decoder.DecodeContext(ctx, &actual); err != nil {
		return nil, err
	}
	return actual, nil
}

// httpSource is a chain's transfer source (a ClickHouse HTTP endpoint).
type httpSource interface {
	HttpBaseURL() string
	HttpDefaultHeaders() map[string]string
	HttpQueryParams() map[string]string
	BitqueryLegacyBillingPaths() string
	RunRequest(context.Context, *http.Request) (any, error)
}

// sqlSource is the labels source.
type sqlSource interface {
	RunSQL(context.Context, string, parameters.ParamValues) (any, error)
}

type Config struct {
	tools.ConfigBase `yaml:",inline"`
	Type             string `yaml:"type" validate:"required"`
	// Source is the source the server resolves for every call; it must be one
	// of ChainSources.
	Source string `yaml:"source" validate:"required"`
	// ChainSources maps a chain to its transfer source.
	ChainSources map[string]string `yaml:"chainSources" validate:"required"`
	// LabelsSource is the labels source.
	LabelsSource string `yaml:"labelsSource" validate:"required"`
	// FreezeSource holds the stablecoin freeze table; until it is set the
	// frozen flag reads not_evaluated.
	FreezeSource string                 `yaml:"freezeSource"`
	Parameters   parameters.Parameters  `yaml:"parameters"`
	Annotations  *tools.ToolAnnotations `yaml:"annotations,omitempty"`
	MethodConfig `yaml:",inline"`
}

var _ tools.ToolConfig = Config{}

func (cfg Config) ToolConfigType() string {
	return resourceType
}

// paramTypes are the parameters the kind reads, with their types. chain and
// address are required; the rest are optional.
var paramTypes = map[string]string{
	"chain":          "string",
	"address":        "string",
	"asset":          "string",
	"direction":      "string",
	"view":           "string",
	"format":         "string",
	"after_time":     "string",
	"before_time":    "string",
	"min_amount_usd": "float",
	"max_hops":       "integer",
	"weights":        "map",
}

func (cfg Config) Initialize(ctx context.Context) (tools.Tool, error) {
	if cfg.Description == "" {
		return nil, fmt.Errorf("description is required for tool %q", cfg.Name)
	}
	m, err := compileMethod(cfg.MethodConfig)
	if err != nil {
		return nil, fmt.Errorf("tool %q: %w", cfg.Name, err)
	}
	if len(cfg.ChainSources) == 0 {
		return nil, fmt.Errorf("tool %q: chainSources is empty", cfg.Name)
	}
	found := false
	for chain, name := range cfg.ChainSources {
		if !slices.Contains(supportedChains, chain) {
			return nil, fmt.Errorf("tool %q: chainSources: chain %q is not supported (supported: %v)", cfg.Name, chain, supportedChains)
		}
		if name == cfg.Source {
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("tool %q: source %q must be one of chainSources", cfg.Name, cfg.Source)
	}

	allParameters, paramManifest, err := parameters.ProcessParameters(nil, cfg.Parameters)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, p := range allParameters {
		want, ok := paramTypes[p.GetName()]
		if !ok {
			return nil, fmt.Errorf("tool %q: parameter %q is not read by this kind", cfg.Name, p.GetName())
		}
		if p.GetType() != want {
			return nil, fmt.Errorf("tool %q: parameter %q must be of type %s, not %s", cfg.Name, p.GetName(), want, p.GetType())
		}
		seen[p.GetName()] = true
	}
	for _, req := range []string{"chain", "address"} {
		if !seen[req] {
			return nil, fmt.Errorf("tool %q: parameter %q is required", cfg.Name, req)
		}
	}

	t := Tool{
		BaseTool: tools.NewBaseTool(
			cfg,
			tools.GetAnnotationsOrDefault(cfg.Annotations, tools.NewReadOnlyAnnotations),
			tools.Manifest{Description: cfg.Description, Parameters: paramManifest, AuthRequired: cfg.AuthRequired},
			allParameters,
		),
		m: m,
	}
	// The other sources come from the server at start-up. Offline flows
	// (skills generation) initialize tools without them and never invoke one;
	// a call is refused then.
	if lookup, ok := tools.SourceLookupFromContext(ctx); ok {
		t.chains = map[string]httpSource{}
		for chain, name := range cfg.ChainSources {
			s, ok := lookup.GetSource(name)
			if !ok {
				return nil, fmt.Errorf("tool %q: unable to retrieve source %q for chain %s", cfg.Name, name, chain)
			}
			hs, ok := s.(httpSource)
			if !ok {
				return nil, fmt.Errorf("tool %q: source %q for chain %s is not an http source", cfg.Name, name, chain)
			}
			t.chains[chain] = hs
		}
		s, ok := lookup.GetSource(cfg.LabelsSource)
		if !ok {
			return nil, fmt.Errorf("tool %q: unable to retrieve labels source %q", cfg.Name, cfg.LabelsSource)
		}
		ls, ok := s.(sqlSource)
		if !ok {
			return nil, fmt.Errorf("tool %q: labels source %q cannot run SQL", cfg.Name, cfg.LabelsSource)
		}
		t.labels = ls
		if cfg.FreezeSource != "" {
			if _, ok := lookup.GetSource(cfg.FreezeSource); !ok {
				return nil, fmt.Errorf("tool %q: unable to retrieve freeze source %q", cfg.Name, cfg.FreezeSource)
			}
		}
	}
	return t, nil
}

// supportedChains are the chains a configuration may name. The direct view
// runs on Tron only for now; the others are refused at call time.
var supportedChains = []string{"tron", "bitcoin", "ethereum"}

var _ tools.Tool = Tool{}

type Tool struct {
	tools.BaseTool[Config]
	m      *method
	chains map[string]httpSource
	labels sqlSource
}

func (t Tool) GetSourceName() string {
	return t.Cfg.Source
}

func (t Tool) ToConfig() tools.ToolConfig {
	return t.Cfg
}

func (t Tool) ValidateSource(source sources.Source) error {
	if _, ok := source.(httpSource); !ok {
		return fmt.Errorf("invalid source for %q tool: source %q is not an http source", t.Cfg.Type, t.Cfg.Source)
	}
	return nil
}

func (t Tool) Invoke(ctx context.Context, _ sources.Source, params parameters.ParamValues, _ tools.AccessToken) (any, util.ToolboxError) {
	return t.run(ctx, params.AsMap())
}
