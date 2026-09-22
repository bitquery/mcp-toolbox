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

// Bitquery: an `escape: single-quotes` body parameter reaches the POSTed SQL
// with backslashes doubled, so ClickHouse reads it as one literal; a routed
// parameter (no escape, quoted in the template) is sent exactly as given.

import (
	"testing"

	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

func TestBitqueryEscapedBodyParamDoublesBackslashes(t *testing.T) {
	f := newBitqueryFixture(t)
	cfg := bitqueryRoutedConfig()
	cfg.RequestBody = "SELECT name FROM system.tables WHERE database = '{{.database}}' AND positionCaseInsensitiveUTF8(name, {{.query}}) > 0"
	cfg.BodyParams = parameters.Parameters{
		parameters.NewStringParameter("database", "database"),
		parameters.NewStringParameter("query", "query", parameters.WithStringEscape("single-quotes")),
	}
	tl := f.tool(t, cfg)

	for _, tc := range []struct{ in, want string }{
		{`x\' OR 1=1 --`, `'x\\'' OR 1=1 --'`},
		{`abc\`, `'abc\\'`},
		{`o'neil`, `'o''neil'`},
		{"plain", `'plain'`},
	} {
		values, err := parameters.ParseParams(cfg.BodyParams, map[string]any{"database": "bsc_api", "query": tc.in}, nil)
		if err != nil {
			t.Fatalf("ParseParams(%q): %v", tc.in, err)
		}
		before := len(f.gateway.calls())
		if _, terr := tl.Invoke(f.ctx, f.sources["gw-eth"], values, tools.AccessToken("")); terr != nil {
			t.Fatalf("Invoke(%q): %v", tc.in, terr)
		}
		calls := f.gateway.calls()
		if len(calls) != before+1 {
			t.Fatalf("Invoke(%q): %d gateway calls, want %d", tc.in, len(calls), before+1)
		}
		got := calls[len(calls)-1]
		want := "SELECT name FROM system.tables WHERE database = 'bsc_api' AND positionCaseInsensitiveUTF8(name, " + tc.want + ") > 0"
		if got.body != want {
			t.Fatalf("Invoke(%q) body\n got %s\nwant %s", tc.in, got.body, want)
		}
		if got.database != "bsc_api" {
			t.Fatalf("Invoke(%q): routed to %q, want bsc_api", tc.in, got.database)
		}
	}
}
