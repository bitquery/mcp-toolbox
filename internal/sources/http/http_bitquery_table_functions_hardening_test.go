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

// Bitquery: the http source refuses the evasions a plain name-and-paren check
// would miss — a vertical tab before the paren, an escaped quoted identifier,
// and a setting that carries a subquery — on the request body, and still sends
// a request the whole statement joins to a forbidden call across the `query`
// parameter / body boundary never leaves.

import (
	"errors"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/googleapis/mcp-toolbox/internal/util"
)

func TestBitqueryForbidTableFunctionsEvasions(t *testing.T) {
	for name, body := range map[string]string{
		"vertical tab":       "SELECT * FROM url\v('http://169.254.169.254/')",
		"escaped identifier": "SELECT * FROM `\\x75rl`('http://169.254.169.254/')",
		"setting subquery":   "SELECT 1 SETTINGS additional_table_filters = {'eth_api.transfers_sender': '1 IN (SELECT 1 FROM url(''http://a''))'}",
	} {
		t.Run(name, func(t *testing.T) {
			var hits int32
			server := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
				atomic.AddInt32(&hits, 1)
			}))
			defer server.Close()

			ctx, _ := bitqueryTestContext(t)
			source := bitqueryGuardedSource(t, server.URL, true)
			req, err := nethttp.NewRequestWithContext(ctx, nethttp.MethodPost, server.URL+bitqueryTestQuery, strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			_, err = source.RunRequest(ctx, req)
			var agentErr *util.AgentError
			if err == nil || !errors.As(err, &agentErr) {
				t.Fatalf("expected an agent refusal, got %v", err)
			}
			if strings.Contains(err.Error(), "169.254") || strings.Contains(err.Error(), "SELECT") {
				t.Fatalf("message echoes the statement: %q", err.Error())
			}
			if atomic.LoadInt32(&hits) != 0 {
				t.Fatalf("the request was sent %d time(s); a refused statement must never leave", hits)
			}
		})
	}
}
