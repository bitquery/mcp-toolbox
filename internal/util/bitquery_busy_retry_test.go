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
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Code 202 refusals as the servers send them. Captured live on 2026-09-21 without
// adding load for the MCP's own database user: a per-query
// max_concurrent_queries_for_user / max_concurrent_queries_for_all_users of 1,
// under a user that already had queries running, is refused at registration.
// The production text differs only in the user name and the counts
// ("… for user mcp. Current: 4, maximum: 4.").
const (
	// 25.6, default_format=Native (what clickhouse-go asks for): plain text, HTTP 500.
	bqBusy25User = "Code: 202. DB::Exception: Too many simultaneous queries for user guest. Current: 13, maximum: 1. (TOO_MANY_SIMULTANEOUS_QUERIES) (version 25.6.4.12 (official build))"
	// The same through clickhouse-go when it is the handshake of a new connection
	// that is refused (the shape of the production failures).
	bqBusy25Handshake = "failed to query server hello: failed to query server hello info: sendQuery: [HTTP 500] response body: \"Code: 202. DB::Exception: Too many simultaneous queries for user mcp. Current: 4, maximum: 4. (TOO_MANY_SIMULTANEOUS_QUERIES) (version 25.6.4.12 (official build))\n\""
	// 25.6, JSONEachRow; the server-wide limit names no user.
	bqBusy25AllUsersJSON = "{\"exception\": \"Code: 202. DB::Exception: Too many simultaneous queries for all users. Current: 24, maximum: 1. (TOO_MANY_SIMULTANEOUS_QUERIES) (version 25.6.4.12 (official build))\"}\n"
	// 25.6, JSONEachRow with wait_end_of_query=1 and output_format_json_array_of_rows=1.
	bqBusy25AllUsersArray = "[\n{\"exception\": \"Code: 202. DB::Exception: Too many simultaneous queries for all users. Current: 23, maximum: 1. (TOO_MANY_SIMULTANEOUS_QUERIES) (version 25.6.4.12 (official build))\"}\n]\n"
	// 20.8: plain text for every format, HTTP 500.
	bqBusy20User = "Code: 202, e.displayText() = DB::Exception: Too many simultaneous queries for user default. Current: 3, maximum: 1 (version 20.8.11.17 (official build))\n"
)

func TestBitqueryIsBusyText(t *testing.T) {
	for _, in := range []string{bqBusy25User, bqBusy25Handshake, bqBusy25AllUsersJSON, bqBusy25AllUsersArray, bqBusy20User,
		// a server-wide limit on 20.8 (synthesized from the server's message format)
		"Code: 202, e.displayText() = DB::Exception: Too many simultaneous queries. Maximum: 100 (version 20.8.11.17 (official build))",
		// an already cleaned message
		"Code: 202. DB::Exception: Too many simultaneous queries for user [user]. Current: 4, maximum: 4. (TOO_MANY_SIMULTANEOUS_QUERIES)",
	} {
		if !BitqueryIsBusyText(in) {
			t.Errorf("BitqueryIsBusyText(%q) = false", in)
		}
	}
	for _, in := range []string{
		"",
		bqDriver60,
		bqOld396,
		// a guard whose own message mentions 202
		"Code: 395. DB::Exception: limit must be below 202: while executing 'FUNCTION throwIf(1 :: 0, 'limit must be below 202' :: 1) -> throwIf(1, 'limit must be below 202') UInt8 : 3'. (FUNCTION_THROW_IF_VALUE_IS_NON_ZERO) (version 25.6.4.12 (official build))",
		// not a ClickHouse exception
		"Code: 202 accepted",
		"<html><body><h1>503 Service Unavailable</h1>202</body></html>",
		"the data service did not answer within 180 seconds; the query may be too heavy, narrow it (shorter time window, fewer rows) or retry later",
		// a 202 quoted in an echo of the caller's SQL
		bqProxyEcho202,
		"sendQuery: [HTTP 400] response body: \"Syntax error: failed at position 8 ('x'): SELECT 'Code: 202. DB::Exception: Too many simultaneous queries for user bob. Current: 4, maximum: 4. (TOO_MANY_SIMULTANEOUS_QUERIES)' x\"",
	} {
		if BitqueryIsBusyText(in) {
			t.Errorf("BitqueryIsBusyText(%q) = true", in)
		}
	}
}

