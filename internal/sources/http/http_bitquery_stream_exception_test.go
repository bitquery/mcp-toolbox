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

// Bitquery: a query that fails after ClickHouse has sent rows must come back as an
// error, not as the rows (or the body as one string); rows that only contain
// exception text stay rows.

import (
	"errors"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/googleapis/mcp-toolbox/internal/util"
)

// Bodies as live servers sent them (20.8.11.17 and 25.9.4.58, user mcp) for
// SELECT number, throwIf(number = N, 'boom') FROM system.numbers ... SETTINGS max_block_size=100,
// shortened to the last rows.
const (
	bqRows208 = `{"number":"14997","throwIf(equals(number, 15000), 'boom')":0}` + "\n" +
		`{"number":"14998","throwIf(equals(number, 15000), 'boom')":0}` + "\n" +
		`{"number":"14999","throwIf(equals(number, 15000), 'boom')":0}` + "\n"
	bqExc208      = "Code: 395, e.displayText() = DB::Exception: boom (version 20.8.11.17 (official build))"
	bqExc208Limit = "Code: 396, e.displayText() = DB::Exception: Limit for result exceeded, max rows: 25.00 thousand, current rows: 25.10 thousand (version 20.8.11.17 (official build))"

	bqRow25a      = `{"number":2998,"throwIf(equals(number, 3000), 'boom')":0}`
	bqRow25b      = `{"number":2999,"throwIf(equals(number, 3000), 'boom')":0}`
	bqExc25       = `Code: 395. DB::Exception: boom: while executing 'FUNCTION throwIf(equals(number, 3000) :: 3, 'boom' :: 2) -> throwIf(equals(number, 3000), 'boom') UInt8 : 1'. (FUNCTION_THROW_IF_VALUE_IS_NON_ZERO) (version 25.9.4.58 (official build))`
	bqExc25Object = `{"exception": "Code: 395. DB::Exception: boom: while executing 'FUNCTION throwIf(equals(number, 3000) :: 3, 'boom' :: 2) -> throwIf(equals(number, 3000), 'boom') UInt8 : 1'. (FUNCTION_THROW_IF_VALUE_IS_NON_ZERO) (version 25.9.4.58 (official build))"}`
)

