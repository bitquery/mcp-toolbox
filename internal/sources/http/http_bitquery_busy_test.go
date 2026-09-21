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

// Bitquery: a request ClickHouse refuses for too many simultaneous queries (Code
// 202, HTTP 500 before any row) is sent again after a short pause, with the same
// URL and body; an answer that carried rows is never sent again.

import (
	"context"
	"encoding/json"
	"io"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/googleapis/mcp-toolbox/internal/util"
)

// Refusals as the servers send them (captured live, user name and counts as in production).
const (
	// 25.x with wait_end_of_query=1 and output_format_json_array_of_rows=1.
	bqBusyArray = "[\n{\"exception\": \"Code: 202. DB::Exception: Too many simultaneous queries for user mcp. Current: 4, maximum: 4. (TOO_MANY_SIMULTANEOUS_QUERIES) (version 25.9.4.58 (official build))\"}\n]\n"
	// 20.8, plain text for every format.
	bqBusyOld = "Code: 202, e.displayText() = DB::Exception: Too many simultaneous queries for user mcp. Current: 4, maximum: 4 (version 20.8.11.17 (official build))\n"
)

type bqAnswer struct {
	status int
	code   string // X-ClickHouse-Exception-Code, "" = not sent
	format string // X-ClickHouse-Format, "" = not sent
	body   string
}

type bqRequest struct {
	query string // URL query string
	body  string
}

type bqServer struct {
	*httptest.Server
	mu   sync.Mutex
	seen []bqRequest
}

func (s *bqServer) requests() []bqRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bqRequest(nil), s.seen...)
}

func bqBusyTestServer(t *testing.T, answer func(n int) bqAnswer) *bqServer {
	t.Helper()
	s := &bqServer{}
	s.Server = httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		n := len(s.seen)
		s.seen = append(s.seen, bqRequest{query: r.URL.RawQuery, body: string(body)})
		s.mu.Unlock()
		a := answer(n)
		if a.code != "" {
			w.Header().Set("X-ClickHouse-Exception-Code", a.code)
		}
		if a.format != "" {
			w.Header().Set(clickHouseFormatHeader, a.format)
		}
		w.WriteHeader(a.status)
		_, _ = w.Write([]byte(a.body))
	}))
	t.Cleanup(s.Close)
	return s
}

func bqFastPauses(t *testing.T) {
	t.Helper()
	t.Cleanup(util.BitquerySetBusyPauses(time.Millisecond, time.Millisecond, time.Millisecond))
}