func TestBitqueryIsBusyError(t *testing.T) {
	ctx, _ := bqTestLogger(t)
	raw := fmt.Errorf("wrapped: %w", &bqDriverError{text: bqBusy25Handshake})
	cleaned := fmt.Errorf("unable to execute query: %w", BitqueryDatabaseFailure(ctx, "clickhouse-trading", raw))
	for _, err := range []error{raw, cleaned, BitqueryDatabaseErrorResponse(ctx, "ch-btc1-http", 500, []byte(bqBusy20User))} {
		if !BitqueryIsBusyError(err) {
			t.Errorf("BitqueryIsBusyError(%q) = false", err)
		}
	}
	for _, err := range []error{nil, errors.New(bqDriver60), BitqueryDatabaseFailure(ctx, "src", errors.New(bqDriver395)),
		BitquerySanitizeTransportError(&url.Error{Op: "Post", URL: bitqueryLeakyURL, Err: fakeTimeout{}}, 0)} {
		if BitqueryIsBusyError(err) {
			t.Errorf("BitqueryIsBusyError(%v) = true", err)
		}
	}
}

func TestBitqueryBusyResponse(t *testing.T) {
	header := func(code string) http.Header {
		h := http.Header{}
		if code != "" {
			h.Set("X-ClickHouse-Exception-Code", code)
		}
		return h
	}
	tcs := []struct {
		desc   string
		status int
		code   string
		body   string
		want   string
	}{
		{desc: "25.6 refusal with its header", status: 500, code: "202", body: bqBusy25AllUsersArray, want: BitqueryBusyDatabase},
		{desc: "the header alone decides", status: 500, code: "202", body: "", want: BitqueryBusyDatabase},
		{desc: "20.8 refusal, header dropped by a proxy", status: 500, body: bqBusy20User, want: BitqueryBusyDatabase},
		{desc: "another code in the header wins over the body", status: 500, code: "60", body: bqBusy20User},
		{desc: "another error", status: 404, code: "60", body: bqBody47},
		{desc: "a 200 answer is never a refusal", status: 200, code: "202", body: "{\"x\":1}\n" + bqBusy25AllUsersJSON},
		{desc: "a non-ClickHouse 503", status: 503, body: "<html>Service Unavailable</html>"},
		{desc: "a ClickHouse 202 body under another status than 500, no header", status: 502, body: bqBusy20User},
		{desc: "a proxy 502 echoing a caller's 202 literal", status: 502, body: bqProxyEcho202},
		{desc: "a 500 whose 202 is only in the query echo", status: 500, body: bqProxyEcho202},

		{desc: "gateway session limit", status: 429, body: bqGatewaySessions, want: BitqueryBusyProxy},
		{desc: "chproxy user concurrency limit", status: 429, body: bqChproxyConcurrency, want: BitqueryBusyProxy},
		{desc: "chproxy cluster user concurrency limit", status: 429, body: bqChproxyClusterLimit, want: BitqueryBusyProxy},
		{desc: "chproxy concurrency limit without its scope", status: 429, body: `limits for user "mcp" are exceeded: max_concurrent_queries limit: 4`, want: BitqueryBusyProxy},
		{desc: "chproxy concurrency limit with the query appended", status: 429, body: bqProxyScope + `limits for user "mcp" are exceeded: max_concurrent_queries limit: 4; query: "SELECT 1 FROM t WHERE s = \"a;b\"\n"`, want: BitqueryBusyProxy},
		{desc: "something else between the limit and the query", status: 429, body: bqProxyScope + `limits for user "mcp" are exceeded: max_concurrent_queries limit: 4 (queued for 3s); query: "SELECT 1"`},
		{desc: "gateway block lasts minutes", status: 429, body: bqGatewayBlocked},
		{desc: "chproxy rate limit lasts a minute", status: 429, body: bqChproxyRate},
		{desc: "chproxy killed a running query", status: 504, body: bqChproxyTimeout},
		{desc: "a proxy text under another status", status: 503, body: bqGatewaySessions},
		{desc: "a proxy text inside a longer body", status: 429, body: "error: " + bqChproxyConcurrency},
	}
	for _, tc := range tcs {
		if got := BitqueryBusyResponse(tc.status, header(tc.code), []byte(tc.body)); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.desc, got, tc.want)
		}
	}
}

// A proxy's own refusals. The gateway texts are the ones its HAProxy
// configuration returns; the chproxy texts follow chproxy's message formats.
const (
	bqProxyScope          = `[ Id: 17A2B3C4D5E6F709; User "mcp"(1) proxying as "mcp"(1) to "10.20.30.40:8123"(3); RemoteAddr: "10.20.30.41:51234"; LocalAddr: "10.20.30.42:8123"; Duration: 12 μs]: `
	bqGatewaySessions     = "Too Many Sessions\n"
	bqGatewayBlocked      = "429 Too Many Requests — temporarily blocked due to a high number of long-running queries. Please try again in a few minutes."
	bqChproxyConcurrency  = bqProxyScope + `limits for user "mcp" are exceeded: max_concurrent_queries limit: 4` + "\n"
	bqChproxyClusterLimit = bqProxyScope + `limits for cluster user "default" are exceeded: max_concurrent_queries limit: 100` + "\n"
	bqChproxyRate         = bqProxyScope + `rate limit for user "mcp" is exceeded: requests_per_minute limit: 100` + "\n"
	bqChproxyTimeout      = bqProxyScope + `timeout for user "mcp" exceeded: 2m0s` + "\n"
	// A proxy error whose query echo quotes a caller's literal refusal text.
	bqProxyEcho202 = bqProxyScope + `cannot reach the backend; query: "SELECT 'Code: 202. DB::Exception: Too many simultaneous queries for user bob. Current: 4, maximum: 4. (TOO_MANY_SIMULTANEOUS_QUERIES)'"` + "\n"
)