func TestTrailingClickHouseException(t *testing.T) {
	netErr := "Code: 279. DB::NetException: All connection tries failed. Log: \n\nCode: 210. DB::NetException: Connection refused (chnode3-hdd:9001). (NETWORK_ERROR) (version 25.9.4.58 (official build))\n: While executing Remote. (ALL_CONNECTION_TRIES_FAILED) (version 25.9.4.58 (official build))"
	limit25 := "Code: 396. DB::Exception: Limit for result exceeded, max rows: 10.00 thousand, current rows: 10.10 thousand. (TOO_MANY_ROWS_OR_BYTES) (version 25.9.4.58 (official build))"

	tcs := []struct {
		name   string
		body   string
		format string
		found  bool
		rows   int
		text   string
	}{
		// Live shapes.
		{name: "20.8 text after rows", body: bqRows208 + bqExc208 + "\n", format: "JSONEachRow", found: true, rows: 3, text: bqExc208},
		{name: "20.8 result limit after rows", body: bqRows208 + bqExc208Limit + "\n", format: "JSONEachRow", found: true, rows: 3, text: bqExc208Limit},
		{name: "25.9 object after rows", body: bqRow25a + "\n" + bqRow25b + "\n" + bqExc25Object + "\n", format: "JSONEachRow", found: true, rows: 2, text: bqExc25},
		{name: "25.9 object inside the array form", body: "[\n" + bqRow25a + ",\n" + bqRow25b + ",\n" + bqExc25Object + "\n]\n", format: "JSONEachRow", found: true, rows: 2, text: bqExc25},

		// Other positions and message shapes (synthesized from the live ones).
		{name: "20.8 exception without rows", body: bqExc208 + "\n", format: "JSONEachRow", found: true, rows: 0, text: bqExc208},
		{name: "array form, exception right after the bracket", body: "[\n" + bqExc25Object + "\n]\n", format: "JSONEachRow", found: true, rows: 0, text: bqExc25},
		{name: "array form, text exception", body: "[\n" + bqRow25a + ",\n" + limit25 + "\n", format: "JSONEachRow", found: true, rows: 1, text: limit25},
		{name: "array form, text exception without rows", body: "[\n" + limit25 + "\n", format: "JSONEachRow", found: true, rows: 0, text: limit25},
		{name: "multi-line message keeps the outer exception", body: bqRow25a + "\n" + netErr + "\n", format: "JSONEachRow", found: true, rows: 1, text: netErr},
		{name: "exception block framing", body: bqRow25a + "\n" + bqRow25b + "\r\n__exception__\r\n" + limit25 + "\r\n187 0123456789abcdef\r\n__exception__\r\n", format: "JSONEachRow", found: true, rows: 2,
			text: limit25 + "\r\n187 0123456789abcdef\r\n__exception__"},
		{name: "CRLF lines", body: strings.ReplaceAll(bqRows208, "\n", "\r\n") + bqExc208 + "\r\n", format: "JSONEachRow", found: true, rows: 3, text: bqExc208},
		{name: "compact row format", body: "[14999,0]\n" + bqExc208 + "\n", format: "JSONCompactEachRow", found: true, rows: 1, text: bqExc208},
		{name: "strings row format", body: bqRows208 + bqExc208 + "\n", format: "JSONStringsEachRow", found: true, rows: 3, text: bqExc208},

		// Not an exception: the result is left as it is.
		{name: "complete stream", body: bqRows208, format: "JSONEachRow"},
		{name: "complete array form", body: "[\n" + bqRow25a + ",\n" + bqRow25b + "\n]\n", format: "JSONEachRow"},
		{name: "empty body", body: "", format: "JSONEachRow"},
		{name: "exception text in a column value", body: bqRow25a + "\n" + `{"message":"` + bqExc25 + `"}` + "\n", format: "JSONEachRow"},
		{name: "exception text in a column named exception", body: bqRow25a + "\n" + `{"exception":"` + bqExc25 + `"}` + "\n", format: "JSONEachRow"},
		{name: "exception text in a column named exception, array form", body: "[\n" + `{"exception":"` + bqExc25 + `"}` + "\n]\n", format: "JSONEachRow"},
		{name: "exception object that is not a ClickHouse message", body: bqRow25a + "\n" + `{"exception": "hello"}` + "\n", format: "JSONEachRow"},
		{name: "row with a column named exception among other columns", body: bqRow25a + "\n" + `{"exception":"` + bqExc25 + `","n":1}` + "\n", format: "JSONEachRow"},
		{name: "row with a column named exception among other columns, array form", body: "[\n" + bqRow25a + ",\n" + `{"n":1,"exception":"` + bqExc25 + `"}` + "\n]\n", format: "JSONEachRow"},
		{name: "exception object with another key", body: bqRow25a + "\n" + `{"exception": "` + bqExc208 + `", "n": 1}` + "\n", format: "JSONEachRow"},
		{name: "exception object before the last row", body: bqExc25Object + "\n" + bqRow25b + "\n", format: "JSONEachRow"},
		{name: "text exception before the last row", body: bqExc208 + "\n" + bqRow25b + "\n", format: "JSONEachRow"},
		{name: "truncated last row", body: bqRow25a + "\n" + `{"number":29`, format: "JSONEachRow"},
		{name: "exception glued to a row", body: bqRow25a + bqExc208 + "\n", format: "JSONEachRow"},
		{name: "text that is not a ClickHouse exception", body: bqRow25a + "\nCode: 395 boom\n", format: "JSONEachRow"},
		{name: "text format", body: "14999\t0\n" + bqExc208 + "\n", format: "TabSeparated"},
		{name: "no format header", body: bqRows208 + bqExc208 + "\n"},
		{name: "object format", body: "{\n\"row_1\": {\"n\":1}\n}\n" + bqExc208 + "\n", format: "JSONObjectEachRow"},
		{name: "exception longer ago than the tail", body: bqExc208 + "\n" + strings.Repeat("x", clickHouseExceptionTail) + "\n", format: "JSONEachRow"},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			got, found := trailingClickHouseException([]byte(tc.body), chHeader(tc.format))
			if found != tc.found {
				t.Fatalf("found = %v, want %v (got %+v)", found, tc.found, got)
			}
			if !found {
				return
			}
			if got.rows != tc.rows {
				t.Errorf("rows = %d, want %d", got.rows, tc.rows)
			}
			if got.text != tc.text {
				t.Errorf("text:\n got %q\nwant %q", got.text, tc.text)
			}
		})
	}
}

