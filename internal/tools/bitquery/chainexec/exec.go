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

// Package chainexec is the shared statement runner for the Bitquery multi-statement
// tool kinds (the address risk report, and the flow-graph kind). It runs one
// read-only ClickHouse HTTP statement at a time against a chain source: each is
// billed to the caller through its own query_id, counted in the call's cost, held
// to the end of the query, and never started once the call's row budget is spent or
// its deadline is too close. The chain-specific statements, the address codecs and
// the result shaping live in the kinds that embed *Exec; this package has no chain
// logic.
package chainexec

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/googleapis/mcp-toolbox/internal/util"
)

// HTTPSource is a chain's transfer source (a ClickHouse HTTP endpoint).
type HTTPSource interface {
	HttpBaseURL() string
	HttpDefaultHeaders() map[string]string
	HttpQueryParams() map[string]string
	BitqueryLegacyBillingPaths() string
	RunRequest(context.Context, *http.Request) (any, error)
}

// StatementCost is one statement's line in the call's cost ledger.
type StatementCost struct {
	Name      string `json:"name"`
	RowsRead  int64  `json:"rows_read"`
	BytesRead int64  `json:"bytes_read"`
	ElapsedMS int64  `json:"elapsed_ms"`
}

// CostOut is the call's cost ledger: how many statements ran, the rows and bytes
// they read, and a line per statement.
type CostOut struct {
	Statements int             `json:"statements"`
	RowsRead   int64           `json:"rows_read"`
	BytesRead  int64           `json:"bytes_read"`
	Detail     []StatementCost `json:"detail"`
}

// Exec runs the statements of one call. Src is the chain source. The three budget
// knobs bound each statement's time and the whole call's rows; Cost is the running
// ledger. The embedding kind holds the labels source and any chain-specific state.
type Exec struct {
	Src  HTTPSource
	Cost CostOut
	// StatementMaxSeconds caps one statement's max_execution_time; ReportRowsBudget
	// caps the rows the whole call may read; StatementMaxRows caps one statement's.
	StatementMaxSeconds int
	StatementMaxRows    int64
	ReportRowsBudget    int64
	// last is how long the last statement of each kind took.
	last map[string]time.Duration
}

// ErrNoTime and ErrBudget carry the address-risk-report's exact wording ("report",
// and "a single asset") so that kind's client-facing refusals stay byte-identical
// after the lift; a kind with other nouns (e.g. flow-graph, which has a token not
// an asset) wraps or replaces them.
var ErrNoTime = fmt.Errorf("the report's deadline is too close to start another statement")

// ErrBudget stops a call whose statements have read its row budget.
var ErrBudget = fmt.Errorf("the report reached its row budget before it was complete; narrow it with after_time / before_time or a single asset")

// MinStartSeconds is the least time left in which a statement is started.
const MinStartSeconds = 5

// Secs is a statement's max_execution_time: StatementMaxSeconds, or the whole
// seconds left before the call's deadline when fewer, at least 1.
func (e *Exec) Secs(ctx context.Context) int {
	s := e.StatementMaxSeconds
	if dl, ok := ctx.Deadline(); ok {
		s = min(s, int(time.Until(dl)/time.Second))
	}
	return max(1, s)
}

// MayStart refuses a statement when the time left before the call's deadline is
// under the larger of MinStartSeconds and the last statement of the same kind.
func (e *Exec) MayStart(ctx context.Context, kind string) error {
	dl, ok := ctx.Deadline()
	if !ok {
		return nil
	}
	need := max(MinStartSeconds*time.Second, e.last[kind])
	if time.Until(dl) < need {
		return ErrNoTime
	}
	return nil
}

// Took records how long the last statement of a kind took.
func (e *Exec) Took(kind string, d time.Duration) {
	if e.last == nil {
		e.last = map[string]time.Duration{}
	}
	e.last[kind] = d
}

// MaxRows is the rows the next statement may read: the row budget left, or one
// statement's cap when fewer. It errors when the budget is spent.
func (e *Exec) MaxRows() (int64, error) {
	left := e.ReportRowsBudget - e.Cost.RowsRead
	if left <= 0 {
		return 0, ErrBudget
	}
	return min(left, e.StatementMaxRows), nil
}