func TestBitqueryBusyRetryPause(t *testing.T) {
	restore := BitquerySetBusyPauses(time.Millisecond, 2*time.Millisecond)
	defer restore()
	ctx, logs := bqTestLogger(t)

	for retried := 0; retried < 2; retried++ {
		if !BitqueryBusyRetryPause(ctx, "clickhouse-trading", BitqueryBusyDatabase, retried) {
			t.Fatalf("retry %d refused", retried+1)
		}
	}
	if BitqueryBusyRetryPause(ctx, "clickhouse-trading", BitqueryBusyDatabase, 2) {
		t.Fatalf("a retry beyond the configured pauses was allowed")
	}
	out := logs.String()
	if n := strings.Count(out, "data source busy, retrying"); n != 2 {
		t.Fatalf("%d retry log lines, want 2: %q", n, out)
	}
	for _, keep := range []string{"WARN", "clickhouse-trading", BitqueryBusyDatabase} {
		if !strings.Contains(out, keep) {
			t.Fatalf("log %q does not contain %q", out, keep)
		}
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if BitqueryBusyRetryPause(cancelled, "src", BitqueryBusyDatabase, 0) {
		t.Fatalf("a retry was allowed on a cancelled context")
	}
}

func TestBitqueryBusyRetryPauseDeadline(t *testing.T) {
	restore := BitquerySetBusyPauses(time.Second)
	defer restore()
	ctx, logs := bqTestLogger(t)

	// Not enough time left for the pause and one more attempt: give up at once.
	short, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	if BitqueryBusyRetryPause(short, "src", BitqueryBusyDatabase, 0) {
		t.Fatalf("a retry was allowed past the deadline")
	}
	// A pause would have taken at least 750 ms.
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("gave up after %s, want at once", elapsed)
	}
	if logs.Len() != 0 {
		t.Fatalf("a refused retry was logged: %q", logs.String())
	}

	// Cancelled while waiting: stop waiting.
	restore2 := BitquerySetBusyPauses(10 * time.Second)
	defer restore2()
	waiting, cancelWaiting := context.WithCancel(ctx)
	time.AfterFunc(20*time.Millisecond, cancelWaiting)
	start = time.Now()
	if BitqueryBusyRetryPause(waiting, "src", BitqueryBusyProxy, 0) {
		t.Fatalf("a retry was allowed after the context ended")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("kept waiting %s after the context ended", elapsed)
	}
}

func TestBitqueryRetryBusy(t *testing.T) {
	restore := BitquerySetBusyPauses(time.Millisecond, time.Millisecond, time.Millisecond)
	defer restore()
	ctx, _ := bqTestLogger(t)
	busy := &bqDriverError{text: bqBusy25Handshake}

	// Refused twice, then answered.
	calls := 0
	got, err := BitqueryRetryBusy(ctx, "src", func() (int, error) {
		calls++
		if calls < 3 {
			return 0, busy
		}
		return 42, nil
	})
	if err != nil || got != 42 || calls != 3 {
		t.Fatalf("got %d, %v after %d calls; want 42, nil after 3", got, err, calls)
	}

	// Always refused: 1 attempt + 3 retries, then the refusal itself.
	calls = 0
	err = BitqueryRetryBusyCall(ctx, "src", func() error { calls++; return busy })
	if err != busy || calls != 4 {
		t.Fatalf("got %v after %d calls; want the refusal after 4", err, calls)
	}

	// Another error is returned at once.
	calls = 0
	other := errors.New(bqDriver60)
	err = BitqueryRetryBusyCall(ctx, "src", func() error { calls++; return other })
	if err != other || calls != 1 {
		t.Fatalf("got %v after %d calls; want the error after 1", err, calls)
	}

	// Out of time: the refusal, not a context error.
	restore2 := BitquerySetBusyPauses(time.Second)
	defer restore2()
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	calls = 0
	err = BitqueryRetryBusyCall(short, "src", func() error { calls++; return busy })
	if err != busy || calls != 1 {
		t.Fatalf("got %v after %d calls; want the refusal after 1", err, calls)
	}
}

func TestBitqueryJitter(t *testing.T) {
	for i := 0; i < 1000; i++ {
		if got := bitqueryJitter(time.Second); got < 750*time.Millisecond || got >= 1250*time.Millisecond {
			t.Fatalf("bitqueryJitter(1s) = %s, want within [750ms, 1250ms)", got)
		}
	}
}