func bqStreamServer(t *testing.T, format, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		w.Header().Set("X-ClickHouse-Query-Id", "0f97fb5d-ee7b-4ddb-8ed3-86983bd8ec7d")
		if format != "" {
			w.Header().Set(clickHouseFormatHeader, format)
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestBitqueryRunRequestExceptionAfterRows(t *testing.T) {
	tcs := []struct {
		name, body, want string
		code             int
		logged           []string
	}{
		{
			name:   "20.8 throwIf",
			body:   strings.Repeat(bqRows208, 5000) + bqExc208 + "\n",
			want:   "query failed after 15000 result rows had been sent (partial result discarded): invalid request: boom",
			code:   395,
			logged: []string{"WARN", "data source query error", "clickhouse-bsc-http", "version 20.8.11.17", "after 15000 result rows"},
		},
		{
			name:   "20.8 result limit",
			body:   bqRows208 + bqExc208Limit + "\n",
			want:   "query failed after 3 result rows had been sent (partial result discarded): Code: 396. DB::Exception: Limit for result exceeded, max rows: 25.00 thousand, current rows: 25.10 thousand",
			code:   396,
			logged: []string{"WARN", "data source query error", "version 20.8.11.17"},
		},
		{
			name:   "25.9 array form",
			body:   "[\n" + bqRow25a + ",\n" + bqRow25b + ",\n" + bqExc25Object + "\n]\n",
			want:   "query failed after 2 result rows had been sent (partial result discarded): invalid request: boom",
			code:   395,
			logged: []string{"WARN", "data source query error", "while executing", "version 25.9.4.58"},
		},
		{
			name:   "exception before any row",
			body:   "[\n" + bqExc25Object + "\n]\n",
			want:   "query failed: invalid request: boom",
			code:   395,
			logged: []string{"WARN", "version 25.9.4.58"},
		},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			server := bqStreamServer(t, "JSONEachRow", tc.body)
			ctx, logs := bitqueryTestContext(t)
			source := bitqueryTestSource(t, ctx, server.URL, "5s")
			got, err := bitqueryRun(t, ctx, source, server.URL+bitqueryTestQuery)
			if err == nil {
				t.Fatalf("expected an error, got a %T result", got)
			}
			if err.Error() != tc.want {
				t.Fatalf("error:\n got %q\nwant %q", err.Error(), tc.want)
			}
			var dbErr *util.BitqueryDatabaseError
			if !errors.As(err, &dbErr) || dbErr.Code() != tc.code {
				t.Fatalf("not a *util.BitqueryDatabaseError with code %d: %#v", tc.code, err)
			}
			for _, leak := range []string{"version", "while executing", "e.displayText", "chnode"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("error %q contains %q", err.Error(), leak)
				}
			}
			out := logs.String()
			for _, keep := range tc.logged {
				if !strings.Contains(out, keep) {
					t.Errorf("server log %q does not contain %q", out, keep)
				}
			}
		})
	}
}

func TestBitqueryRunRequestRowsWithExceptionTextStayRows(t *testing.T) {
	ctx, logs := bitqueryTestContext(t)

	// A column holding exception text, as a query over a query log would return.
	body := bqRow25a + "\n" + `{"exception":"` + bqExc25 + `"}` + "\n"
	server := bqStreamServer(t, "JSONEachRow", body)
	got, err := bitqueryRun(t, ctx, bitqueryTestSource(t, ctx, server.URL, "5s"), server.URL+bitqueryTestQuery)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []any{
		map[string]any{"number": 2998.0, "throwIf(equals(number, 3000), 'boom')": 0.0},
		map[string]any{"exception": bqExc25},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("unexpected result (-want +got):\n%s", diff)
	}

	// A body cut without an exception keeps the old result: the raw string.
	cut := bqRow25a + "\n" + `{"number":29`
	server = bqStreamServer(t, "JSONEachRow", cut)
	got, err = bitqueryRun(t, ctx, bitqueryTestSource(t, ctx, server.URL, "5s"), server.URL+bitqueryTestQuery)
	if err != nil || got != cut {
		t.Fatalf("got %#v, %v; want the raw body", got, err)
	}

	// Without a ClickHouse format header nothing is checked.
	plain := bqRows208 + bqExc208 + "\n"
	server = bqStreamServer(t, "", plain)
	got, err = bitqueryRun(t, ctx, bitqueryTestSource(t, ctx, server.URL, "5s"), server.URL+bitqueryTestQuery)
	if err != nil || got != plain {
		t.Fatalf("got %#v, %v; want the raw body", got, err)
	}

	if strings.Contains(logs.String(), "data source query error") {
		t.Fatalf("unexpected query error log: %q", logs.String())
	}
}
