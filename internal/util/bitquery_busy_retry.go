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

// Bitquery-specific: send a query again when ClickHouse refused it for too many
// simultaneous queries (Code 202, TOO_MANY_SIMULTANEOUS_QUERIES). The server
// checks its limits while it registers a query, before it reads or sends
// anything, so a refused query did no work and a short pause often finds a free
// slot. The database user of the MCP servers has a small per-node limit shared by
// every server and caller, and a burst of tool calls runs into it — as does a new
// pooled connection, whose handshake (SELECT displayName(), version(), …) is a
// query too. A proxy in front of ClickHouse refusing a request for its own
// concurrency limit (HTTP 429, see bqProxyBusyRe) is treated the same way. Only a
// refusal is retried, never an answer that carried rows, and never past the
// caller's deadline.
package util

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// BitqueryCodeTooManySimultaneousQueries is ClickHouse's TOO_MANY_SIMULTANEOUS_QUERIES.
const BitqueryCodeTooManySimultaneousQueries = 202

// What a busy refusal came from, as the retry log line names it.
const (
	BitqueryBusyDatabase = "database: too many simultaneous queries (Code 202)"
	BitqueryBusyProxy    = "proxy: concurrency limit (HTTP 429)"
)

// bqProxyBusyRe is a proxy's refusal for its concurrency limit — the whole body
// of an HTTP 429: the Bitquery ClickHouse gateway (HAProxy) sends "Too Many
// Sessions" when a user runs more sessions than its limit; chproxy sends `limits
// for user "x" are exceeded: max_concurrent_queries limit: N` after its request
// scope, possibly followed by `; query: "<the query>"`. The proxies' other 429s
// are not retried: the gateway's "temporarily blocked … Please try again in a
// few minutes" and chproxy's requests_per_minute rate limit last far longer than
// the pauses here.
var bqProxyBusyRe = regexp.MustCompile(`^(?:\[ ?Id: .*?\]: ?)?(?:Too Many Sessions|limits for (?:cluster )?user "(?:[^"\\]|\\.)*" are exceeded: max_concurrent_queries limit: \d+(?:; query: "(?:[^"\\]|\\.)*")?)$`)

const (
	bqNameTooManySimultaneousQueries = "TOO_MANY_SIMULTANEOUS_QUERIES"
	// bqBusyMinAttempt is the least time that must be left before the deadline,
	// after the pause, for a retry to be worth sending: a retry cut by the deadline
	// would replace the refusal with a less useful timeout error.
	bqBusyMinAttempt = 100 * time.Millisecond
)

// bitqueryBusyPauses are the pauses before the first, second and third retry.
// Each is jittered by ±25% (bitqueryJitter) so that callers refused together do
// not come back together. Worst case, all retries spent: 2.125 s of pauses.
var bitqueryBusyPauses = []time.Duration{200 * time.Millisecond, 500 * time.Millisecond, time.Second}

// BitquerySetBusyPauses replaces the retry pauses — their count is the number of
// retries — and returns a function that restores the previous ones. It exists
// for tests; it is not safe to call while queries run.
func BitquerySetBusyPauses(pauses ...time.Duration) (restore func()) {
	previous := bitqueryBusyPauses
	bitqueryBusyPauses = append([]time.Duration(nil), pauses...)
	return func() { bitqueryBusyPauses = previous }
}

// BitqueryIsBusyError reports whether err is ClickHouse refusing a query for too
// many simultaneous queries — the database user's limit or the server's — in
// any form a source sees it: a driver error wrapping the HTTP body, the body
// itself, or an error already cleaned by BitqueryDatabaseFailure.
func BitqueryIsBusyError(err error) bool {
	if err == nil {
		return false
	}
	var dbErr *BitqueryDatabaseError
	if errors.As(err, &dbErr) {
		return dbErr.code == BitqueryCodeTooManySimultaneousQueries || dbErr.name == bqNameTooManySimultaneousQueries
	}
	return BitqueryIsBusyText(err.Error())
}

