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

// Bitquery: over the HTTP protocol a connect/timeout failure must not hand the
// caller (the model) the request URL — internal host, port, database — while the
// server log keeps it; a ClickHouse error body still passes through.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/googleapis/mcp-toolbox/internal/log"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"go.opentelemetry.io/otel"
)

func bitqueryTestSource(t *testing.T, addr string) (context.Context, *Source, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	logger, err := log.NewLogger("standard", log.Info, logs, logs)
	if err != nil {
		t.Fatalf("failed to create logger: %v", err)
	}
	ctx := util.WithLogger(context.Background(), logger)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("bad addr %q: %v", addr, err)
	}
	pool, err := initClickHouseConnectionPool(ctx, otel.Tracer("test"), "clickhouse-trading", host, port, "guest", "", "trading_rt", "http", false)
	if err != nil {
		t.Fatalf("failed to create pool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	return ctx, &Source{Config: Config{Name: "clickhouse-trading", Type: SourceType, Host: host, Port: port}, Pool: pool}, logs
}

func bitqueryAssertNoAddress(t *testing.T, text, addr string) {
	t.Helper()
	host, port, _ := net.SplitHostPort(addr)
	for _, leak := range []string{host, port, "http://", "?", "guest", "database=", "trading_rt", "query_id", "dial tcp", "sendQuery"} {
		if strings.Contains(text, leak) {
			t.Fatalf("caller-facing error %q contains %q", text, leak)
		}
	}
}

func TestBitqueryRunSQLConnectionRefusedIsSanitized(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()

	ctx, source, logs := bitqueryTestSource(t, addr)
	_, err = source.RunSQL(ctx, "SELECT 1", nil)
	if err == nil {
		t.Fatalf("expected an error")
	}
	bitqueryAssertNoAddress(t, err.Error(), addr)
	if want := "unable to execute query: could not connect to the data service (connection refused)"; !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("got %q, want prefix %q", err.Error(), want)
	}
	for _, keep := range []string{"ERROR", "data source transport error", "clickhouse-trading", addr, "connection refused"} {
		if !strings.Contains(logs.String(), keep) {
			t.Fatalf("server log %q does not contain %q", logs.String(), keep)
		}
	}
}

func TestBitqueryRunSQLTimeoutIsSanitized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain the body so the server cancels r.Context() when the client leaves.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer server.Close()
	addr := server.Listener.Addr().String()

	ctx, source, logs := bitqueryTestSource(t, addr)
	// database/sql opens connections without the query context, so what expires on
	// a silent server is the driver's read timeout (300 s by default in the
	// toolbox DSN); shorten it for the test.
	pool, err := sql.Open("clickhouse", "http://guest:@"+addr+"/trading_rt?read_timeout=300ms")
	if err != nil {
		t.Fatalf("failed to open pool: %v", err)
	}
	defer pool.Close()
	source.Pool = pool
	_, err = source.RunSQL(ctx, "SELECT 1", nil)
	if err == nil {
		t.Fatalf("expected an error")
	}
	bitqueryAssertNoAddress(t, err.Error(), addr)
	if !strings.Contains(err.Error(), "the data service did not answer in time") {
		t.Fatalf("got %q, want a sanitized timeout", err.Error())
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("errors.Is(err, context.DeadlineExceeded) = false for %q", err)
	}
	if !strings.Contains(logs.String(), "data source transport error") {
		t.Fatalf("server log %q has no transport error line", logs.String())
	}
}

func TestBitqueryRunSQLDatabaseErrorUnchanged(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("Code: 62. DB::Exception: Syntax error"))
	}))
	defer server.Close()

	ctx, source, logs := bitqueryTestSource(t, server.Listener.Addr().String())
	_, err := source.RunSQL(ctx, "SELECT 1", nil)
	if err == nil {
		t.Fatalf("expected an error")
	}
	if !strings.Contains(err.Error(), "Code: 62. DB::Exception: Syntax error") || strings.Contains(err.Error(), "the data service") {
		t.Fatalf("database error body was altered: %q", err.Error())
	}
	if strings.Contains(logs.String(), "data source transport error") {
		t.Fatalf("database error logged as a transport failure: %q", logs.String())
	}
}
