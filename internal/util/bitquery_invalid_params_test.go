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
package util_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/googleapis/mcp-toolbox/internal/util"
)

const (
	bqGuardBody = `Code: 395. DB::Exception: after_time must be earlier than before_time: ` +
		`While executing MergeTreeSelect. (FUNCTION_THROW_IF_VALUE_IS_NON_ZERO) (version 25.9.1.1 (official build))`
	bqGuardWant = "invalid request: after_time must be earlier than before_time"
	bqTableBody = `Code: 60. DB::Exception: Table eth_api.nope does not exist. (UNKNOWN_TABLE) ` +
		`(version 25.9.1.1 (official build))`
	bqLimitBody = `Code: 158. DB::Exception: Limit for result exceeded, max rows: 1.00 million, ` +
		`current rows: 1.05 million. (TOO_MANY_ROWS_OR_BYTES) (version 25.9.1.1 (official build))`
)

// The three shapes a database error arrives in all carry the guard, and the
// framing Error() adds is not part of the message the caller is handed.
func TestBitqueryInvalidParamsRefusalEveryShape(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"http non-2xx", util.BitqueryDatabaseErrorResponse(ctx, "s", 500, []byte(bqGuardBody))},
		{"driver", fmt.Errorf("unable to execute query: %w",
			util.BitqueryDatabaseFailure(ctx, "s", errors.New(bqGuardBody)))},
		{"trailing exception", util.BitqueryDatabaseErrorInResult(ctx, "s", 7, bqGuardBody)},
		{"wrapped as an agent error", util.ProcessGeneralError(
			util.BitqueryDatabaseErrorResponse(ctx, "s", 500, []byte(bqGuardBody)))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := util.BitqueryInvalidParamsRefusal(tc.err)
			if !ok {
				t.Fatalf("not classified as a refusal: %v", tc.err)
			}
			if got != bqGuardWant {
				t.Fatalf("got %q, want %q", got, bqGuardWant)
			}
		})
	}
}

// Everything that is not a guard stays a failed call.
func TestBitqueryInvalidParamsRefusalLeavesTheRestAlone(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"plain error", errors.New("boom")},
		{"unknown table (code 60)", util.BitqueryDatabaseErrorResponse(ctx, "s", 404, []byte(bqTableBody))},
		{"result limit (code 158)", util.BitqueryDatabaseErrorResponse(ctx, "s", 500, []byte(bqLimitBody))},
		{"no clickhouse code at all", util.BitqueryDatabaseErrorResponse(ctx, "s", 502,
			[]byte("<html><body>502 Bad Gateway</body></html>"))},
		{"transport failure", util.BitqueryTransportFailure(ctx, "s",
			errors.New(`Get "http://10.0.0.1:8123/?query=x": dial tcp 10.0.0.1:8123: connect: refused`), 0)},
		{"http status with no body detail", fmt.Errorf("unexpected status code: 500 (Internal Server Error)")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := util.BitqueryInvalidParamsRefusal(tc.err); ok {
				t.Fatalf("classified as a refusal: %q (from %v)", got, tc.err)
			}
		})
	}
}

// The framing Error() reports is unchanged by the new field.
func TestBitqueryDatabaseErrorFramingUnchanged(t *testing.T) {
	ctx := context.Background()
	err := util.BitqueryDatabaseErrorResponse(ctx, "s", 500, []byte(bqGuardBody))
	if want := "unexpected status code: 500, response body: " + bqGuardWant; err.Error() != want {
		t.Fatalf("got %q, want %q", err.Error(), want)
	}
}
