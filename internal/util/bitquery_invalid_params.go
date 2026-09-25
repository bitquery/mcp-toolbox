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

// Bitquery-specific: tell a refusal apart from a failure. A tool validates one
// parameter with allowedValues and the server refuses a bad value before the
// query runs, as invalid params. A rule that relates two parameters (a window
// whose start is not before its end, two alternatives passed together) cannot be
// written that way, so a tool states it as a guard inside its own statement and
// the database raises it — arriving here as a database error, and reported to
// the caller as a failed call carrying an HTTP status. The caller's arguments
// were wrong either way, so the guard is reported as invalid params too.
package util

import "errors"

// BitqueryInvalidParamsRefusal reports whether err is a guard a statement raised
// on the caller's arguments, and returns the caller-facing message for it.
//
// The test is the database error code, not the text: ClickHouse raises 395
// (THROW_IF_VALUE_IS_NON_ZERO) only for throwIf, the one function a statement
// uses to refuse its own arguments. Every other refusal a server makes on its
// own — a row or byte limit, a timeout, a quota, too many queries, a missing
// table, a parse error — has a different code and is left alone. The message is
// the cleaned text without the framing of the failure shape: for code 395
// BitqueryCleanDatabaseMessage has already reduced it to "invalid request: "
// plus the guard author's own words, with no server detail and no echo of the
// input.
func BitqueryInvalidParamsRefusal(err error) (string, bool) {
	var dbErr *BitqueryDatabaseError
	if !errors.As(err, &dbErr) || dbErr.Code() != bqCodeThrowIf {
		return "", false
	}
	msg := dbErr.Message()
	if msg == "" {
		return "", false
	}
	return msg, true
}
