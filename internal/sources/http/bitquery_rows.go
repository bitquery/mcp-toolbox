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

package http

// Bitquery: ClickHouse writes JSONEachRow as one JSON object per line. Servers
// that know output_format_json_array_of_rows wrap those lines in an array, which
// is one JSON document; older servers reject that setting, so their body is a
// bare row stream. A single json.Unmarshal cannot read a stream, and the result
// shape used to depend on the row count: "" for no rows, a bare object for one
// row, and the whole body as one escaped string for two or more. The helpers
// below give a row stream the same []any the array form produces.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// clickHouseFormatHeader carries the output format of a ClickHouse HTTP answer.
const clickHouseFormatHeader = "X-ClickHouse-Format"

// isJSONRowStreamFormat reports whether a ClickHouse output format writes one
// JSON object per row, one row per line.
func isJSONRowStreamFormat(format string) bool {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "jsoneachrow", "jsonstringseachrow", "jsonlines", "ndjson":
		return true
	}
	return false
}

// decodeResponseBody turns a successful response body into the tool result.
//
//   - A body that is one JSON value is returned as that value, exactly as
//     before. The one exception is a ClickHouse row stream (see
//     isJSONRowStreamFormat) holding a single row: it comes back as a one-row
//     []any, the same thing the array form of that answer parses into.
//   - An empty (or whitespace-only) ClickHouse row stream is zero rows: []any{}.
//     Any other empty body is still returned as the string it is.
//   - Two or more JSON objects separated by newlines become []any, one element
//     per object, whatever the headers say.
//   - Everything else (plain text, HTML, a stream with a broken line or a
//     trailing ClickHouse exception, values that are not objects, several
//     values on one line) is returned as the raw string, as before.
func decodeResponseBody(body []byte, header http.Header) any {
	rowStream := isJSONRowStreamFormat(header.Get(clickHouseFormatHeader))

	// One JSON document: the common case, and the only one with no extra work.
	var data any
	if err := json.Unmarshal(body, &data); err == nil {
		if _, isRow := data.(map[string]any); isRow && rowStream {
			return []any{data}
		}
		return data
	}

	if len(skipJSONSpace(body)) == 0 {
		if rowStream {
			return []any{}
		}
		return string(body)
	}

	if rows, ok := decodeObjectLines(body); ok {
		return rows
	}
	return string(body)
}

// decodeObjectLines decodes a body made of two or more JSON objects, each
// separated from the previous one by whitespace that contains a newline. It
// reports false for anything else, including a stream that breaks part way:
// ClickHouse appends its exception text to a 200 answer once rows have been
// sent, and handing back the rows before it would hide that the result is
// incomplete.
//
// The decoder reads from the body already in memory and keeps about one row in
// its buffer at a time, so the extra memory is bounded by the largest row.
func decodeObjectLines(body []byte) ([]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	var rows []any
	for {
		offset := dec.InputOffset()
		var row any
		err := dec.Decode(&row)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, false
		}
		if _, isObject := row.(map[string]any); !isObject {
			return nil, false
		}
		if len(rows) > 0 && !startsWithNewline(body[offset:]) {
			return nil, false
		}
		rows = append(rows, row)
	}
	if len(rows) < 2 {
		return nil, false
	}
	return rows, true
}

// skipJSONSpace drops leading JSON whitespace (space, tab, CR, LF).
func skipJSONSpace(b []byte) []byte {
	for len(b) > 0 && isJSONSpace(b[0]) {
		b = b[1:]
	}
	return b
}

// startsWithNewline reports whether the leading whitespace of b contains a newline.
func startsWithNewline(b []byte) bool {
	for _, c := range b {
		if c == '\n' {
			return true
		}
		if !isJSONSpace(c) {
			return false
		}
	}
	return false
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}
