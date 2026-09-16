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

// Bitquery: a ClickHouse error reaches the caller without the server version or
// the expression context; a throwIf guard reaches it as the guard's message.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/googleapis/mcp-toolbox/internal/util"
)

func TestBitqueryRunSQLThrowIfIsCleaned(t *testing.T) {
	body := "Code: 395. DB::Exception: addresses: at most 50 distinct addresses per call - split the list: while executing 'FUNCTION throwIf(greater(length(Wanted), 50) :: 0, 'addresses: at most 50 distinct addresses per call - split the list' :: 3) -> throwIf(greater(length(Wanted), 50), 'addresses: at most 50 distinct addresses per call - split the list') UInt8 : 2'. (FUNCTION_THROW_IF_VALUE_IS_NON_ZERO) (version 25.6.4.12 (official build))\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	ctx, source, logs := bitqueryTestSource(t, server.Listener.Addr().String())
	_, err := source.RunSQL(ctx, "SELECT 1", nil)
	if err == nil {
		t.Fatalf("expected an error")
	}
	if want := "unable to execute query: invalid request: addresses: at most 50 distinct addresses per call - split the list"; err.Error() != want {
		t.Fatalf("got %q, want %q", err.Error(), want)
	}
	var dbErr *util.BitqueryDatabaseError
	if !errors.As(err, &dbErr) || dbErr.Code() != 395 {
		t.Fatalf("errors.As(*util.BitqueryDatabaseError) failed for %#v", err)
	}
	out := logs.String()
	for _, keep := range []string{"WARN", "data source query error", "clickhouse-trading", "while executing", "version 25.6.4.12"} {
		if !strings.Contains(out, keep) {
			t.Fatalf("server log %q does not contain %q", out, keep)
		}
	}
	if strings.Contains(out, "data source transport error") {
		t.Fatalf("database error logged as a transport failure: %q", out)
	}
}
