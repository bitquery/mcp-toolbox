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
	"sort"

	"github.com/googleapis/mcp-toolbox/internal/util/orderedmap"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

// Bitquery: a tool result with two readers. Since MCP 2025-06-18 a tools/call
// result may carry `structuredContent` next to `content`; an MCP Apps view
// renders the structured object while the model reads the text (MCP Apps
// 2026-01-26, "Data Passing": `content` is the "text representation for model
// context and text-only hosts", `structuredContent` is "structured data
// optimized for UI rendering (not added to model context)").
//
// A tool's Invoke returns a StructuredResult (or a pointer to one) to send
// both. Every other result goes out exactly as before: a []any becomes one text
// item per element, anything else one text item.

// StructuredResult is a tools/call result with a structured object.
type StructuredResult struct {
	// Content becomes the text content items: one per element, each the JSON
	// encoding of that element, exactly as a []any result does. When it is
	// empty, the one text item is the JSON encoding of Structured, so a client
	// that reads only `content` still gets the data ("Tools MUST return
	// meaningful content array even when UI is available").
	Content []any `json:"content"`
	// Structured goes out as `structuredContent` in the protocol versions that
	// have it (2025-06-18 and later); the older ones drop it.
	Structured map[string]any `json:"structuredContent,omitempty"`
}

func (r StructuredResult) items() []any {
	if len(r.Content) == 0 && r.Structured != nil {
		return []any{r.Structured}
	}
	return r.Content
}

// TextResult is a result delivered as a single text content item verbatim, with
// no JSON encoding and no structured content. A []any or any other result has
// each element JSON-encoded into its text item, so a string element becomes a
// quoted JSON literal (`"line"`), and a multi-line one keeps its newlines as
// `\n`. A tool whose result is already prose for the model — a rendered
// document — returns a TextResult so the text reaches the model and a text-only
// host as written, not as an escaped JSON string. Only this type opts into the
// raw path; every other string element stays JSON-encoded as before.
type TextResult struct {
	Text string
}

// SplitResult returns the elements that become a tools/call result's text
// content items, and its structured object (nil for a plain result).
func SplitResult(result any) ([]any, map[string]any) {
	switch r := result.(type) {
	case StructuredResult:
		return r.items(), r.Structured
	case *StructuredResult:
		if r != nil {
			return r.items(), r.Structured
		}
	case TextResult:
		return []any{r}, nil
	case *TextResult:
		if r != nil {
			return []any{*r}, nil
		}
	case []any:
		return r, nil
	}
	return []any{result}, nil
}

// PlainResult returns a result as a caller that knows nothing of structured
// content receives it: a StructuredResult becomes its content elements, any
// other result is returned unchanged.
func PlainResult(result any) any {
	switch r := result.(type) {
	case StructuredResult:
		return r.items()
	case *StructuredResult:
		if r != nil {
			return r.items()
		}
	case TextResult:
		return r.Text
	case *TextResult:
		if r != nil {
			return r.Text
		}
	}
	return result
}

// Bitquery: the opt-in `format` parameter. A tool that would otherwise send one
// text content item per row (one JSON object, so the column names repeat on
// every row) can instead return the rows as a single columnar block,
// {"columns":[...],"rows":[[...],...]}, that names the columns once. It is
// opt-in and the default is unchanged: with the parameter absent or set to
// "rows" the tool returns exactly what it returns today, byte for byte. The
// http and sqlite kinds inject this parameter (FormatParamName) and consume it
// locally — it is never forwarded to the database.
const (
	// FormatParamName is the reserved parameter name the http and sqlite kinds
	// consume. A kind injects it only when the tool does not already declare a
	// parameter of this name.
	FormatParamName = "format"
	// FormatRows is the default: one content item per row, as today.
	FormatRows = "rows"
	// FormatColumnar asks for the single columnar block.
	FormatColumnar = "columnar"
)

// NewFormatParameter builds the opt-in format parameter the http and sqlite
// kinds inject. Its default keeps today's one-item-per-row shape, so a call
// that does not set it is unchanged.
func NewFormatParameter() parameters.Parameter {
	return parameters.NewStringParameter(
		FormatParamName,
		`Shape of the returned data. "rows" (the default) returns one item per row. "columnar" returns a single item, {"columns":[...],"rows":[[value,...],...]}, naming the columns once and listing each row's values in that order; it is more compact for large results and carries the same values.`,
		parameters.WithStringAllowedValues([]any{FormatRows, FormatColumnar}),
		parameters.WithStringDefault(FormatRows),
	)
}

