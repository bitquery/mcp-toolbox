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

// Bitquery: a statement that calls url(), remote(), redis(), ... is refused by
// RunSQL before it reaches the database, on every ClickHouse source.

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/googleapis/mcp-toolbox/internal/log"
	"github.com/googleapis/mcp-toolbox/internal/util"
)

func bitqueryTestContextOnly(t *testing.T) (context.Context, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	logger, err := log.NewLogger("standard", log.Info, logs, logs)
	if err != nil {
		t.Fatalf("failed to create logger: %v", err)
	}
	return util.WithLogger(context.Background(), logger), logs
}

func TestBitqueryRunSQLRefusesTableFunctions(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte("1\n"))
	}))
	defer server.Close()

	ctx, source, logs := bitqueryTestSource(t, server.Listener.Addr().String())
	atomic.StoreInt32(&hits, 0) // count only what RunSQL sends
	for sql, want := range map[string]string{
		"SELECT * FROM url('http://172.16.121.1:8123/ping','LineAsString','x String')": "url()",
		"SELECT * FROM remote('ch-eth-api.api-cluster.local:9000', system.query_log)":  "remote()",
		"SELECT * FROM `redis`('172.16.121.1:6379','k','k String')":                    "redis()",
		"SELECT * FROM file('/etc/passwd', 'LineAsString')":                            "file()",
	} {
		_, err := source.RunSQL(ctx, sql, nil)
		if err == nil {
			t.Fatalf("%q: expected a refusal", sql)
		}
		var agentErr *util.AgentError
		if !errors.As(err, &agentErr) {
			t.Fatalf("%q: refusal is %T, want *util.AgentError: %v", sql, err, err)
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("%q: message %q does not name %s", sql, err.Error(), want)
		}
		bitqueryAssertNoAddress(t, err.Error(), server.Listener.Addr().String())
		if strings.Contains(err.Error(), "172.16") || strings.Contains(err.Error(), "/etc/") {
			t.Fatalf("%q: message echoes the statement: %q", sql, err.Error())
		}
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatalf("the database received %d request(s); a refused statement must never be sent", hits)
	}
	if strings.Contains(logs.String(), "transport error") {
		t.Fatalf("a refusal was logged as a transport failure: %q", logs.String())
	}
}

func TestBitqueryRunSQLRefusalNeedsNoPool(t *testing.T) {
	// The guard runs before the pool is touched: a Source with no pool still
	// refuses, instead of panicking.
	source := &Source{Config: Config{Name: "clickhouse-trading", Type: SourceType}}
	ctx, _ := bitqueryTestContextOnly(t)
	_, err := source.RunSQL(ctx, "SELECT * FROM s3('https://b.s3.amazonaws.com/k')", nil)
	if err == nil || !strings.Contains(err.Error(), "s3()") {
		t.Fatalf("expected an s3() refusal, got %v", err)
	}
}