func bqRetryLines(logs string) []string {
	var lines []string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "data source busy, retrying") {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestBitqueryRunRequestRetriesBusy(t *testing.T) {
	bqFastPauses(t)
	server := bqBusyTestServer(t, func(n int) bqAnswer {
		if n < 2 {
			return bqAnswer{status: 500, code: "202", format: "JSONEachRow", body: bqBusyArray}
		}
		return bqAnswer{status: 200, format: "JSONEachRow", body: "[\n{\"x\":1},\n{\"x\":2}\n]\n"}
	})

	ctx, logs := bitqueryTestContext(t)
	source := bitqueryTestSource(t, ctx, server.URL, "5s")
	got, err := bitqueryRun(t, ctx, source, server.URL+bitqueryTestQuery+"&query_id=q-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotJSON, _ := json.Marshal(got); string(gotJSON) != `[{"x":1},{"x":2}]` {
		t.Fatalf("got %s", gotJSON)
	}

	requests := server.requests()
	if len(requests) != 3 {
		t.Fatalf("server saw %d requests, want 3", len(requests))
	}
	for _, r := range requests {
		// The same statement and URL, query_id included, every time.
		if r.body != "SELECT 1" || r.query != requests[0].query || !strings.Contains(r.query, "query_id=q-1") {
			t.Fatalf("request %+v differs from the first %+v", r, requests[0])
		}
	}
	lines := bqRetryLines(logs.String())
	if len(lines) != 2 {
		t.Fatalf("%d retry log lines, want 2: %q", len(lines), logs.String())
	}
	for _, line := range lines {
		if !strings.Contains(line, "WARN") || !strings.Contains(line, "clickhouse-bsc-http") || strings.Contains(line, "mcp") {
			t.Fatalf("retry log line %q: want WARN and the source, and no user name", line)
		}
	}
}

func TestBitqueryRunRequestBusyGivesUp(t *testing.T) {
	bqFastPauses(t)
	// 20.8, and no header: the body alone identifies the refusal.
	server := bqBusyTestServer(t, func(int) bqAnswer { return bqAnswer{status: 500, body: bqBusyOld} })

	ctx, logs := bitqueryTestContext(t)
	source := bitqueryTestSource(t, ctx, server.URL, "5s")
	_, err := bitqueryRun(t, ctx, source, server.URL+bitqueryTestQuery)
	want := "unexpected status code: 500, response body: Code: 202. DB::Exception: Too many simultaneous queries for user [user]. Current: 4, maximum: 4"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
	if !util.BitqueryIsBusyError(err) {
		t.Fatalf("the final error is not recognizable as a refusal: %#v", err)
	}
	if n := len(server.requests()); n != 4 {
		t.Fatalf("server saw %d requests, want 4", n)
	}
	if n := len(bqRetryLines(logs.String())); n != 3 {
		t.Fatalf("%d retry log lines, want 3", n)
	}

	// Without returnFullError the caller gets the status only, after the same retries.
	server2 := bqBusyTestServer(t, func(int) bqAnswer { return bqAnswer{status: 500, code: "202", body: bqBusyArray} })
	cfg := Config{Name: "clickhouse-bsc-http", Type: SourceType, BaseURL: server2.URL, Timeout: "5s", AllowPrivateNetworks: true}
	initialized, ierr := cfg.Initialize(ctx, nil)
	if ierr != nil {
		t.Fatalf("failed to initialize source: %v", ierr)
	}
	_, err = bitqueryRun(t, ctx, initialized.(*Source), server2.URL+bitqueryTestQuery)
	if want := "unexpected status code: 500 (Internal Server Error)"; err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
	if n := len(server2.requests()); n != 4 {
		t.Fatalf("server saw %d requests, want 4", n)
	}
}

func TestBitqueryRunRequestDoesNotRetry(t *testing.T) {
	bqFastPauses(t)
	tcs := []struct {
		desc   string
		answer bqAnswer
		want   string
	}{
		{
			desc:   "another database error",
			answer: bqAnswer{status: 404, code: "60", body: "Code: 60. DB::Exception: Table eth_api.x does not exist. (UNKNOWN_TABLE) (version 25.9.4.58 (official build))"},
			want:   "unexpected status code: 404, response body: Code: 60. DB::Exception: Table eth_api.x does not exist. (UNKNOWN_TABLE)",
		},
		{
			desc:   "the header names another code",
			answer: bqAnswer{status: 500, code: "60", body: bqBusyOld},
			want:   "unexpected status code: 500, response body: Code: 202. DB::Exception: Too many simultaneous queries for user [user]. Current: 4, maximum: 4",
		},
		{
			desc:   "rows were sent before the exception",
			answer: bqAnswer{status: 200, format: "JSONEachRow", body: "{\"x\":1}\n{\"exception\": \"Code: 202. DB::Exception: Too many simultaneous queries for user mcp. Current: 4, maximum: 4. (TOO_MANY_SIMULTANEOUS_QUERIES) (version 25.9.4.58 (official build))\"}\n"},
			want:   "query failed after 1 result rows had been sent (partial result discarded): Code: 202. DB::Exception: Too many simultaneous queries for user [user]. Current: 4, maximum: 4. (TOO_MANY_SIMULTANEOUS_QUERIES)",
		},
	}
	for _, tc := range tcs {
		t.Run(tc.desc, func(t *testing.T) {
			server := bqBusyTestServer(t, func(int) bqAnswer { return tc.answer })
			ctx, _ := bitqueryTestContext(t)
			source := bitqueryTestSource(t, ctx, server.URL, "5s")
			_, err := bitqueryRun(t, ctx, source, server.URL+bitqueryTestQuery)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if n := len(server.requests()); n != 1 {
				t.Fatalf("server saw %d requests, want 1", n)
			}
		})
	}

	// A body that cannot be produced again is not retried.
	server := bqBusyTestServer(t, func(int) bqAnswer { return bqAnswer{status: 500, code: "202", body: bqBusyArray} })
	ctx, _ := bitqueryTestContext(t)
	source := bitqueryTestSource(t, ctx, server.URL, "5s")
	req, err := nethttp.NewRequestWithContext(ctx, nethttp.MethodPost, server.URL+bitqueryTestQuery, io.MultiReader(strings.NewReader("SELECT 1")))
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	if _, err := source.RunRequest(ctx, req); err == nil || !util.BitqueryIsBusyError(err) {
		t.Fatalf("got %v, want the refusal", err)
	}
	if n := len(server.requests()); n != 1 {
		t.Fatalf("server saw %d requests, want 1", n)
	}
}

func TestBitqueryRunRequestBusyRetryStopsAtTheDeadline(t *testing.T) {
	// A pause of at least 3.75 s does not fit in a 1 s deadline: no retry. The
	// deadline leaves the first request ample time even on a loaded machine.
	t.Cleanup(util.BitquerySetBusyPauses(5 * time.Second))
	server := bqBusyTestServer(t, func(int) bqAnswer { return bqAnswer{status: 500, code: "202", body: bqBusyArray} })
	ctx, logs := bitqueryTestContext(t)
	source := bitqueryTestSource(t, ctx, server.URL, "5s")
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	start := time.Now()
	_, err := bitqueryRun(t, ctx, source, server.URL+bitqueryTestQuery)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("RunRequest took %s", elapsed)
	}
	if err == nil || !util.BitqueryIsBusyError(err) {
		t.Fatalf("got %v, want the refusal", err)
	}
	if n := len(server.requests()); n != 1 {
		t.Fatalf("server saw %d requests, want 1", n)
	}
	if len(bqRetryLines(logs.String())) != 0 {
		t.Fatalf("a retry was logged: %q", logs.String())
	}
}

