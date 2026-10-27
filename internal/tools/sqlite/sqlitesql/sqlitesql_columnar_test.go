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

package sqlitesql_test

// Bitquery: the opt-in format=columnar parameter returns the rows as one
// columnar text block with no structured content; format=rows and an absent
// format keep today's decoded rows unchanged.

import (
	"context"
	"database/sql"
	"reflect"
	"testing"

	"github.com/googleapis/mcp-toolbox/internal/sources"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/tools/sqlite/sqlitesql"
	"github.com/googleapis/mcp-toolbox/internal/util/orderedmap"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

type mockColumnarSource struct{ rows any }

func (mockColumnarSource) SourceType() string             { return "sqlite" }
func (mockColumnarSource) ToConfig() sources.SourceConfig { return nil }
func (mockColumnarSource) SQLiteDB() *sql.DB              { return nil }
func (m mockColumnarSource) RunSQL(context.Context, string, []any) (any, error) {
	return m.rows, nil
}

func columnarSQLiteTool(t *testing.T) sqlitesql.Tool {
	t.Helper()
	cfg := sqlitesql.Config{
		ConfigBase: tools.ConfigBase{Name: "rows_tool", Description: "returns rows"},
		Type:       "sqlite-sql",
		Source:     "s",
		Statement:  "SELECT 1",
	}
	tl, err := cfg.Initialize(context.Background())
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return tl.(sqlitesql.Tool)
}

func TestColumnarSQLiteTool(t *testing.T) {
	ctx := context.Background()
	// The sqlite source returns []any of orderedmap.Row (not map[string]any);
	// Columnar must handle that, so the mock uses the real shape.
	rowsValue := []any{
		orderedmap.Row{Columns: []orderedmap.Column{{Name: "Id", Value: "1"}, {Name: "Name", Value: "a"}}},
		orderedmap.Row{Columns: []orderedmap.Column{{Name: "Id", Value: "2"}, {Name: "Name", Value: "b"}}},
	}
	src := mockColumnarSource{rows: rowsValue}
	tl := columnarSQLiteTool(t)

	// format absent: unchanged.
	got, terr := tl.Invoke(ctx, src, parameters.ParamValues{}, "")
	if terr != nil {
		t.Fatalf("invoke (absent): %v", terr)
	}
	if !reflect.DeepEqual(got, rowsValue) {
		t.Fatalf("absent format = %#v, want %#v", got, rowsValue)
	}

	// format=rows: unchanged.
	got, terr = tl.Invoke(ctx, src, parameters.ParamValues{{Name: "format", Value: "rows"}}, "")
	if terr != nil {
		t.Fatalf("invoke (rows): %v", terr)
	}
	if !reflect.DeepEqual(got, rowsValue) {
		t.Fatalf("format=rows = %#v, want unchanged", got)
	}

	// format=columnar: one verbatim TextResult, no structured content.
	got, terr = tl.Invoke(ctx, src, parameters.ParamValues{{Name: "format", Value: "columnar"}}, "")
	if terr != nil {
		t.Fatalf("invoke (columnar): %v", terr)
	}
	tr, ok := got.(tools.TextResult)
	if !ok {
		t.Fatalf("columnar result is %T, want tools.TextResult", got)
	}
	if want := `{"columns":["Id","Name"],"rows":[["1","a"],["2","b"]]}`; tr.Text != want {
		t.Errorf("columnar text = %s, want %s", tr.Text, want)
	}
	if _, structured := tools.SplitResult(got); structured != nil {
		t.Errorf("columnar carried structuredContent: %v", structured)
	}
}
