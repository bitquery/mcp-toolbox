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

package clickhouse

// Bitquery: an `escape: single-quotes` template parameter reaches the SQL with
// backslashes doubled, and the labels-query-service hook, which reads the same
// escaped values, still extracts the same addresses.

import (
	"context"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/googleapis/mcp-toolbox/internal/bitquerylabels"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

func TestInvokeEscapedTemplateParamDoublesBackslashes(t *testing.T) {
	templateParams := parameters.Parameters{
		parameters.NewStringParameter("address", "address", parameters.WithStringEscape("single-quotes")),
		parameters.NewStringParameter("query", "query", parameters.WithStringEscape("single-quotes")),
	}
	tl, err := Config{
		ConfigBase:         tools.ConfigBase{Name: "labels_tool", Description: "d"},
		Type:               sqlType,
		Source:             "labels",
		Statement:          "SELECT 1 WHERE address = {{.address}} AND positionCaseInsensitiveUTF8(label, {{.query}}) > 0",
		TemplateParameters: templateParams,
	}.Initialize(context.Background())
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	tool := tl.(Tool)

	var events []string
	src := &hookSource{
		plainSource:  plainSource{events: &events},
		preprocessor: bitquerylabels.NewPreprocessor(&recordingClient{events: &events}, nil, ""),
	}
	values, err := parameters.ParseParams(templateParams, map[string]any{
		"address": hookTestAddress + `\`,
		"query":   `x\' OR 1=1 --`,
	}, nil)
	if err != nil {
		t.Fatalf("ParseParams: %v", err)
	}
	invokeHookTestTool(t, tool, src, values)

	want := []string{
		// the hook splits on non-alphanumerics: quotes and backslashes never reach the service
		"refresh:" + strings.ToLower(hookTestAddress) + "@" + strings.Join([]string{"ethereum", "bsc", "polygon", "arbitrum", "optimism", "base"}, "+"),
		"sql:SELECT 1 WHERE address = '" + hookTestAddress + `\\' AND positionCaseInsensitiveUTF8(label, 'x\\'' OR 1=1 --') > 0`,
	}
	if diff := cmp.Diff(want, events); diff != "" {
		t.Fatalf("events mismatch (-want +got):\n%s", diff)
	}
}