// A proxy's own refusals: the Bitquery gateway (HAProxy) and chproxy answer 429
// without reaching ClickHouse, so there is no X-ClickHouse-Exception-Code header.
const (
	bqProxyScope = `[ Id: 17A2B3C4D5E6F709; User "mcp"(1) proxying as "mcp"(1) to "10.20.30.40:8123"(3); RemoteAddr: "10.20.30.41:51234"; LocalAddr: "10.20.30.42:8123"; Duration: 12 μs]: `
	// Gateway: a user over its session limit, and a user blocked for long-running queries.
	bqGatewaySessions = "Too Many Sessions\n"
	bqGatewayBlocked  = "429 Too Many Requests — temporarily blocked due to a high number of long-running queries. Please try again in a few minutes."
	bqChproxyLimit    = bqProxyScope + `limits for user "mcp" are exceeded: max_concurrent_queries limit: 4` + "\n"
	bqChproxyRate     = bqProxyScope + `rate limit for user "mcp" is exceeded: requests_per_minute limit: 100` + "\n"
	bqChproxyTimeout  = bqProxyScope + `timeout for user "mcp" exceeded: 2m0s` + "\n"
)

func TestBitqueryRunRequestRetriesProxyConcurrencyLimit(t *testing.T) {
	bqFastPauses(t)
	withQuery := bqProxyScope + `limits for user "mcp" are exceeded: max_concurrent_queries limit: 4; query: "SELECT 1"` + "\n"
	for _, refusal := range []string{bqGatewaySessions, bqChproxyLimit, withQuery} {
		server := bqBusyTestServer(t, func(n int) bqAnswer {
			if n < 2 {
				return bqAnswer{status: 429, body: refusal}
			}
			return bqAnswer{status: 200, format: "JSONEachRow", body: "[\n{\"x\":1}\n]\n"}
		})
		ctx, logs := bitqueryTestContext(t)
		source := bitqueryTestSource(t, ctx, server.URL, "5s")
		got, err := bitqueryRun(t, ctx, source, server.URL+bitqueryTestQuery)
		if err != nil {
			t.Fatalf("%q: unexpected error: %v", refusal, err)
		}
		if gotJSON, _ := json.Marshal(got); string(gotJSON) != `[{"x":1}]` {
			t.Fatalf("%q: got %s", refusal, gotJSON)
		}
		if n := len(server.requests()); n != 3 {
			t.Fatalf("%q: server saw %d requests, want 3", refusal, n)
		}
		lines := bqRetryLines(logs.String())
		if len(lines) != 2 || !strings.Contains(lines[0], util.BitqueryBusyProxy) || strings.Contains(lines[0], "mcp") {
			t.Fatalf("%q: retry log lines %q: want 2, naming the proxy and no user", refusal, lines)
		}
	}

	// Always refused: the refusal after 1 attempt and 3 retries, without the user name.
	server := bqBusyTestServer(t, func(int) bqAnswer { return bqAnswer{status: 429, body: bqChproxyLimit} })
	ctx, _ := bitqueryTestContext(t)
	source := bitqueryTestSource(t, ctx, server.URL, "5s")
	_, err := bitqueryRun(t, ctx, source, server.URL+bitqueryTestQuery)
	if want := "unexpected status code: 429, response body: limits for user [user] are exceeded: max_concurrent_queries limit: 4"; err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
	if n := len(server.requests()); n != 4 {
		t.Fatalf("server saw %d requests, want 4", n)
	}
}

