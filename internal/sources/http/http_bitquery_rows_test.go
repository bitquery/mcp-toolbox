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

// Bitquery: a ClickHouse JSONEachRow row stream must reach the caller in the same
// shape as the array form, while every other body keeps its old result.

import (
	"encoding/json"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func chHeader(format string) nethttp.Header {
	h := nethttp.Header{}
	if format != "" {
		h.Set(clickHouseFormatHeader, format)
	}
	return h
}

func row(k string, v any) map[string]any { return map[string]any{k: v} }

const chException = "Code: 395, e.displayText() = DB::Exception: Value passed to 'throwIf' function is non zero (version 20.8.11.17 (official build))\n"

func TestDecodeResponseBody(t *testing.T) {
	tcs := []struct {
		name   string
		body   string
		format string
		want   any
	}{
		// ClickHouse row stream (legacy servers, no array wrapping).
		{name: "row stream, 0 rows", body: "", format: "JSONEachRow", want: []any{}},
		{name: "row stream, whitespace only", body: "\n", format: "JSONEachRow", want: []any{}},
		{name: "row stream, 1 row", body: "{\"number\":\"0\"}\n", format: "JSONEachRow", want: []any{row("number", "0")}},
		{name: "row stream, 1 row without newline", body: `{"number":"0"}`, format: "JSONEachRow", want: []any{row("number", "0")}},
		{name: "row stream, 3 rows", body: "{\"number\":\"0\"}\n{\"number\":\"1\"}\n{\"number\":\"2\"}\n", format: "JSONEachRow", want: []any{row("number", "0"), row("number", "1"), row("number", "2")}},
		{name: "row stream, 3 rows without final newline", body: "{\"n\":1}\n{\"n\":2}\n{\"n\":3}", format: "JSONEachRow", want: []any{row("n", 1.0), row("n", 2.0), row("n", 3.0)}},
		{name: "row stream, CRLF and blank lines", body: "{\"n\":1}\r\n\r\n{\"n\":2}\r\n", format: "JSONEachRow", want: []any{row("n", 1.0), row("n", 2.0)}},
		{name: "row stream, nested values", body: "{\"a\":[1,{\"b\":null}],\"s\":\"x\\ny\"}\n{\"a\":[],\"s\":\"\"}\n", format: "JSONEachRow", want: []any{map[string]any{"a": []any{1.0, map[string]any{"b": nil}}, "s": "x\ny"}, map[string]any{"a": []any{}, "s": ""}}},
		{name: "row stream, header case and alias", body: "{\"n\":1}\n", format: " jsonlines ", want: []any{row("n", 1.0)}},
		{name: "row stream, strings format", body: "{\"n\":\"1\"}\n{\"n\":\"2\"}\n", format: "JSONStringsEachRow", want: []any{row("n", "1"), row("n", "2")}},

		// ClickHouse array form (servers with output_format_json_array_of_rows): unchanged.
		{name: "array form, 0 rows", body: "[\n\n]\n", format: "JSONEachRow", want: []any{}},
		{name: "array form, 1 row", body: "[\n{\"number\":0}\n]\n", format: "JSONEachRow", want: []any{row("number", 0.0)}},
		{name: "array form, 3 rows", body: "[\n{\"n\":0},\n{\"n\":1},\n{\"n\":2}\n]\n", format: "JSONEachRow", want: []any{row("n", 0.0), row("n", 1.0), row("n", 2.0)}},

		// Newline-separated objects without the ClickHouse header: rows too.
		{name: "ndjson without header", body: "{\"id\":1}\n{\"id\":2}\n", want: []any{row("id", 1.0), row("id", 2.0)}},

		// One JSON value without the ClickHouse header: exactly as before.
		{name: "single object", body: `{"status":"ok"}`, want: row("status", "ok")},
		{name: "single object, trailing newline", body: "{\"status\":\"ok\"}\n", want: row("status", "ok")},
		{name: "single array", body: `[{"id":1,"name":"Alice"},{"id":3,"name":"Sid"}]`, want: []any{map[string]any{"id": 1.0, "name": "Alice"}, map[string]any{"id": 3.0, "name": "Sid"}}},
		{name: "empty array", body: "[]", want: []any{}},
		{name: "json string", body: "\"hello world\"\n", want: "hello world"},
		{name: "json number", body: "42", want: 42.0},
		{name: "json null", body: "null", want: nil},
		{name: "single object from a non row format", body: `{"meta":[],"data":[],"rows":0}`, format: "JSON", want: map[string]any{"meta": []any{}, "data": []any{}, "rows": 0.0}},

		// Not JSON, or not a clean object stream: the raw string, as before.
		{name: "empty body without header", body: "", want: ""},
		{name: "whitespace body without header", body: "\n", want: "\n"},
		{name: "empty body from a non row format", body: "", format: "TabSeparated", want: ""},
		{name: "plain text", body: "hello world", want: "hello world"},
		{name: "tab separated numbers", body: "0\n1\n2\n", format: "TabSeparated", want: "0\n1\n2\n"},
		{name: "html", body: "<html><body>502 Bad Gateway</body></html>", want: "<html><body>502 Bad Gateway</body></html>"},
		{name: "broken line in the middle", body: "{\"n\":1}\n{\"n\":\n{\"n\":3}\n", format: "JSONEachRow", want: "{\"n\":1}\n{\"n\":\n{\"n\":3}\n"},
		{name: "truncated last row", body: "{\"n\":1}\n{\"n\":2", format: "JSONEachRow", want: "{\"n\":1}\n{\"n\":2"},
		{name: "exception after rows", body: "{\"n\":1}\n{\"n\":2}\n" + chException, format: "JSONEachRow", want: "{\"n\":1}\n{\"n\":2}\n" + chException},
		{name: "exception after one row", body: "{\"n\":1}\n" + chException, format: "JSONEachRow", want: "{\"n\":1}\n" + chException},
		{name: "objects on one line", body: `{"n":1}{"n":2}`, format: "JSONEachRow", want: `{"n":1}{"n":2}`},
		{name: "objects separated by a space", body: `{"n":1} {"n":2}`, want: `{"n":1} {"n":2}`},
		{name: "arrays per line", body: "[0,\"a\"]\n[1,\"b\"]\n", format: "JSONCompactEachRow", want: "[0,\"a\"]\n[1,\"b\"]\n"},
		{name: "scalars per line", body: "1\n2\n", want: "1\n2\n"},
		{name: "object then array", body: "{\"n\":1}\n[2]\n", want: "{\"n\":1}\n[2]\n"},
		{name: "object then garbage", body: `{"n":1}}`, format: "JSONEachRow", want: `{"n":1}}`},
		{name: "number out of range", body: "{\"n\":1}\n{\"n\":1e400}\n", want: "{\"n\":1}\n{\"n\":1e400}\n"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeResponseBody([]byte(tc.body), chHeader(tc.format))
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("unexpected result (-want +got):\n%s", diff)
			}
		})
	}
}

