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

package tools_test

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/googleapis/mcp-toolbox/internal/embeddingmodels"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

// Compile-time check: ConfigBase satisfies ToolMeta on its own.
var _ tools.ToolMeta = tools.ConfigBase{}

func newBaseTool() (tools.BaseTool[tools.ConfigBase], tools.Manifest) {
	cfg := tools.ConfigBase{
		Name:           "my-tool",
		Description:    "my tool description",
		AuthRequired:   []string{"google"},
		ScopesRequired: []string{"scope-a", "scope-b"},
	}
	manifest := tools.Manifest{
		Description:  "manifest description",
		AuthRequired: []string{"google"},
	}
	b := tools.NewBaseTool(
		cfg,
		tools.NewReadOnlyAnnotations(),
		manifest,
		parameters.Parameters{parameters.NewStringParameter("p1", "first param")},
	)
	return b, manifest
}

func TestBaseToolGetters(t *testing.T) {
	b, wantManifest := newBaseTool()

	if got, want := b.GetName(), "my-tool"; got != want {
		t.Errorf("GetName() = %q, want %q", got, want)
	}
	if got, want := b.GetDescription(), "my tool description"; got != want {
		t.Errorf("GetDescription() = %q, want %q", got, want)
	}
	if diff := cmp.Diff([]string{"google"}, b.GetAuthRequired()); diff != "" {
		t.Errorf("GetAuthRequired() mismatch (-want +got):\n%s", diff)
	}
	got := b.GetAnnotations()
	if got == nil || got.ReadOnlyHint == nil || !*got.ReadOnlyHint {
		t.Errorf("GetAnnotations() = %+v, want ReadOnlyHint=true", got)
	}
	gotManifest, err := b.Manifest(nil)
	if err != nil {
		t.Fatalf("Manifest() error = %v", err)
	}
	if diff := cmp.Diff(wantManifest, gotManifest); diff != "" {
		t.Errorf("Manifest() mismatch (-want +got):\n%s", diff)
	}
	p, err := b.GetParameters(nil)
	if err != nil {
		t.Fatalf("GetParameters() error = %v", err)
	}
	if len(p) != 1 || p[0].GetName() != "p1" {
		t.Errorf("GetParameters() = %+v, want one param named p1", p)
	}
	if diff := cmp.Diff([]string{"scope-a", "scope-b"}, b.GetScopesRequired()); diff != "" {
		t.Errorf("GetScopesRequired() mismatch (-want +got):\n%s", diff)
	}
}

func TestDefaultTitle(t *testing.T) {
	tcs := []struct {
		name string
		want string
	}{
		{"eth_transfers_in", "Eth Transfers In"},
		{"execute_sql", "Execute Sql"},
		{"my-tool", "My Tool"},
		{"list.tables", "List Tables"},
		{"already Titled", "Already Titled"},
		{"__double__underscore__", "Double Underscore"},
		{"único_nombre", "Único Nombre"},
		{"x", "X"},
		{"", ""},
		{"___", "___"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			if got := tools.DefaultTitle(tc.name); got != tc.want {
				t.Errorf("DefaultTitle(%q) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestTitleOrDefault(t *testing.T) {
	tcs := []struct {
		desc        string
		annotations *tools.ToolAnnotations
		want        string
	}{
		{"nil annotations fall back to the name", nil, "Eth Transfers In"},
		{"annotations without a title fall back to the name", tools.NewReadOnlyAnnotations(), "Eth Transfers In"},
		{"a blank title falls back to the name", &tools.ToolAnnotations{Title: " \t"}, "Eth Transfers In"},
		{"a set title wins", &tools.ToolAnnotations{Title: "Ethereum: incoming transfers"}, "Ethereum: incoming transfers"},
		{"a set title is trimmed", &tools.ToolAnnotations{Title: "  Incoming transfers "}, "Incoming transfers"},
	}
	for _, tc := range tcs {
		t.Run(tc.desc, func(t *testing.T) {
			if got := tools.TitleOrDefault("eth_transfers_in", tc.annotations); got != tc.want {
				t.Errorf("TitleOrDefault() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBaseToolAuthorized(t *testing.T) {
	tcs := []struct {
		desc         string
		authRequired []string
		verified     []string
		want         bool
	}{
		{"empty required is always authorized", nil, nil, true},
		{"empty required ignores verified", nil, []string{"foo"}, true},
		{"verified includes required", []string{"google"}, []string{"google", "github"}, true},
		{"verified missing required", []string{"google"}, []string{"github"}, false},
		{"verified empty when required non-empty", []string{"google"}, nil, false},
	}
	for _, tc := range tcs {
		t.Run(tc.desc, func(t *testing.T) {
			b := tools.NewBaseTool(tools.ConfigBase{AuthRequired: tc.authRequired}, nil, tools.Manifest{}, nil)
			if got := b.Authorized(tc.verified); got != tc.want {
				t.Errorf("Authorized(%v) = %v, want %v", tc.verified, got, tc.want)
			}
		})
	}
}

func TestBaseToolRequiresClientAuthorization(t *testing.T) {
	b := tools.NewBaseTool(tools.ConfigBase{}, nil, tools.Manifest{}, nil)
	got, err := b.RequiresClientAuthorization(nil)
	if err != nil {
		t.Fatalf("RequiresClientAuthorization() error = %v", err)
	}
	if got {
		t.Errorf("RequiresClientAuthorization() = true, want false")
	}
}

func TestBaseToolGetAuthTokenHeaderName(t *testing.T) {
	b := tools.NewBaseTool(tools.ConfigBase{}, nil, tools.Manifest{}, nil)
	got, err := b.GetAuthTokenHeaderName(nil)
	if err != nil {
		t.Fatalf("GetAuthTokenHeaderName() error = %v", err)
	}
	if got != "Authorization" {
		t.Errorf("GetAuthTokenHeaderName() = %q, want %q", got, "Authorization")
	}
}

func TestBaseToolEmbedParamsPassthrough(t *testing.T) {
	b := tools.NewBaseTool(
		tools.ConfigBase{},
		nil,
		tools.Manifest{},
		parameters.Parameters{parameters.NewStringParameter("p1", "first")},
	)
	values := parameters.ParamValues{{Name: "p1", Value: "hello"}}
	got, err := b.EmbedParams(context.Background(), values, map[string]embeddingmodels.EmbeddingModel{})
	if err != nil {
		t.Fatalf("EmbedParams() error = %v", err)
	}
	if diff := cmp.Diff(values, got); diff != "" {
		t.Errorf("EmbedParams() mismatch (-want +got):\n%s", diff)
	}
}