func TestBitqueryRunRequestDoesNotRetryAnEchoedRefusal(t *testing.T) {
	// A proxy error (no X-ClickHouse-Exception-Code header) whose query echo quotes
	// a caller's literal "Code: 202 …": not ClickHouse's refusal, sent once.
	bqFastPauses(t)
	echo := bqProxyScope + `cannot reach the backend; query: "SELECT 'Code: 202. DB::Exception: Too many simultaneous queries for user bob. Current: 4, maximum: 4. (TOO_MANY_SIMULTANEOUS_QUERIES)'"` + "\n"
	for _, status := range []int{502, 500} {
		server := bqBusyTestServer(t, func(int) bqAnswer { return bqAnswer{status: status, body: echo} })
		ctx, logs := bitqueryTestContext(t)
		source := bitqueryTestSource(t, ctx, server.URL, "5s")
		if _, err := bitqueryRun(t, ctx, source, server.URL+bitqueryTestQuery); err == nil {
			t.Fatalf("status %d: expected an error", status)
		}
		if n := len(server.requests()); n != 1 {
			t.Fatalf("status %d: server saw %d requests, want 1", status, n)
		}
		if len(bqRetryLines(logs.String())) != 0 {
			t.Fatalf("status %d: a retry was logged: %q", status, logs.String())
		}
	}
}

func TestBitqueryRunRequestDoesNotRetryOtherProxyRefusals(t *testing.T) {
	bqFastPauses(t)
	tcs := []struct {
		desc   string
		answer bqAnswer
		want   string
	}{
		{desc: "gateway block for minutes", answer: bqAnswer{status: 429, body: bqGatewayBlocked},
			want: "unexpected status code: 429, response body: " + bqGatewayBlocked},
		{desc: "chproxy rate limit", answer: bqAnswer{status: 429, body: bqChproxyRate},
			want: "unexpected status code: 429, response body: rate limit for user [user] is exceeded: requests_per_minute limit: 100"},
		{desc: "chproxy killed a running query", answer: bqAnswer{status: 504, body: bqChproxyTimeout},
			want: "unexpected status code: 504, response body: timeout for user [user] exceeded: 2m0s"},
		{desc: "a session refusal under another status", answer: bqAnswer{status: 503, body: bqGatewaySessions},
			want: "unexpected status code: 503, response body: " + bqGatewaySessions},
	}
	for _, tc := range tcs {
		t.Run(tc.desc, func(t *testing.T) {
			server := bqBusyTestServer(t, func(int) bqAnswer { return tc.answer })
			ctx, logs := bitqueryTestContext(t)
			source := bitqueryTestSource(t, ctx, server.URL, "5s")
			_, err := bitqueryRun(t, ctx, source, server.URL+bitqueryTestQuery)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if n := len(server.requests()); n != 1 {
				t.Fatalf("server saw %d requests, want 1", n)
			}
			if len(bqRetryLines(logs.String())) != 0 {
				t.Fatalf("a retry was logged: %q", logs.String())
			}
		})
	}
}
