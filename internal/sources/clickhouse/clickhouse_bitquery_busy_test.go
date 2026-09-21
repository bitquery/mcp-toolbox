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

package clickhouse

// Bitquery: a query ClickHouse refuses for too many simultaneous queries (Code
// 202) is sent again after a short pause — including when the refused query is
// the handshake clickhouse-go runs to open a connection, which is where the
// production refusals landed — and the caller-facing error does not name the
// database user.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/googleapis/mcp-toolbox/internal/util"
	"go.opentelemetry.io/otel"
)

// The production refusal, as ClickHouse 25.6 sends it for default_format=Native.
const bqBusyBody = "Code: 202. DB::Exception: Too many simultaneous queries for user mcp. Current: 4, maximum: 4. (TOO_MANY_SIMULTANEOUS_QUERIES) (version 25.6.4.12 (official build))\n"

type bqRecordingServer struct {
	*httptest.Server
	mu      sync.Mutex
	queries []string
}

func (s *bqRecordingServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.queries...)
}

// bqBusyServer answers the n-th request (0-based) with answer(n): an HTTP status,
// the X-ClickHouse-Exception-Code header, and a body.
func bqBusyServer(t *testing.T, answer func(n int) (int, string, string)) *bqRecordingServer {
	t.Helper()
	rec := &bqRecordingServer{}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		n := len(rec.queries)
		rec.queries = append(rec.queries, string(query))
		rec.mu.Unlock()
		status, code, body := answer(n)
		if code != "" {
			w.Header().Set("X-ClickHouse-Exception-Code", code)
		}
		w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(rec.Close)
	return rec
}

func TestBitqueryRunSQLRetriesBusyHandshake(t *testing.T) {
	restore := util.BitquerySetBusyPauses(time.Millisecond, time.Millisecond, time.Millisecond)
	defer restore()
	server := bqBusyServer(t, func(int) (int, string, string) { return http.StatusInternalServerError, "202", bqBusyBody })

	ctx, source, logs := bitqueryTestSource(t, server.Listener.Addr().String())
	_, err := source.RunSQL(ctx, "SELECT 1", nil)
	if err == nil {
		t.Fatalf("expected an error")
	}
	want := "unable to execute query: Code: 202. DB::Exception: Too many simultaneous queries for user [user]. Current: 4, maximum: 4. (TOO_MANY_SIMULTANEOUS_QUERIES)"
	if err.Error() != want {
		t.Fatalf("got %q, want %q", err.Error(), want)
	}
	var dbErr *util.BitqueryDatabaseError
	if !errors.As(err, &dbErr) || dbErr.Code() != 202 {
		t.Fatalf("errors.As(*util.BitqueryDatabaseError) failed for %#v", err)
	}

	// One attempt and three retries, each refused while opening the connection.
	queries := server.seen()
	if len(queries) != 4 {
		t.Fatalf("server saw %d queries, want 4: %q", len(queries), queries)
	}
	for _, q := range queries {
		if !strings.Contains(q, "displayName()") {
			t.Fatalf("expected only connection handshakes, got %q", q)
		}
	}

	out := logs.String()
	var retries []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "data source busy, retrying") {
			retries = append(retries, line)
		}
	}
	if len(retries) != 3 {
		t.Fatalf("%d retry log lines, want 3: %q", len(retries), out)
	}
	for _, line := range retries {
		if !strings.Contains(line, "WARN") || !strings.Contains(line, "clickhouse-trading") || strings.Contains(line, "mcp") {
			t.Fatalf("retry log line %q: want WARN and the source, and no user name", line)
		}
	}
	// The final failure is logged once, with the original for the operator.
	if n := strings.Count(out, "data source query error"); n != 1 {
		t.Fatalf("%d query error log lines, want 1: %q", n, out)
	}
}

func TestBitqueryRunSQLStopsRetryingOnAnotherError(t *testing.T) {
	restore := util.BitquerySetBusyPauses(time.Millisecond, time.Millisecond, time.Millisecond)
	defer restore()
	server := bqBusyServer(t, func(n int) (int, string, string) {
		if n < 2 {
			return http.StatusInternalServerError, "202", bqBusyBody
		}
		return http.StatusNotFound, "60", "Code: 60. DB::Exception: Table trading_rt.no_such_table_xyz does not exist. (UNKNOWN_TABLE) (version 25.6.4.12 (official build))\n"
	})

	ctx, source, _ := bitqueryTestSource(t, server.Listener.Addr().String())
	_, err := source.RunSQL(ctx, "SELECT 1", nil)
	if want := "unable to execute query: Code: 60. DB::Exception: Table trading_rt.no_such_table_xyz does not exist. (UNKNOWN_TABLE)"; err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
	if n := len(server.seen()); n != 3 {
		t.Fatalf("server saw %d queries, want 3", n)
	}
}

func TestBitqueryRunSQLBusyRetryStopsAtTheDeadline(t *testing.T) {
	// A pause of at least 3.75 s does not fit in a 1 s deadline: no retry, and the
	// caller gets the refusal, not a timeout. The deadline leaves the first
	// attempt ample time even on a loaded machine.
	restore := util.BitquerySetBusyPauses(5 * time.Second)
	defer restore()
	server := bqBusyServer(t, func(int) (int, string, string) { return http.StatusInternalServerError, "202", bqBusyBody })
	ctx, source, logs := bitqueryTestSource(t, server.Listener.Addr().String())
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	start := time.Now()
	_, err := source.RunSQL(ctx, "SELECT 1", nil)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("RunSQL took %s", elapsed)
	}
	if err == nil || !util.BitqueryIsBusyError(err) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want the refusal", err)
	}
	if n := len(server.seen()); n != 1 {
		t.Fatalf("server saw %d queries, want 1", n)
	}
	if strings.Contains(logs.String(), "retrying") {
		t.Fatalf("a retry was logged: %q", logs.String())
	}
}

func TestBitqueryInitializeRetriesBusyPing(t *testing.T) {
	restore := util.BitquerySetBusyPauses(time.Millisecond, time.Millisecond, time.Millisecond)
	defer restore()
	server := bqBusyServer(t, func(int) (int, string, string) { return http.StatusInternalServerError, "202", bqBusyBody })
	host, port, _ := strings.Cut(server.Listener.Addr().String(), ":")

	ctx, _, _ := bitqueryTestSource(t, server.Listener.Addr().String())
	cfg := Config{Name: "clickhouse-trading", Type: SourceType, Host: host, Port: port, Database: "trading_rt", User: "guest", Protocol: "http"}
	_, err := cfg.Initialize(ctx, otel.Tracer("test"))
	if err == nil || !strings.Contains(err.Error(), "unable to connect successfully") {
		t.Fatalf("got %v, want a start failure", err)
	}
	if n := len(server.seen()); n != 4 {
		t.Fatalf("server saw %d queries, want 4 (the ping and three retries)", n)
	}
}