// mcpTextBlocks mirrors how the MCP tools/call handler turns a tool result into
// content blocks: a []any is one block per element, anything else one block.
func mcpTextBlocks(t *testing.T, result any) []string {
	t.Helper()
	items, ok := result.([]any)
	if !ok {
		items = []any{result}
	}
	blocks := make([]string, 0, len(items))
	for _, item := range items {
		b, err := json.Marshal(item)
		if err != nil {
			t.Fatalf("marshal %v: %v", item, err)
		}
		blocks = append(blocks, string(b))
	}
	return blocks
}

// TestBitqueryRowStreamMatchesArrayForm runs the same rows through RunRequest in
// both ClickHouse answer forms and expects byte-identical results.
func TestBitqueryRowStreamMatchesArrayForm(t *testing.T) {
	tcs := []struct {
		name   string
		stream string // server without output_format_json_array_of_rows
		array  string // server with it
		blocks []string
	}{
		{name: "0 rows", stream: "", array: "[\n\n]\n", blocks: []string{}},
		{name: "1 row", stream: "{\"s\":\"a\"}\n", array: "[\n{\"s\":\"a\"}\n]\n", blocks: []string{`{"s":"a"}`}},
		{name: "3 rows", stream: "{\"s\":\"a\"}\n{\"s\":\"b\"}\n{\"s\":\"c\"}\n", array: "[\n{\"s\":\"a\"},\n{\"s\":\"b\"},\n{\"s\":\"c\"}\n]\n", blocks: []string{`{"s":"a"}`, `{"s":"b"}`, `{"s":"c"}`}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			run := func(body string) any {
				server := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
					w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
					w.Header().Set(clickHouseFormatHeader, "JSONEachRow")
					_, _ = w.Write([]byte(body))
				}))
				defer server.Close()
				ctx, _ := bitqueryTestContext(t)
				source := bitqueryTestSource(t, ctx, server.URL, "5s")
				got, err := bitqueryRun(t, ctx, source, server.URL+"/?default_format=JSONEachRow&user=mcp")
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return got
			}
			stream, array := run(tc.stream), run(tc.array)

			streamJSON, _ := json.Marshal(stream)
			arrayJSON, _ := json.Marshal(array)
			if string(streamJSON) != string(arrayJSON) {
				t.Fatalf("row stream result %s, array form result %s", streamJSON, arrayJSON)
			}
			if diff := cmp.Diff(tc.blocks, mcpTextBlocks(t, stream)); diff != "" {
				t.Fatalf("row stream content blocks (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.blocks, mcpTextBlocks(t, array)); diff != "" {
				t.Fatalf("array form content blocks (-want +got):\n%s", diff)
			}
		})
	}
}

