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

// Bitquery: with returnFullError the caller gets the ClickHouse error body without
// the server version, replica host or expression context; the log keeps it.

import (
	"errors"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/googleapis/mcp-toolbox/internal/util"
)

func bitqueryErrorServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestBitqueryRunRequestDatabaseErrorIsCleaned(t *testing.T) {
	// A throwIf that failed on a replica, as a current server sends it for JSONEachRow
	// with output_format_json_array_of_rows=1.
	body := "[\n{\"exception\": \"Code: 395. DB::Exception: Received from chnode47-fast:9000. DB::Exception: remote-throw: while executing 'FUNCTION throwIf(greater(Block_Number, 0) :: 3, 'remote-throw' :: 2) -> throwIf(greater(Block_Number, 0), 'remote-throw') UInt8 : 1'. (FUNCTION_THROW_IF_VALUE_IS_NON_ZERO) (version 25.9.4.58 (official build))\"}\n]\n"
	server := bitqueryErrorServer(t, nethttp.StatusInternalServerError, body)

	ctx, logs := bitqueryTestContext(t)
	source := bitqueryTestSource(t, ctx, server.URL, "5s")
	_, err := bitqueryRun(t, ctx, source, server.URL+bitqueryTestQuery)
	if err == nil {
		t.Fatalf("expected an error")
	}
	if want := "unexpected status code: 500, response body: invalid request: remote-throw"; err.Error() != want {
		t.Fatalf("got %q, want %q", err.Error(), want)
	}
	var dbErr *util.BitqueryDatabaseError
	if !errors.As(err, &dbErr) || dbErr.Code() != 395 {
		t.Fatalf("errors.As(*util.BitqueryDatabaseError) failed for %#v", err)
	}
	out := logs.String()
	for _, keep := range []string{"WARN", "data source query error", "clickhouse-bsc-http", "chnode47-fast:9000", "version 25.9.4.58"} {
		if !strings.Contains(out, keep) {
			t.Fatalf("server log %q does not contain %q", out, keep)
		}
	}
}

func TestBitqueryRunRequestLegacyDatabaseErrorIsCleaned(t *testing.T) {
	body := "Code: 60, e.displayText() = DB::Exception: Received from ch1.dc426:9000. DB::Exception: Table bitcoin.no_such_table doesn't exist.. (version 20.8.11.17 (official build))\n"
	server := bitqueryErrorServer(t, nethttp.StatusNotFound, body)

	ctx, _ := bitqueryTestContext(t)
	source := bitqueryTestSource(t, ctx, server.URL, "5s")
	_, err := bitqueryRun(t, ctx, source, server.URL+bitqueryTestQuery)
	if want := "unexpected status code: 404, response body: Code: 60. DB::Exception: Table bitcoin.no_such_table doesn't exist.."; err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
}

func TestBitqueryRunRequestOtherErrorBodiesUnchanged(t *testing.T) {
	html := "<html><body><h1>404 Not Found</h1>\nThe resource could not be found.\n</body></html>\n"
	server := bitqueryErrorServer(t, nethttp.StatusNotFound, html)

	ctx, logs := bitqueryTestContext(t)
	source := bitqueryTestSource(t, ctx, server.URL, "5s")
	_, err := bitqueryRun(t, ctx, source, server.URL+bitqueryTestQuery)
	if want := "unexpected status code: 404, response body: " + html; err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}

	// Without returnFullError the body never reaches the caller.
	source.ReturnFullError = false
	dbServer := bitqueryErrorServer(t, nethttp.StatusInternalServerError, "Code: 62. DB::Exception: Syntax error. (SYNTAX_ERROR) (version 25.9.4.58 (official build))")
	_, err = bitqueryRun(t, ctx, source, dbServer.URL+bitqueryTestQuery)
	if want := "unexpected status code: 500 (Internal Server Error)"; err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
	if strings.Contains(logs.String(), "data source query error") {
		t.Fatalf("unexpected query error log: %q", logs.String())
	}
}