// BitqueryIsBusyText reports whether text — an error message or an HTTP error
// body — is that refusal. The text must be a ClickHouse exception whose code is
// 202 (the first "Code: N" in it, as BitqueryCleanDatabaseMessage reads it) or
// whose code name is TOO_MANY_SIMULTANEOUS_QUERIES; a mention of 202 elsewhere
// does not count, nor does anything from an echo of the caller's SQL on (a proxy's
// `; query: "…"` or a syntax error's fragment may quote "Code: 202" verbatim).
func BitqueryIsBusyText(text string) bool {
	if loc := bqSQLEchoRe.FindStringIndex(text); loc != nil {
		text = text[:loc[0]]
	}
	if !strings.Contains(text, "202") && !strings.Contains(text, bqNameTooManySimultaneousQueries) {
		return false
	}
	_, code, name, _ := BitqueryCleanDatabaseMessage(text)
	return code == BitqueryCodeTooManySimultaneousQueries || name == bqNameTooManySimultaneousQueries
}

// BitqueryBusyResponse reports whether an HTTP answer is a busy refusal, as the
// reason to log (BitqueryBusyDatabase or BitqueryBusyProxy), or "" when it is not
// one. Only a non-2xx answer can be one: the refusal comes before any row. For
// ClickHouse the X-ClickHouse-Exception-Code header decides when present (20.8
// and 25.x both send it); without it — a proxy may drop headers — the body of a
// 500 answer does (ClickHouse answers a refusal with 500; another status is not
// ClickHouse's own answer). A proxy's own refusal never reached ClickHouse and
// has no such header.
func BitqueryBusyResponse(status int, header http.Header, body []byte) string {
	switch {
	case status >= 200 && status <= 299:
		return ""
	case header.Get("X-ClickHouse-Exception-Code") != "":
		if strings.TrimSpace(header.Get("X-ClickHouse-Exception-Code")) == "202" {
			return BitqueryBusyDatabase
		}
		return ""
	case status == http.StatusTooManyRequests && bqProxyBusyRe.MatchString(strings.TrimSpace(string(body))):
		return BitqueryBusyProxy
	case status == http.StatusInternalServerError && BitqueryIsBusyText(string(body)):
		return BitqueryBusyDatabase
	}
	return ""
}

// BitqueryBusyRetryPause is called after an attempt was refused as busy (reason:
// BitqueryBusyDatabase or BitqueryBusyProxy); retried is the number of retries
// already made. It returns false — give up and report the refusal — when the
// retries are spent, when ctx is done, or when ctx would end before the pause and
// one more attempt are over. Otherwise it logs a WARN line (without the error
// text: a refusal names the database user), waits the jittered pause and returns
// true; it returns false if ctx ends while it waits.
func BitqueryBusyRetryPause(ctx context.Context, sourceName, reason string, retried int) bool {
	pauses := bitqueryBusyPauses
	if retried < 0 || retried >= len(pauses) || ctx.Err() != nil {
		return false
	}
	pause := bitqueryJitter(pauses[retried])
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < pause+bqBusyMinAttempt {
		return false
	}
	if logger, err := LoggerFromContext(ctx); err == nil {
		logger.WarnContext(ctx, "data source busy, retrying",
			"source", sourceName,
			"reason", reason,
			"retry", retried+1,
			"max_retries", len(pauses),
			"pause_ms", pause.Milliseconds())
	}
	timer := time.NewTimer(pause)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// BitqueryRetryBusy runs attempt, and runs it again after a pause while it fails
// with a busy refusal and BitqueryBusyRetryPause allows it. It returns the last
// attempt's result: on giving up, the refusal itself, not a context error. The
// caller must only wrap a step that has not handed out any row yet.
func BitqueryRetryBusy[T any](ctx context.Context, sourceName string, attempt func() (T, error)) (T, error) {
	for retried := 0; ; retried++ {
		v, err := attempt()
		if err == nil || !BitqueryIsBusyError(err) || !BitqueryBusyRetryPause(ctx, sourceName, BitqueryBusyDatabase, retried) {
			return v, err
		}
	}
}

// BitqueryRetryBusyCall is BitqueryRetryBusy for a step without a result.
func BitqueryRetryBusyCall(ctx context.Context, sourceName string, attempt func() error) error {
	_, err := BitqueryRetryBusy(ctx, sourceName, func() (struct{}, error) { return struct{}{}, attempt() })
	return err
}

// bitqueryJitter spreads d uniformly over [0.75·d, 1.25·d).
func bitqueryJitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.75 + 0.5*rand.Float64()))
}