// HasFormatParam reports whether a parameter named FormatParamName is already
// declared, so a kind can skip injecting its own (injecting a duplicate would
// refuse the whole configuration at load).
func HasFormatParam(ps parameters.Parameters) bool {
	for _, p := range ps {
		if p.GetName() == FormatParamName {
			return true
		}
	}
	return false
}

// WantColumnar reports whether a call asked for the columnar block. The caller
// gates this on having injected the parameter itself, so a tool that declares
// its own `format` for another purpose is never reinterpreted.
func WantColumnar(paramsMap map[string]any) bool {
	f, _ := paramsMap[FormatParamName].(string)
	return f == FormatColumnar
}

// Columnar reshapes rows of maps into one columnar object delivered as a single
// text content item: {"columns":[sorted top-level keys],"rows":[[cell,...],...]}.
// It operates on the already-decoded values and lets encoding/json marshal the
// whole object, so every cell is byte-for-byte what the per-row path (one
// json.Marshal of the row map) emits today — HTML escaping on, a Go nil as JSON
// null, a nested value marshaled recursively, and a 64-bit id or raw amount that
// decoded to a Go string left as a quoted string, never coerced to a number. A
// status row (e.g. TOO_LARGE) is an ordinary row here: it carries the same
// top-level keys as every other row (a UNION ALL branch), so its status value
// lands in that column and the others carry their own values.
//
// ok is false — and the caller keeps today's per-row behaviour — unless the
// input is a non-empty []any whose every element is a map[string]any with the
// SAME set of top-level keys. A row whose key set differs is not reshaped:
// there is no single column list that holds it without inventing a null the
// per-row path never emitted, so falling back keeps "exactly as today" absolute
// rather than guessing.
func Columnar(rows []any) (TextResult, bool) {
	if len(rows) == 0 {
		return TextResult{}, false
	}
	maps := make([]map[string]any, len(rows))
	for i, r := range rows {
		m, ok := rowToMap(r)
		if !ok {
			return TextResult{}, false
		}
		maps[i] = m
	}

	columns := make([]string, 0, len(maps[0]))
	for k := range maps[0] {
		columns = append(columns, k)
	}
	sort.Strings(columns)

	// Every row must carry exactly the first row's key set: same cardinality
	// plus every column present means the sets are identical. Any difference
	// falls back to the per-row path rather than inventing a null.
	for _, m := range maps {
		if len(m) != len(columns) {
			return TextResult{}, false
		}
		for _, c := range columns {
			if _, has := m[c]; !has {
				return TextResult{}, false
			}
		}
	}

	cells := make([][]any, len(maps))
	for i, m := range maps {
		row := make([]any, len(columns))
		for j, c := range columns {
			row[j] = m[c]
		}
		cells[i] = row
	}

	// Marshal with plain json.Marshal (HTML escaping on) so each cell matches
	// the per-row path, which marshals the same decoded values the same way. A
	// value that cannot be marshalled (a non-finite float) fails here; the
	// caller then falls back to the per-row path, which fails identically.
	b, err := json.Marshal(map[string]any{"columns": columns, "rows": cells})
	if err != nil {
		return TextResult{}, false
	}
	return TextResult{Text: string(b)}, true
}

// rowToMap returns a decoded row as a name->value map. It accepts a plain
// map[string]any (the http tools' decoded rows, from encoding/json) and an
// orderedmap.Row (the sqlite source's rows); a Row whose column name repeats is
// rejected, so collapsing two columns into one never diverges from the per-row
// path. Any other shape returns false, which keeps the per-row behaviour.
func rowToMap(r any) (map[string]any, bool) {
	switch v := r.(type) {
	case map[string]any:
		return v, true
	case orderedmap.Row:
		return orderedRowToMap(v)
	case *orderedmap.Row:
		if v == nil {
			return nil, false
		}
		return orderedRowToMap(*v)
	}
	return nil, false
}

func orderedRowToMap(r orderedmap.Row) (map[string]any, bool) {
	m := make(map[string]any, len(r.Columns))
	for _, c := range r.Columns {
		if _, dup := m[c.Name]; dup {
			return nil, false
		}
		m[c.Name] = c.Value
	}
	return m, true
}
