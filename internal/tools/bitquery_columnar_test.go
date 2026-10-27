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

package tools

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/googleapis/mcp-toolbox/internal/util/orderedmap"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

// colOut mirrors the columnar block. Rows holds each cell as its raw source
// bytes, so a test can compare a cell against json.Marshal of the same decoded
// value — cell-exact, which is the whole point of the format.
type colOut struct {
	Columns []string            `json:"columns"`
	Rows    [][]json.RawMessage `json:"rows"`
}

// TestColumnarByteIdentity is the load-bearing test: every cell of the columnar
// block is byte-for-byte json.Marshal of the decoded value the per-row path
// would have marshalled, for nulls, nested values, HTML-escaped strings, a
// status row, a Cursor and string-exact ids/amounts.
func TestColumnarByteIdentity(t *testing.T) {
	rowA := map[string]any{
		"Amount_Raw":    "123456789012345678901234567890", // must stay a quoted string
		"Id":            "9223372036854775807",            // 64-bit id, a string
		"Value":         float64(1.5),
		"Nothing":       nil,
		"Arguments":     map[string]any{"k": "v", "n": float64(2)},
		"Top_Receivers": []any{map[string]any{"addr": "0xABc", "amt": "10"}},
		"Note":          "a<b&c", // HTML-escaped the same as the per-row path
		"Cursor":        "eyJ0IjoxfQ==",
		"Status":        "OK",
	}
	// A status row (e.g. TOO_LARGE) carries the SAME keys as a normal row.
	rowB := map[string]any{
		"Amount_Raw":    nil,
		"Id":            nil,
		"Value":         nil,
		"Nothing":       nil,
		"Arguments":     nil,
		"Top_Receivers": nil,
		"Note":          nil,
		"Cursor":        "",
		"Status":        "TOO_LARGE",
	}
	input := []any{rowA, rowB}

	tr, ok := Columnar(input)
	if !ok {
		t.Fatal("Columnar returned ok=false for a uniform input")
	}

	var out colOut
	if err := json.Unmarshal([]byte(tr.Text), &out); err != nil {
		t.Fatalf("columnar text is not valid JSON: %s\n%s", err, tr.Text)
	}

	wantCols := []string{"Amount_Raw", "Arguments", "Cursor", "Id", "Note", "Nothing", "Status", "Top_Receivers", "Value"}
	if !reflect.DeepEqual(out.Columns, wantCols) {
		t.Fatalf("columns = %v, want sorted %v", out.Columns, wantCols)
	}
	if len(out.Rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(out.Rows))
	}

	// Every cell equals json.Marshal of the decoded value in that column.
	for i, src := range []map[string]any{rowA, rowB} {
		if len(out.Rows[i]) != len(wantCols) {
			t.Fatalf("row %d has %d cells, want %d", i, len(out.Rows[i]), len(wantCols))
		}
		for j, col := range out.Columns {
			want, err := json.Marshal(src[col])
			if err != nil {
				t.Fatalf("marshal want cell [%d][%s]: %s", i, col, err)
			}
			if got := string(out.Rows[i][j]); got != string(want) {
				t.Errorf("cell [%d][%s] = %s, want %s", i, col, got, want)
			}
		}
	}

	// Spell out the cases the brief names, so a regression reads clearly.
	cell := func(i int, col string) string {
		for j, c := range out.Columns {
			if c == col {
				return string(out.Rows[i][j])
			}
		}
		t.Fatalf("column %q not found", col)
		return ""
	}
	if got := cell(0, "Amount_Raw"); got != `"123456789012345678901234567890"` {
		t.Errorf("Amount_Raw cell = %s, want the quoted string (never a number)", got)
	}
	if got := cell(0, "Id"); got != `"9223372036854775807"` {
		t.Errorf("Id cell = %s, want the quoted 64-bit id", got)
	}
	// The Note cell is HTML-escaped exactly as the per-row path escapes it:
	// equal to json.Marshal of the value, with no raw < or & surviving.
	noteCell := cell(0, "Note")
	wantNote, _ := json.Marshal("a<b&c")
	if noteCell != string(wantNote) {
		t.Errorf("Note cell = %s, want %s (HTML-escaped like the per-row path)", noteCell, wantNote)
	}
	if strings.ContainsAny(noteCell, "<&") {
		t.Errorf("Note cell %s kept a raw < or &; HTML escaping is off", noteCell)
	}
	var noteDecoded string
	if err := json.Unmarshal([]byte(noteCell), &noteDecoded); err != nil || noteDecoded != "a<b&c" {
		t.Errorf("Note cell %s does not decode back to the value (err %v)", noteCell, err)
	}
	if got := cell(0, "Nothing"); got != "null" {
		t.Errorf("Nothing cell = %s, want null", got)
	}
	if got := cell(0, "Arguments"); got != `{"k":"v","n":2}` {
		t.Errorf("Arguments cell = %s, want the nested object", got)
	}
	if got := cell(1, "Status"); got != `"TOO_LARGE"` {
		t.Errorf("status row Status cell = %s", got)
	}
	if got := cell(1, "Amount_Raw"); got != "null" {
		t.Errorf("status row Amount_Raw = %s, want null", got)
	}

	// The object names columns before rows, deterministically.
	if tr.Text[:len(`{"columns":`)] != `{"columns":` {
		t.Errorf("columnar text does not start with the columns key: %s", tr.Text[:20])
	}
}

