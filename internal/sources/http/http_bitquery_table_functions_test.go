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

// Bitquery: a raw-SQL statement that calls url(), remote(), redis(), ... is
// refused by the source before any request leaves, when the source asks for it.

import (
	"errors"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/googleapis/mcp-toolbox/internal/util"
)

func bitqueryGuardedSource(t *testing.T, baseURL string, forbid bool) *Source {
	t.Helper()
	return &Source{
		Config: Config{
			Name:                 "clickhouse-eth-http",
			Type:                 SourceType,
			BaseURL:              baseURL,
			ReturnFullError:      true,
			AllowPrivateNetworks: true,
			ForbidTableFunctions: forbid,
		},
		client: nethttp.DefaultClient,
	}
}

func TestBitqueryForbidTableFunctionsRefusesBody(t *testing.T) {
	var hits int32
	server := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`[{"x":1}]`))
	}))
	defer server.Close()

	ctx, _ := bitqueryTestContext(t)
	source := bitqueryGuardedSource(t, server.URL, true)
	req, err := nethttp.NewRequestWithContext(ctx, nethttp.MethodPost, server.URL+bitqueryTestQuery,
		strings.NewReader("SELECT * FROM url('http://169.254.169.254/latest/meta-data/','RawBLOB','d String')"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = source.RunRequest(ctx, req)
	if err == nil {
		t.Fatalf("expected a refusal")
	}
	var agentErr *util.AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("refusal is %T, want *util.AgentError: %v", err, err)
	}
	if !strings.Contains(err.Error(), "url()") || strings.Contains(err.Error(), "169.254") {
		t.Fatalf("unexpected message %q", err.Error())
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatalf("the request was sent %d time(s); a refused statement must never leave", hits)
	}
}

func TestBitqueryForbidTableFunctionsRefusesQueryParam(t *testing.T) {
	var hits int32
	server := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer server.Close()

	ctx, _ := bitqueryTestContext(t)
	source := bitqueryGuardedSource(t, server.URL, true)
	req, err := nethttp.NewRequestWithContext(ctx, nethttp.MethodGet,
		server.URL+"/?user=mcp&query=SELECT+*+FROM+redis('172.16.1.1:6379','k','k+String')", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = source.RunRequest(ctx, req); err == nil || !strings.Contains(err.Error(), "redis()") {
		t.Fatalf("expected a redis() refusal, got %v", err)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatalf("the request was sent")
	}
}

func TestBitqueryForbidTableFunctionsPassesPlainSQL(t *testing.T) {
	const statement = "SELECT count() FROM eth_api.transfers_sender WHERE Transfer_Success = 1 LIMIT 1"
	var seen string
	server := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		seen = string(b[:n])
		if r.ContentLength != int64(len(statement)) {
			t.Errorf("Content-Length %d, want %d", r.ContentLength, len(statement))
		}
		_, _ = w.Write([]byte(`[{"count()":7}]`))
	}))
	defer server.Close()

	ctx, _ := bitqueryTestContext(t)
	source := bitqueryGuardedSource(t, server.URL, true)
	req, err := nethttp.NewRequestWithContext(ctx, nethttp.MethodPost, server.URL+bitqueryTestQuery, strings.NewReader(statement))
	if err != nil {
		t.Fatal(err)
	}
	got, err := source.RunRequest(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if seen != statement {
		t.Fatalf("the server received %q, want the statement unchanged", seen)
	}
	want := []any{map[string]any{"count()": 7.0}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("unexpected result (-want +got):\n%s", diff)
	}
}

func TestBitqueryForbidTableFunctionsOffByDefault(t *testing.T) {
	var hits int32
	server := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()

	ctx, _ := bitqueryTestContext(t)
	source := bitqueryGuardedSource(t, server.URL, false)
	req, err := nethttp.NewRequestWithContext(ctx, nethttp.MethodPost, server.URL+bitqueryTestQuery,
		strings.NewReader("SELECT * FROM url('http://a','RawBLOB','d String')"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = source.RunRequest(ctx, req); err != nil {
		t.Fatalf("unexpected error with the guard off: %v", err)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("the request was sent %d time(s), want 1", hits)
	}
}
