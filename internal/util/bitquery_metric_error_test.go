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
	"net"
	"strings"
	"testing"
	"time"
)

func TestBitqueryMetricErrorType(t *testing.T) {
	const secret = "SELECT secret_column FROM customer_table WHERE wallet = 'abc'"
	ctx := context.Background()

	modern := BitqueryDatabaseErrorResponse(ctx, "src", 404, []byte(
		"Code: 47. DB::Exception: Unknown identifier: While processing "+secret+". (UNKNOWN_IDENTIFIER) (version 26.8.7.19 (official build))"))
	old := BitqueryDatabaseErrorResponse(ctx, "src", 404, []byte(
		"Code: 47, e.displayText() = DB::Exception: Missing columns: 'x' while processing query: '"+secret+"', required columns: 'x' (version 20.8.7.15 (official build))"))

	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"a database error from a modern server", modern, "db_code_47"},
		{"a database error from the old dialect", old, "db_code_47"},
		{"wrapped on the way to the tool layer", fmt.Errorf("error processing request: %w", modern), "db_code_47"},
		{"a database error that was never cleaned", fmt.Errorf("unable to execute query: Code: 62. DB::Exception: Syntax error: failed at position 8 (%s)", secret), "db_code_62"},
		{"the old dialect's text, never cleaned", fmt.Errorf("Code: 159, e.displayText() = DB::Exception: Timeout exceeded: %s", secret), "db_code_159"},
		{"a non-2xx answer that is no exception", fmt.Errorf("unexpected status code: 502, response body: %s", secret), "http_502"},
		{"a timeout", BitquerySanitizeTransportError(fmt.Errorf("%w: %s", context.DeadlineExceeded, secret), 180*time.Second), bitqueryMetricTimeout},
		{"a cancelled call", fmt.Errorf("%w: %s", context.Canceled, secret), bitqueryMetricCanceled},
		{"a refused connection", BitquerySanitizeTransportError(&net.OpError{Op: "dial", Err: errors.New("connection refused")}, time.Second), bitqueryMetricUnreachable},
		{"an agent error holding caller text", NewAgentError("invalid request: "+secret, nil), bitqueryMetricAgent},
		{"a server error holding caller text", NewClientServerError(secret, 500, nil), bitqueryMetricServer},
		{"anything else", errors.New(secret), bitqueryMetricOther},
		{"caller text dressed as a code", errors.New("Code: 1234567. DB::Exception: " + secret), bitqueryMetricOther},
		{"caller text dressed as a code, no framing", errors.New("Code: 12. " + secret), bitqueryMetricOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BitqueryMetricErrorType(tc.err)
			if got != tc.want {
				t.Fatalf("BitqueryMetricErrorType() = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "secret") || len(got) > 32 {
				t.Fatalf("the label is not bounded: %q", got)
			}
		})
	}
}
