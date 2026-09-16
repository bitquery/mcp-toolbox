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

// Bitquery: ClickHouse answers 200 and streams rows while a query still runs.
// When the query fails after that, the status can no longer change: the server
// writes the exception after the rows it has sent and ends the response
// normally. Two shapes were seen on live servers:
//
//   - as text on a line of its own (20.8; any server writing the exception as text):
//     {"number":"14999"}
//     Code: 395, e.displayText() = DB::Exception: boom (version 20.8.11.17 (official build))
//   - as a JSON object in JSON formats (current servers, http_write_exception_in_output_format),
//     inside the array when output_format_json_array_of_rows=1:
//     {"number":2999},
//     {"exception": "Code: 395. DB::Exception: boom: while executing 'FUNCTION throwIf(…)'. (FUNCTION_THROW_IF_VALUE_IS_NON_ZERO) (version 25.9.4.58 (official build))"}
//     ]
//
// Unchecked, the first reached the caller as the whole body in one string ending
// in the server's message, and the second as rows plus one "exception" row: a
// partial result that reads as a complete one. RunRequest reports both as a
// query error instead.
//
// Only JSON line formats are checked. Every row there is one line starting with
// { or [, so the exception is a line no row can be: a text line starting with
// "Code: N", or an object written as {"exception": "…"} — ClickHouse's row writer
// never puts a space after the colon. A row that only holds exception text in a
// column, even in a column named exception, stays a row.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
)

// clickHouseExceptionTail is how far back from the end of a body the exception
// is looked for; the messages are much shorter.
const clickHouseExceptionTail = 16 << 10

// clickHouseJSONExceptionPrefix starts the exception object ClickHouse writes in
// JSON output formats.
const clickHouseJSONExceptionPrefix = `{"exception": "`

// clickHouseExceptionStartRe matches the start of a ClickHouse exception message:
// "Code: 395. DB::Exception: …", or "Code: 395, e.displayText() = DB::Exception: …"
// on 20.8.
var clickHouseExceptionStartRe = regexp.MustCompile(`^Code: \d+(?:\.|, e\.(?:displayText|what)\(\) =) DB::\w*Exception\b`)

// streamException is a ClickHouse exception found at the end of a 200 body.
type streamException struct {
	text string // the exception message as the server wrote it
	rows int    // result rows sent before it
}

// isJSONLineFormat reports whether a ClickHouse output format writes each row as
// one line holding a JSON object or array.
func isJSONLineFormat(format string) bool {
	f := strings.ToLower(strings.TrimSpace(format))
	switch f {
	case "jsonlines", "ndjson":
		return true
	case "jsonobjecteachrow":
		return false
	}
	return strings.HasPrefix(f, "json") && strings.Contains(f, "eachrow")
}

// trailingClickHouseException reports the exception a ClickHouse server appended
// to a successful response body, if there is one. It looks at the lines of the
// last clickHouseExceptionTail bytes, from the end back to the last row.
func trailingClickHouseException(body []byte, header http.Header) (streamException, bool) {
	if !isJSONLineFormat(header.Get(clickHouseFormatHeader)) {
		return streamException{}, false
	}
	body = bytes.TrimRight(body, " \t\r\n")
	array := isArrayOfRows(body)
	floor := max(0, len(body)-clickHouseExceptionTail)

	start := -1
	found := streamException{rows: -1}
	last := true // only blank lines and the closing bracket follow this line
scan:
	for end := len(body); end > floor; {
		lineStart := bytes.LastIndexByte(body[floor:end], '\n') + 1
		if lineStart == 0 && floor > 0 {
			break // the line starts before the tail
		}
		lineStart += floor
		line := bytes.TrimSpace(body[lineStart:end])
		end = lineStart - 1

		switch {
		case len(line) == 0:
		case last && array && len(line) == 1 && line[0] == ']':
		case line[0] == '{' || line[0] == '[' || line[0] == ']':
			// A row (or the opening bracket): whatever exception there is follows
			// it. Only the last line can be the exception object itself.
			if !last {
				found.rows = lineCount(body[:lineStart]) + 1
			} else if message, ok := clickHouseJSONException(line); ok {
				start, found.text, found.rows = lineStart, message, lineCount(body[:lineStart])
			}
			break scan
		case clickHouseExceptionStartRe.Match(line):
			// Keep going: a message can carry the exceptions it relays on lines of
			// their own, and the outermost one comes first.
			start, found.text = lineStart, string(body[lineStart:])
			last = false
		default:
			// A later line of a multi-line message.
			last = false
		}
	}
	if start < 0 {
		return streamException{}, false
	}
	if found.rows < 0 {
		found.rows = lineCount(body[:start]) // no row inside the tail
	}
	if array && found.rows > 0 {
		found.rows-- // the opening bracket
	}
	return found, true
}

// isArrayOfRows reports whether the body is in the array form
// (output_format_json_array_of_rows=1): a first line holding only "[".
func isArrayOfRows(body []byte) bool {
	first := skipJSONSpace(body)
	if i := bytes.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	return string(bytes.TrimSpace(first)) == "["
}

func lineCount(b []byte) int {
	return bytes.Count(b, []byte{'\n'})
}

// clickHouseJSONException returns the message of the {"exception": "…"} object
// ClickHouse writes in JSON output formats.
func clickHouseJSONException(line []byte) (string, bool) {
	if !bytes.HasPrefix(line, []byte(clickHouseJSONExceptionPrefix)) {
		return "", false
	}
	var obj map[string]any
	if err := json.Unmarshal(line, &obj); err != nil || len(obj) != 1 {
		return "", false
	}
	message, ok := obj["exception"].(string)
	if !ok || !clickHouseExceptionStartRe.MatchString(message) {
		return "", false
	}
	return message, true
}
