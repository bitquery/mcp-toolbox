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

// Package flowgraph is the bitquery-flow-graph tool kind: it computes, in Go from a
// chain's transfer tables and the labels source, the funds flowing between an
// address set and its counterparties — in USD, with labels, and totalled by label
// type. It reuses the shared statement runner in internal/tools/bitquery/chainexec.
// The first tool of this kind is counterparties (BIT-16068).
//
// This file registers the kind as a skeleton so its configuration and wiring are in
// place; the method is built in a following change, so an invoke is refused until
// then. No configuration uses the kind yet, so the registration is inert (it adds no
// tool to any toolset and does not change tools/list).
package flowgraph

import (
	"context"
	"fmt"

	yaml "github.com/goccy/go-yaml"
	"github.com/googleapis/mcp-toolbox/internal/sources"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/tools/bitquery/chainexec"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

const resourceType string = "bitquery-flow-graph"

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

// Config is the kind's configuration. Like the risk report, a tool names one chain
// source per call and the labels source; ChainSources maps each supported chain to
// its transfer source. The method parameters are built in a following change.
type Config struct {
	tools.ConfigBase `yaml:",inline"`
	Type             string                 `yaml:"type" validate:"required"`
	Source           string                 `yaml:"source" validate:"required"`
	ChainSources     map[string]string      `yaml:"chainSources" validate:"required"`
	LabelsSource     string                 `yaml:"labelsSource" validate:"required"`
	Parameters       parameters.Parameters  `yaml:"parameters"`
	Annotations      *tools.ToolAnnotations `yaml:"annotations,omitempty"`
}

var _ tools.ToolConfig = Config{}

func (cfg Config) ToolConfigType() string {
	return resourceType
}

func (cfg Config) Initialize(ctx context.Context) (tools.Tool, error) {
	if cfg.Description == "" {
		return nil, fmt.Errorf("description is required for tool %q", cfg.Name)
	}
	allParameters, paramManifest, err := parameters.ProcessParameters(nil, cfg.Parameters)
	if err != nil {
		return nil, err
	}
	return Tool{
		BaseTool: tools.NewBaseTool(
			cfg,
			tools.GetAnnotationsOrDefault(cfg.Annotations, tools.NewReadOnlyAnnotations),
			tools.Manifest{Description: cfg.Description, Parameters: paramManifest, AuthRequired: cfg.AuthRequired},
			allParameters,
		),
	}, nil
}

var _ tools.Tool = Tool{}

type Tool struct {
	tools.BaseTool[Config]
}

func (t Tool) GetSourceName() string {
	return t.Cfg.Source
}

func (t Tool) ToConfig() tools.ToolConfig {
	return t.Cfg
}

func (t Tool) ValidateSource(source sources.Source) error {
	// The runner (chainexec.Exec) issues HTTP statements against the chain source,
	// so the resolved source must be an http source — the same check the risk report
	// makes, and the first use of chainexec by this kind.
	if _, ok := source.(chainexec.HTTPSource); !ok {
		return fmt.Errorf("invalid source for %q tool: source %q is not an http source", t.Cfg.Type, t.Cfg.Source)
	}
	return nil
}

func (t Tool) Invoke(ctx context.Context, _ sources.Source, _ parameters.ParamValues, _ tools.AccessToken) (any, util.ToolboxError) {
	return nil, util.NewAgentError("the bitquery-flow-graph tool is not implemented yet", nil)
}