// TestBitqueryRowStreamErrorBodyStaysAnError: a failed query is still an error,
// not rows; its text loses the server version like any other error body.
func TestBitqueryRowStreamErrorBodyStaysAnError(t *testing.T) {
	server := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		w.Header().Set(clickHouseFormatHeader, "JSONEachRow")
		w.WriteHeader(nethttp.StatusInternalServerError)
		_, _ = w.Write([]byte(chException))
	}))
	defer server.Close()

	ctx, _ := bitqueryTestContext(t)
	source := bitqueryTestSource(t, ctx, server.URL, "5s")
	_, err := bitqueryRun(t, ctx, source, server.URL+"/?default_format=JSONEachRow&user=mcp")
	want := "unexpected status code: 500, response body: invalid request: Value passed to 'throwIf' function is non zero"
	if err == nil || err.Error() != want {
		t.Fatalf("got error %v, want %q", err, want)
	}
}

func BenchmarkDecodeResponseBody(b *testing.B) {
	line := `{"Sender":"bc1qxy2kgdygjrsqtzq2n0yrf2493p83kkfjhx0wlh","Receiver":"1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2","Amount":0.01234567,"Tx":"f4184fc596403b9d638783cf57adfe4c75c605f6356fbc91338530e9831e9e16","Time":"2026-09-16 10:00:00"}`
	rows := strings.Repeat(line+"\n", 25000)
	array := "[\n" + strings.Repeat(line+",\n", 24999) + line + "\n]\n"
	header := chHeader("JSONEachRow")
	b.Run("row stream 25k", func(b *testing.B) {
		body := []byte(rows)
		b.SetBytes(int64(len(body)))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = decodeResponseBody(body, header)
		}
	})
	b.Run("array form 25k", func(b *testing.B) {
		body := []byte(array)
		b.SetBytes(int64(len(body)))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = decodeResponseBody(body, header)
		}
	})
}
