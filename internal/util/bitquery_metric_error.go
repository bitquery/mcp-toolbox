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

// Bitquery-specific: what the metric attribute error.type may hold. A metric
// keeps one series per distinct attribute value for the whole retention, and the
// text of a database error is not safe there even once it is cleaned: ClickHouse
// quotes the statement in its own message ("while processing query: '...'", the
// parser's "failed at position N (...)"), so every failed statement of a raw-SQL
// tool became a label holding the caller's SQL. Measured 2026-11-02: 68 of 173
// labels over 14 days, the longest 16,384 characters. The label records the KIND
// of the failure; the text stays where it belongs, in the tool's answer to the
// caller and in the server log.
package util

import (
	"context"
	"errors"
	"regexp"
	"strconv"
)

// The bounded values of error.type that carry no database code.
const (
	bitqueryMetricCanceled    = "canceled"
	bitqueryMetricTimeout     = "data_service_timeout"
	bitqueryMetricUnreachable = "data_service_unreachable"
	bitqueryMetricAgent       = "agent_error"
	bitqueryMetricServer      = "server_error"
	bitqueryMetricOther       = "error"
)

// bqMetricCodeRe finds the code of a ClickHouse exception in an error that was
// not turned into a *BitqueryDatabaseError (a text with no server detail in it is
// passed through unchanged). It requires the exception's own framing and at most
// four digits, so text a caller wrote cannot mint new label values.
var bqMetricCodeRe = regexp.MustCompile(`\bCode: (\d{1,4})[.,] (?:DB::Exception|e\.displayText)`)

// bqMetricStatusRe finds the status of a non-2xx answer that was not a database
// exception, such as a balancer's 502 or a proxy's 429.
var bqMetricStatusRe = regexp.MustCompile(`\bunexpected status code: (\d{3})\b`)

// BitqueryMetricErrorType returns the value the metric attribute error.type
// records for err: a short bounded kind, never any part of the error's text. It
// is "" for a nil error.
func BitqueryMetricErrorType(err error) string {
	if err == nil {
		return ""
	}
	var dbErr *BitqueryDatabaseError
	if errors.As(err, &dbErr) && dbErr.Code() > 0 {
		return "db_code_" + strconv.Itoa(dbErr.Code())
	}
	text := err.Error()
	if m := bqMetricCodeRe.FindStringSubmatch(text); m != nil {
		return "db_code_" + m[1]
	}
	if errors.Is(err, context.Canceled) {
		return bitqueryMetricCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return bitqueryMetricTimeout
	}
	var transport *BitqueryTransportError
	if errors.As(err, &transport) || BitqueryIsTransportError(err) {
		return bitqueryMetricUnreachable
	}
	if m := bqMetricStatusRe.FindStringSubmatch(text); m != nil {
		return "http_" + m[1]
	}
	var toolboxErr ToolboxError
	if errors.As(err, &toolboxErr) {
		if toolboxErr.Category() == CategoryAgent {
			return bitqueryMetricAgent
		}
		return bitqueryMetricServer
	}
	return bitqueryMetricOther
}