// Query posts one statement to the chain source and returns its rows. header is
// the table the statement reads, as the X-Bitquery-Table header names it.
func (e *Exec) Query(ctx context.Context, name, header, statement string) ([]map[string]any, error) {
	if err := e.MayStart(ctx, name); err != nil {
		return nil, err
	}
	start := time.Now()
	u, err := url.Parse(strings.TrimRight(e.Src.HttpBaseURL(), "/") + "/")
	if err != nil {
		return nil, fmt.Errorf("error building the request URL: %w", err)
	}
	q := u.Query()
	for k, v := range e.Src.HttpQueryParams() {
		q.Set(k, v)
	}
	// Bitquery: each statement carries its own billing query_id, built from the
	// caller identity, as the http tool does for its one statement.
	var qid string
	var stamp bool
	if paths := e.Src.BitqueryLegacyBillingPaths(); paths != "" {
		qid, stamp = util.BitqueryLegacyClickhouseQueryID(ctx, paths)
	} else {
		qid, stamp = util.BitqueryClickhouseQueryID(ctx)
	}
	if stamp {
		q.Set("query_id", qid)
	}
	// A statement answers FORMAT JSON, one document: with the answer held to the
	// end of the query, a limit or a timeout that fires after the first block is
	// an HTTP error (classified as a limit), not a body cut short. Set here,
	// whatever the source's own parameters say.
	q.Set("wait_end_of_query", "1")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(statement))
	if err != nil {
		return nil, fmt.Errorf("error creating the request: %w", err)
	}
	for k, v := range e.Src.HttpDefaultHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set("X-Bitquery-Table", header)
	resp, err := e.Src.RunRequest(ctx, req)
	e.Took(name, time.Since(start))
	if err != nil {
		// A refused or failed statement is listed too: it was sent, and may have
		// read before it stopped.
		e.Cost.Statements++
		e.Cost.Detail = append(e.Cost.Detail, StatementCost{Name: name + " (failed)", ElapsedMS: time.Since(start).Milliseconds()})
		return nil, err
	}
	doc, ok := resp.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("unexpected answer to the %s statement", name)
	}
	sc := StatementCost{Name: name, ElapsedMS: time.Since(start).Milliseconds()}
	if st, ok := doc["statistics"].(map[string]any); ok {
		sc.RowsRead = AsInt(st["rows_read"])
		sc.BytesRead = AsInt(st["bytes_read"])
	}
	e.Cost.Statements++
	e.Cost.RowsRead += sc.RowsRead
	e.Cost.BytesRead += sc.BytesRead
	e.Cost.Detail = append(e.Cost.Detail, sc)
	data, _ := doc["data"].([]any)
	rows := make([]map[string]any, 0, len(data))
	for _, d := range data {
		if r, ok := d.(map[string]any); ok {
			rows = append(rows, r)
		}
	}
	return rows, nil
}

// AsInt coerces a ClickHouse JSON value to int64.
func AsInt(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case uint64:
		return int64(n)
	case int:
		return int64(n)
	case uint32:
		return int64(n)
	case int32:
		return int64(n)
	case uint8:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	}
	return 0
}

// AsStr coerces a value to string.
func AsStr(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case nil:
		return ""
	case []byte:
		return string(s)
	}
	return fmt.Sprint(v)
}

// AsBig coerces a value to a big.Int (0 on failure).
func AsBig(v any) *big.Int {
	s := strings.TrimSpace(AsStr(v))
	if s == "" {
		return new(big.Int)
	}
	b, ok := new(big.Int).SetString(s, 10)
	if !ok {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			b, _ = big.NewFloat(f).Int(nil)
			return b
		}
		return new(big.Int)
	}
	return b
}

// AsStrings coerces a value to a slice of non-empty strings.
func AsStrings(v any) []string {
	arr, _ := v.([]any)
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		if s := AsStr(x); s != "" {
			out = append(out, s)
		}
	}
	return out
}
