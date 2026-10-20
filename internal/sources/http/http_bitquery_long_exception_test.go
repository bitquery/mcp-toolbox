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

// Bitquery: an exception the server appends after streamed rows fails the call
// whatever its length, and the caller never sees the server version or a replica
// address. Up to 964ba806 the detector looked only at the last 16 KiB of a body, so
// a longer message (a throwIf whose message the caller wrote) came back as data:
// the 20.8 shape as one string holding the rows, the version and host:port, the
// 25.x shape as rows plus an "exception" row. When the detector cannot run (no
// X-ClickHouse-Format header), the result decoder and the text-result check
// still refuse to hand back such a body as rows or as raw text.

import (
	"encoding/json"
	"errors"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/googleapis/mcp-toolbox/internal/util"
)

// bqLongMessage is longer than the 16 KiB the detector used to look back over;
// bqShortMessage is the control that was always caught.
var (
	bqLongMessage  = strings.Repeat("a", 16484)
	bqShortMessage = strings.Repeat("a", 600)
)

const bqRows25x3 = `{"number":2997,"throwIf(greater(number, 2996), 'a')":0}` + "\n" +
	`{"number":2998,"throwIf(greater(number, 2996), 'a')":0}` + "\n" +
	`{"number":2999,"throwIf(greater(number, 2996), 'a')":0}` + "\n"

// bqRelayed208 is a throwIf relayed from a replica in the 20.8 text shape.
func bqRelayed208(message string) string {
	return "Code: 395, e.displayText() = DB::Exception: Received from 172.16.221.1:9000. DB::Exception: " + message + " (version 20.8.11.17 (official build))"
}

// bqRelayed25 is the same in the 25.x shape, expression context included.
func bqRelayed25(message string) string {
	return "Code: 395. DB::Exception: Received from 172.16.221.1:9000. DB::Exception: " + message +
		": while executing 'FUNCTION throwIf(greater(number, 2996) :: 1, '" + message + "' :: 2) -> throwIf(greater(number, 2996), '" + message +
		"') UInt8 : 3'. (FUNCTION_THROW_IF_VALUE_IS_NON_ZERO) (version 25.9.4.58 (official build))"
}

// bqExceptionObject is the object a current server writes in JSON formats: a
// space after the colon, which its row writer never puts there.
func bqExceptionObject(t *testing.T, text string) string {
	t.Helper()
	quoted, err := json.Marshal(text)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return `{"exception": ` + string(quoted) + `}`
}

// bqArrayForm wraps JSONEachRow lines the way output_format_json_array_of_rows=1 does.
func bqArrayForm(lines ...string) string {
	return "[\n" + strings.Join(lines, ",\n") + "\n]\n"
}

func bqAssertNoServerDetail(t *testing.T, text string) {
	t.Helper()
	for _, leak := range []string{"172.16.", ":9000", "version", "20.8.11", "25.9.4", "official build", "Received from", "while executing", "e.displayText", `"exception"`, "2997", "14999"} {
		if strings.Contains(text, leak) {
			t.Errorf("caller-facing text contains %q (%d bytes, starts %.120q)", leak, len(text), text)
		}
	}
}

func TestBitqueryRunRequestLongExceptionAfterRows(t *testing.T) {
	rows25 := strings.Split(strings.TrimSuffix(bqRows25x3, "\n"), "\n")
	tcs := []struct {
		name, format, body, want string
	}{
		{
			name:   "20.8 text, message longer than 16 KiB",
			format: "JSONEachRow",
			body:   bqRows208 + bqRelayed208(bqLongMessage) + "\n",
			want:   "query failed after 3 result rows had been sent (partial result discarded): invalid request: " + bqLongMessage,
		},
		{
			name:   "20.8 text, 600 bytes (control)",
			format: "JSONEachRow",
			body:   bqRows208 + bqRelayed208(bqShortMessage) + "\n",
			want:   "query failed after 3 result rows had been sent (partial result discarded): invalid request: " + bqShortMessage,
		},
		{
			name:   "25.x object line, message longer than 16 KiB",
			format: "JSONEachRow",
			body:   bqRows25x3 + bqExceptionObject(t, bqRelayed25(bqLongMessage)) + "\n",
			want:   "query failed after 3 result rows had been sent (partial result discarded): invalid request: " + bqLongMessage,
		},
		{
			name:   "25.x object line in the array form, message longer than 16 KiB",
			format: "JSONEachRow",
			body:   bqArrayForm(append(rows25, bqExceptionObject(t, bqRelayed25(bqLongMessage)))...),
			want:   "query failed after 3 result rows had been sent (partial result discarded): invalid request: " + bqLongMessage,
		},
		{
			name:   "25.x object line, 600 bytes (control)",
			format: "JSONEachRow",
			body:   bqRows25x3 + bqExceptionObject(t, bqRelayed25(bqShortMessage)) + "\n",
			want:   "query failed after 3 result rows had been sent (partial result discarded): invalid request: " + bqShortMessage,
		},

		// Without the format header the detector does not run: the decoder refuses
		// the rows and the text-result cleaning reports the failure.
		{
			name: "no format header, 20.8 text after rows",
			body: bqRows208 + bqRelayed208(bqLongMessage) + "\n",
			want: "query failed: invalid request: " + bqLongMessage,
		},
		{
			name: "no format header, 25.x object after rows",
			body: bqRows25x3 + bqExceptionObject(t, bqRelayed25(bqLongMessage)) + "\n",
			want: "query failed: invalid request: " + bqLongMessage,
		},
		{
			name: "no format header, 25.x object in the array form",
			body: bqArrayForm(append(rows25, bqExceptionObject(t, bqRelayed25(bqShortMessage)))...),
			want: "query failed: invalid request: " + bqShortMessage,
		},
		{
			name: "no format header, 25.x object alone",
			body: bqExceptionObject(t, bqRelayed25(bqShortMessage)) + "\n",
			want: "query failed: invalid request: " + bqShortMessage,
		},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			server := bqStreamServer(t, tc.format, tc.body)
			ctx, logs := bitqueryTestContext(t)
			source := bitqueryTestSource(t, ctx, server.URL, "5s")
			got, err := bitqueryRun(t, ctx, source, server.URL+bitqueryTestQuery)
			if err == nil {
				t.Fatalf("expected an error, got a %T result of %d bytes / rows", got, bqResultSize(got))
			}
			if got != nil {
				t.Errorf("a partial result came back with the error: %T", got)
			}
			if err.Error() != tc.want {
				t.Fatalf("error (%d bytes):\n got %.200q\nwant %.200q", len(err.Error()), err.Error(), tc.want)
			}
			var dbErr *util.BitqueryDatabaseError
			if !errors.As(err, &dbErr) || dbErr.Code() != 395 {
				t.Fatalf("not a *util.BitqueryDatabaseError with code 395: %T", err)
			}
			bqAssertNoServerDetail(t, err.Error())
			out := logs.String()
			for _, keep := range []string{"WARN", "data source query error", "172.16.221.1:9000"} {
				if !strings.Contains(out, keep) {
					t.Errorf("server log does not contain %q", keep)
				}
			}
		})
	}
}