func TestColumnarSimple(t *testing.T) {
	tr, ok := Columnar([]any{
		map[string]any{"A": float64(1), "B": "x"},
		map[string]any{"A": float64(2), "B": "y"},
	})
	if !ok {
		t.Fatal("ok=false for a simple uniform input")
	}
	if want := `{"columns":["A","B"],"rows":[[1,"x"],[2,"y"]]}`; tr.Text != want {
		t.Errorf("columnar = %s, want %s", tr.Text, want)
	}
}

// TestColumnarOrderedRow covers the sqlite source's row type (orderedmap.Row):
// it is reshaped like a map, each cell byte-equal to json.Marshal of the value,
// and a row with a repeated column name falls back rather than collapsing.
func TestColumnarOrderedRow(t *testing.T) {
	rowA := orderedmap.Row{Columns: []orderedmap.Column{
		{Name: "B", Value: "x"},
		{Name: "A", Value: float64(1)},
		{Name: "Nested", Value: map[string]any{"k": nil}},
	}}
	rowB := orderedmap.Row{Columns: []orderedmap.Column{
		{Name: "B", Value: "y"},
		{Name: "A", Value: nil},
		{Name: "Nested", Value: nil},
	}}
	tr, ok := Columnar([]any{rowA, rowB})
	if !ok {
		t.Fatal("ok=false for orderedmap.Row input")
	}
	var out colOut
	if err := json.Unmarshal([]byte(tr.Text), &out); err != nil {
		t.Fatalf("not valid JSON: %s", err)
	}
	if !reflect.DeepEqual(out.Columns, []string{"A", "B", "Nested"}) {
		t.Fatalf("columns = %v, want sorted [A B Nested]", out.Columns)
	}
	// Byte-identity against json.Marshal of each column's value.
	for i, src := range []orderedmap.Row{rowA, rowB} {
		byName := map[string]any{}
		for _, c := range src.Columns {
			byName[c.Name] = c.Value
		}
		for j, col := range out.Columns {
			want, _ := json.Marshal(byName[col])
			if got := string(out.Rows[i][j]); got != string(want) {
				t.Errorf("row %d col %s = %s, want %s", i, col, got, want)
			}
		}
	}

	// A repeated column name falls back (never collapses two columns into one).
	dup := orderedmap.Row{Columns: []orderedmap.Column{{Name: "A", Value: 1}, {Name: "A", Value: 2}}}
	if _, ok := Columnar([]any{dup}); ok {
		t.Error("ok=true for a row with a duplicate column name; want fallback")
	}
}

func TestColumnarFallback(t *testing.T) {
	cases := []struct {
		name string
		in   []any
	}{
		{"empty", []any{}},
		{"nil slice", nil},
		{"non-map element", []any{"a string row"}},
		{"second element not a map", []any{map[string]any{"A": 1}, "x"}},
		{"different keys same count", []any{map[string]any{"A": 1, "B": 2}, map[string]any{"A": 1, "C": 3}}},
		{"shorter second row", []any{map[string]any{"A": 1, "B": 2}, map[string]any{"A": 1}}},
		{"longer second row", []any{map[string]any{"A": 1}, map[string]any{"A": 1, "B": 2}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tr, ok := Columnar(tc.in); ok {
				t.Errorf("ok=true for %s; want fallback (text=%s)", tc.name, tr.Text)
			}
		})
	}
}

// TestColumnarRidesTextResult confirms the columnar block travels the same
// single-verbatim-item path as a rendered document: SplitResult yields one
// element and no structured content.
func TestColumnarRidesTextResult(t *testing.T) {
	tr, ok := Columnar([]any{map[string]any{"A": float64(1)}})
	if !ok {
		t.Fatal("ok=false")
	}
	items, structured := SplitResult(tr)
	if structured != nil {
		t.Errorf("columnar carried structuredContent: %v", structured)
	}
	if len(items) != 1 {
		t.Fatalf("SplitResult returned %d items, want 1", len(items))
	}
	if _, isText := items[0].(TextResult); !isText {
		t.Errorf("SplitResult item is %T, want tools.TextResult", items[0])
	}
}

func TestFormatParameter(t *testing.T) {
	p := NewFormatParameter()
	if p.GetName() != FormatParamName {
		t.Errorf("name = %q, want %q", p.GetName(), FormatParamName)
	}
	if p.GetType() != "string" {
		t.Errorf("type = %q, want string", p.GetType())
	}
	if d, _ := p.GetDefault().(string); d != FormatRows {
		t.Errorf("default = %v, want %q", p.GetDefault(), FormatRows)
	}
	if _, err := p.Parse(FormatColumnar); err != nil {
		t.Errorf("columnar rejected: %s", err)
	}
	if _, err := p.Parse(FormatRows); err != nil {
		t.Errorf("rows rejected: %s", err)
	}
	if _, err := p.Parse("csv"); err == nil {
		t.Error("csv accepted; the enum must refuse it")
	}
}

func TestWantColumnarAndHasFormatParam(t *testing.T) {
	if !WantColumnar(map[string]any{FormatParamName: FormatColumnar}) {
		t.Error("WantColumnar false for columnar")
	}
	if WantColumnar(map[string]any{FormatParamName: FormatRows}) {
		t.Error("WantColumnar true for rows")
	}
	if WantColumnar(map[string]any{}) {
		t.Error("WantColumnar true when absent")
	}
	if !HasFormatParam(parameters.Parameters{NewFormatParameter()}) {
		t.Error("HasFormatParam false when format is declared")
	}
	if HasFormatParam(parameters.Parameters{parameters.NewStringParameter("other", "d")}) {
		t.Error("HasFormatParam true when format is not declared")
	}
}
