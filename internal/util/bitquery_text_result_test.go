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
package util

import (
	"errors"
	"strings"
	"testing"
)

const (
	bqQueryLogException = "Code: 60. DB::Exception: Unknown table expression identifier 't' in scope SELECT * FROM t. (UNKNOWN_TABLE) (version 25.9.4.58 (official build))"
	bqTaggedException   = "__exception__\r\nTAGtagTAGtag0123\r\nCode: 395. DB::Exception: boom: while executing 'FUNCTION throwIf(1 :: 0, 'boom' :: 1) -> throwIf(1, 'boom') UInt8 : 2'. (FUNCTION_THROW_IF_VALUE_IS_NON_ZERO) (version 25.9.4.58 (official build))\r\n178 TAGtagTAGtag0123\r\n__exception__\r\n"
)

// Text that does not end in an exception is data, server detail and exception
// text in it included, and nothing is logged.
func TestBitqueryTextResult(t *testing.T) {
	for _, tc := range []struct{ desc, in string }{
		{desc: "text with nothing in it to look at", in: "14999\t0\n14998\t\t1\n"},
		{desc: "whitespace alone", in: "  a  b \n\n"},
		{desc: "empty", in: ""},
		{desc: "addresses, host names and a version string are data", in: "peer\t10.20.30.40:9000\tchnode3.streaming-cluster.local\nversion\t26.8.7.19\n"},
		{desc: "a user name in a URL is data", in: "url\thttp://example.com/?user=mcp&x=1\t\n"},
		{desc: "exception text in a column before others (system.query_log)", in: "1\t" + bqQueryLogException + "\t2026-10-20 10:00:00\n2\t\t2026-10-20 10:00:01\n"},
		{desc: "exception text in the last column of the last row", in: "1\tok\n2\t" + bqQueryLogException + "\n"},
		{desc: "exception text on a line the body goes on after", in: bqQueryLogException + "\n" + "next\t1\n"},
		{desc: "exception text inside a JSON row", in: "{\"n\":1}\n{\"exception\":\"" + bqQueryLogException + "\"}\n{\"n\":2"},
		{desc: "a line that starts like an exception and does not end like one", in: "1\tok\nCode: 60. DB::Exception: cut short\n"},
		{desc: "an exception object that is not a ClickHouse message", in: "{\"n\":1}\n{\"exception\": \"hello\"}\n"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			ctx, logs := bqTestLogger(t)
			if err := BitqueryTextResult(ctx, "clickhouse-eth-http", tc.in); err != nil {
				t.Fatalf("got %v; want data", err)
			}
			if logs.Len() != 0 {
				t.Errorf("unexpected log: %q", logs.String())
			}
		})
	}
}

// A failed query logs at most bitqueryDatabaseLogLimit bytes of each text, and
// never the rows before the exception.
func TestBitqueryTextResultLogIsBounded(t *testing.T) {
	ctx, logs := bqTestLogger(t)
	row := "row\t10.20.30.40\t" + strings.Repeat("d", 100) + "\n"
	long := strings.Repeat("a", 17000)
	failed := strings.Repeat(row, 200) + "Code: 395, e.displayText() = DB::Exception: " + long + " (version 20.8.11.17 (official build))\n"
	if err := BitqueryTextResult(ctx, "clickhouse-eth-http", failed); err == nil {
		t.Fatalf("expected an error")
	}
	if n := logs.Len(); n > 2*bitqueryDatabaseLogLimit+1024 {
		t.Fatalf("logged %d bytes, want at most two capped texts", n)
	}
	if strings.Contains(logs.String(), "ddd") {
		t.Fatalf("the rows before the exception were logged")
	}
	if !strings.Contains(logs.String(), "bytes truncated") {
		t.Fatalf("the long message was not truncated in the log")
	}
}

func TestBitqueryTextResultException(t *testing.T) {
	tcs := []struct {
		desc, in, want string
		code           int
	}{
		{desc: "20.8 text after rows",
			in:   "{\"n\":1}\n{\"n\":2}\nCode: 395, e.displayText() = DB::Exception: Received from 172.16.221.1:9000. DB::Exception: boom (version 20.8.11.17 (official build))\n",
			want: "query failed: invalid request: boom", code: 395},
		{desc: "25.x object after rows",
			in:   "{\"n\":1}\n{\"exception\": \"Code: 396. DB::Exception: Limit for result exceeded, max rows: 10.00 thousand, current rows: 10.10 thousand. (TOO_MANY_ROWS_OR_BYTES) (version 25.9.4.58 (official build))\"}\n",
			want: "query failed: Code: 396. DB::Exception: Limit for result exceeded, max rows: 10.00 thousand, current rows: 10.10 thousand. (TOO_MANY_ROWS_OR_BYTES)", code: 396},
		{desc: "20.8 text after rows, one of them with an exception column",
			in:   "{\"n\":1}\n{\"exception\": \"hello\"}\nCode: 60, e.displayText() = DB::Exception: Table default.t doesn't exist. (version 20.8.11.17 (official build))",
			want: "query failed: Code: 60. DB::Exception: Table default.t doesn't exist.", code: 60},
		{desc: "25.x array form",
			in:   "[\n{\"n\":1},\n{\"exception\": \"Code: 241. DB::Exception: Query memory limit exceeded: would use 6.50 MiB, maximum: 1000.00 B: While executing AggregatingTransform. (MEMORY_LIMIT_EXCEEDED) (version 25.9.4.58 (official build))\"}\n]\n",
			want: "query failed: Code: 241. DB::Exception: Query memory limit exceeded: would use 6.50 MiB, maximum: 1000.00 B. (MEMORY_LIMIT_EXCEEDED)", code: 241},
		{desc: "TSV rows, then the exception as text; exception text in a column before it",
			in:   "1\t" + bqQueryLogException + "\n2\tok\n" + bqQueryLogException + "\n",
			want: "query failed: Code: 60. DB::Exception: Unknown table expression identifier 't' in scope SELECT * FROM t. (UNKNOWN_TABLE)", code: 60},
		{desc: "a message on several lines, ending in the code name",
			in:   "1\tok\nCode: 279. DB::NetException: All connection tries failed. Log: \n\nConnection refused. (ALL_CONNECTION_TRIES_FAILED)\n",
			want: "query failed: Code: 279. DB::NetException: All connection tries failed. Log: \n\nConnection refused. (ALL_CONNECTION_TRIES_FAILED)", code: 279},
		{desc: "TSV rows, then the tagged block of a current server",
			in:   "1\tok\n2\tok\n" + bqTaggedException,
			want: "query failed: invalid request: boom", code: 395},
	}
	for _, tc := range tcs {
		t.Run(tc.desc, func(t *testing.T) {
			ctx, logs := bqTestLogger(t)
			err := BitqueryTextResult(ctx, "clickhouse-eth-http", tc.in)
			if err == nil {
				t.Fatalf("got no error")
			}
			if err.Error() != tc.want {
				t.Fatalf("error:\n got %q\nwant %q", err.Error(), tc.want)
			}
			var dbErr *BitqueryDatabaseError
			if !errors.As(err, &dbErr) || dbErr.Code() != tc.code {
				t.Fatalf("not a *BitqueryDatabaseError with code %d: %#v", tc.code, err)
			}
			if strings.Contains(err.Error(), `{"n"`) || strings.Contains(err.Error(), "ok") {
				t.Errorf("rows reached the error text: %q", err.Error())
			}
			if !strings.Contains(logs.String(), "data source query error") {
				t.Errorf("server log %q does not hold the original", logs.String())
			}
		})
	}
}