func bqResultSize(v any) int {
	switch r := v.(type) {
	case string:
		return len(r)
	case []any:
		return len(r)
	}
	return 0
}

// A text body that does not end in an exception is returned as the result, byte
// for byte. Only answers of a ClickHouse server are checked.
func TestBitqueryRunRequestTextResult(t *testing.T) {
	ctx, _ := bitqueryTestContext(t)

	// A text output format (a raw statement may end in FORMAT TSV): byte for byte.
	tsv := "14999\t0\n14998\t\t1\n"
	server := bqStreamServer(t, "TabSeparated", tsv)
	got, err := bitqueryRun(t, ctx, bitqueryTestSource(t, ctx, server.URL, "5s"), server.URL+bitqueryTestQuery)
	if err != nil || got != tsv {
		t.Fatalf("got %#v, %v; want the body unchanged", got, err)
	}

	// The rows, then the exception the server wrote as text: a failed query.
	server = bqStreamServer(t, "TabSeparated", tsv+bqRelayed25("boom")+"\n")
	got, err = bitqueryRun(t, ctx, bitqueryTestSource(t, ctx, server.URL, "5s"), server.URL+bitqueryTestQuery)
	if want := "query failed: invalid request: boom"; err == nil || err.Error() != want || got != nil {
		t.Fatalf("got %#v, %v; want the error %q", got, err, want)
	}

	// Not a ClickHouse answer (no X-ClickHouse-* header, no ClickHouse parameter):
	// the text is returned as it came, as upstream does.
	plain := bqRelayed208("boom") + "\n"
	other := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		_, _ = w.Write([]byte(plain))
	}))
	t.Cleanup(other.Close)
	got, err = bitqueryRun(t, ctx, bitqueryTestSource(t, ctx, other.URL, "5s"), other.URL+"/?q=1")
	if err != nil || got != plain {
		t.Fatalf("got %#v, %v; want the body unchanged", got, err)
	}
}

// A data body comes back byte for byte: addresses, a version string, empty TSV
// fields, spaces inside a field, and exception text in a column (a raw statement
// over system.query_log) are all data.
func TestBitqueryRunRequestTextResultIsData(t *testing.T) {
	body := "host\tpeer\tnote\tn\n" +
		"a\t172.16.221.1:9000\t\t1\n" +
		"b\t\tvalue with  two spaces  \t2\n" +
		"c\t10.20.30.40\t26.8.7.19\t\n" +
		"d\t" + bqRelayed25("boom") + "\t\t3\n" +
		"\t\t\t\n"

	for _, format := range []string{"TabSeparated", "TSV", ""} {
		t.Run("format "+format, func(t *testing.T) {
			ctx, logs := bitqueryTestContext(t)
			server := bqStreamServer(t, format, body)
			got, err := bitqueryRun(t, ctx, bitqueryTestSource(t, ctx, server.URL, "5s"), server.URL+bitqueryTestQuery)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if text, ok := got.(string); !ok || text != body {
				t.Fatalf("got %#v\nwant %q", got, body)
			}
			if strings.Contains(logs.String(), "data source") {
				t.Errorf("unexpected log: %q", logs.String())
			}
		})
	}
}
