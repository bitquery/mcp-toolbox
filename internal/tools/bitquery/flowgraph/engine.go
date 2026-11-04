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

package flowgraph

// The counterparties method. One call parses the 1–50 address set, the
// direction and filters, resolves the window to fixed instants, renders the
// chain's statement with built literals, runs it once through the shared runner,
// and shapes the top-K counterparties plus the below_cutoff remainder derived
// from the statement's WITH TOTALS row. The keyset cursor is a following change;
// the label filters are carried here and applied by the output shaping.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/tools/bitquery/chainexec"
	"github.com/googleapis/mcp-toolbox/internal/util"
)

const (
	// headerSender is the X-Bitquery-Table of the counterparties read: bare, as
	// the risk report names it; the database is set by the source's own header.
	headerSender = "transfers_sender"
	maxAddresses = 50
	defaultDays  = 90
	defaultLimit = 100
	maxLimit     = 1000
)

// Budget bounds a call: its deadline, and one statement's time and rows. The
// row budget is per call (one statement today, so it equals the statement cap).
type Budget struct {
	DeadlineSeconds     int   `yaml:"deadline_s"`
	StatementMaxSeconds int   `yaml:"statement_max_seconds"`
	StatementMaxRows    int64 `yaml:"statement_max_rows"`
	ReportRowsBudget    int64 `yaml:"report_rows_budget"`
}

func (b Budget) withDefaults() Budget {
	if b.DeadlineSeconds <= 0 {
		b.DeadlineSeconds = 60
	}
	if b.StatementMaxSeconds <= 0 {
		b.StatementMaxSeconds = 60
	}
	if b.StatementMaxRows <= 0 {
		b.StatementMaxRows = 100_000_000
	}
	if b.ReportRowsBudget <= 0 {
		b.ReportRowsBudget = 100_000_000
	}
	return b
}

func agentErr(format string, args ...any) util.ToolboxError {
	return util.NewAgentError(fmt.Sprintf(format, args...), nil)
}

// request is one parsed, validated call.
type request struct {
	chain           string
	addrs           []string // normalized, de-duplicated, valid, capped at 50
	invalid         []string
	requested       int
	groupBy         string
	direction       string // in | out | both
	wStart, wEnd    time.Time
	includeInternal int
	includeZero     int
	token           string // "" = all, else the chain's token key (EVM: 40 lower-hex, no 0x)
	minUsd          float64
	limit           int
	// labelTypes / excludeLabelTypes are carried for the output shaping to filter
	// counterparties by their labels; they do not reach the SQL.
	labelTypes        []string
	excludeLabelTypes []string
	format            string
}

// keepSender / keepReceiver turn the direction into the two statement arms.
func (r request) keepSender() bool   { return r.direction == "both" || r.direction == "out" }
func (r request) keepReceiver() bool { return r.direction == "both" || r.direction == "in" }

func (t Tool) run(ctx context.Context, p map[string]any) (any, util.ToolboxError) {
	start := time.Now()
	if t.chains == nil || t.labels == nil {
		return nil, util.NewClientServerError("the counterparties tool is not initialized with its sources", 500, nil)
	}
	req, terr := t.parse(p)
	if terr != nil {
		return nil, terr
	}
	codec, _ := codecFor(req.chain)
	setLit, err := codec.setLiteral(req.addrs)
	if err != nil {
		return nil, agentErr("%v", err)
	}
	statement, err := render(t.stmts[req.chain], stmtVars{
		SetLiteral:       setLit,
		WStart:           sqlTime(req.wStart),
		WEnd:             sqlTime(req.wEnd),
		IncludeInternal:  req.includeInternal,
		IncludeZeroValue: req.includeZero,
		KeepSender:       req.keepSender(),
		KeepReceiver:     req.keepReceiver(),
		TokenFilter:      req.token,
		MultiAddr:        len(req.addrs) > 1,
		TopK:             req.limit,
		MinUsd:           formatFloat(req.minUsd),
	})
	if err != nil {
		return nil, util.ProcessGeneralError(err)
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(t.budget.DeadlineSeconds)*time.Second)
	defer cancel()
	ex := &chainexec.Exec{
		Src:                 t.chains[req.chain],
		StatementMaxSeconds: t.budget.StatementMaxSeconds,
		StatementMaxRows:    t.budget.StatementMaxRows,
		ReportRowsBudget:    t.budget.ReportRowsBudget,
	}
	maxRows, err := ex.MaxRows()
	if err != nil {
		return nil, util.ProcessGeneralError(err)
	}
	// The generated statement carries no SETTINGS or FORMAT; the runner forces
	// FORMAT JSON, and the budget is applied here so max_execution_time is the
	// time left before the call's deadline (lowering a setting is allowed on the
	// readonly=2 gateway user). short_circuit keeps the gated price lookups from
	// running on every bucket.
	statement += settingsClause(maxRows, ex.Secs(ctx))
	doc, qerr := ex.QueryDoc(ctx, "counterparties", headerSender, statement)
	if qerr != nil {
		return nil, util.ProcessGeneralError(qerr)
	}

	// Labels for the returned counterparties (the top-K): group_by=address
	// attaches them per row; group_by=label_type totals the flow by each one's
	// primary type. Fetched once, over the returned page only.
	rows := chainexec.RowsOf(doc)
	addrs := make([]string, 0, len(rows))
	for _, r := range rows {
		addrs = append(addrs, strings.ToLower(chainexec.AsStr(r["Counterparty"])))
	}
	byAddr, partial, lerr := t.fetchLabels(ctx, ex, req.chain, addrs)
	if lerr != nil {
		return nil, util.ProcessGeneralError(lerr)
	}

	if req.groupBy == "label_type" {
		tres := t.shapeLabelType(req, doc, byAddr)
		tres.Partial = partial
		tres.Cost = ex.Cost
		tres.TimingMS = time.Since(start).Milliseconds()
		if req.format == "json" {
			return tres, nil
		}
		return tools.TextResult{Text: tres.markdown()}, nil
	}

	res := shapeAddress(req, doc)
	t.attachLabels(&res, byAddr)
	res.Partial = partial
	res.Cost = ex.Cost
	res.TimingMS = time.Since(start).Milliseconds()
	if req.format == "json" {
		return res, nil
	}
	return tools.TextResult{Text: res.markdown()}, nil
}

func (t Tool) parse(p map[string]any) (request, util.ToolboxError) {
	chain := strings.ToLower(strings.TrimSpace(chainexec.AsStr(p["chain"])))
	if chain == "" {
		return request{}, agentErr("chain is required")
	}
	codec, ok := codecFor(chain)
	if !ok {
		return request{}, agentErr("chain %q is not supported by this tool", chain)
	}
	if _, ok := t.stmts[chain]; !ok {
		return request{}, agentErr("chain %q is not available here (no statement configured)", chain)
	}
	if _, ok := t.chains[chain]; !ok {
		return request{}, agentErr("chain %q is not available here (no source configured)", chain)
	}
	req := request{chain: chain}

	raw := splitAddresses(chainexec.AsStr(p["addresses"]))
	req.requested = len(raw)
	seen := map[string]bool{}
	for _, a := range raw {
		if !codec.valid(a) {
			req.invalid = append(req.invalid, a)
			continue
		}
		n := codec.normalize(a)
		if seen[n] {
			continue
		}
		seen[n] = true
		if len(req.addrs) < maxAddresses {
			req.addrs = append(req.addrs, n)
		}
	}
	if len(req.addrs) == 0 {
		return request{}, agentErr("no valid %s address in addresses (%d invalid)", chain, len(req.invalid))
	}

	req.groupBy = strings.ToLower(strings.TrimSpace(chainexec.AsStr(p["group_by"])))
	if req.groupBy == "" {
		req.groupBy = "address"
	}
	switch req.groupBy {
	case "address", "label_type":
	default:
		return request{}, agentErr("group_by must be address or label_type")
	}

	req.direction = strings.ToLower(strings.TrimSpace(chainexec.AsStr(p["direction"])))
	if req.direction == "" {
		req.direction = "both"
	}
	switch req.direction {
	case "in", "out", "both":
	default:
		return request{}, agentErr("direction must be in, out or both")
	}

	before, hasBefore, err := parseTime(chainexec.AsStr(p["before_time"]))
	if err != nil {
		return request{}, agentErr("before_time: %v", err)
	}
	if !hasBefore {
		before = time.Now().UTC()
	}
	after, hasAfter, err := parseTime(chainexec.AsStr(p["after_time"]))
	if err != nil {
		return request{}, agentErr("after_time: %v", err)
	}
	if !hasAfter {
		after = before.AddDate(0, 0, -defaultDays)
	}
	if !after.Before(before) {
		return request{}, agentErr("after_time must be before before_time")
	}
	// The requested window is used as given — never silently shortened (the
	// eth_flows_between 366-day cut, which AT1/AT9 need lifted).
	req.wStart, req.wEnd = after, before

	if v, ok := p["include_internal"]; ok && v != nil {
		req.includeInternal = clampBit(chainexec.AsInt(v))
	} else {
		req.includeInternal = defaultIncludeInternal(chain)
	}

	if v, ok := p["include_zero_value"]; ok && v != nil {
		req.includeZero = clampBit(chainexec.AsInt(v))
	}

	if tok := strings.TrimSpace(chainexec.AsStr(p["token"])); tok != "" {
		key, ok := codec.tokenKey(tok)
		if !ok {
			return request{}, agentErr("token must be a %s token id", chain)
		}
		req.token = key
	}

	req.labelTypes, err = parseLabelTypes(chainexec.AsStr(p["label_type"]))
	if err != nil {
		return request{}, agentErr("label_type: %v", err)
	}
	req.excludeLabelTypes, err = parseLabelTypes(chainexec.AsStr(p["exclude_label_type"]))
	if err != nil {
		return request{}, agentErr("exclude_label_type: %v", err)
	}

	req.minUsd = asFloat(p["min_usd"])
	if req.minUsd < 0 || math.IsNaN(req.minUsd) || math.IsInf(req.minUsd, 0) {
		return request{}, agentErr("min_usd must be a non-negative number")
	}

	req.limit = defaultLimit
	if v, ok := p["limit"]; ok && v != nil {
		req.limit = int(chainexec.AsInt(v))
	}
	if req.limit < 1 {
		req.limit = 1
	}
	if req.limit > maxLimit {
		req.limit = maxLimit
	}

	req.format = strings.ToLower(strings.TrimSpace(chainexec.AsStr(p["format"])))
	if req.format == "" {
		req.format = "markdown"
	}
	if req.format != "markdown" && req.format != "json" {
		return request{}, agentErr("format must be markdown or json")
	}
	return req, nil
}

var labelTypeRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// parseLabelTypes splits a comma list of label types, lower-cases and validates
// each; the values are carried into the request and used by the output shaping
// to keep or drop counterparties, never put into SQL.
func parseLabelTypes(s string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		t := strings.ToLower(strings.TrimSpace(part))
		if t == "" {
			continue
		}
		if !labelTypeRe.MatchString(t) {
			return nil, fmt.Errorf("not a label type: %q", t)
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out, nil
}

// settingsClause is the SETTINGS the engine appends to every statement: the row
// budget (throw, never a short read), the per-statement deadline, and forced
// short-circuit so a gated price lookup does not run on every bucket.
func settingsClause(maxRows int64, maxSeconds int) string {
	return fmt.Sprintf("\nSETTINGS max_rows_to_read = %d, read_overflow_mode = 'throw', max_execution_time = %d, short_circuit_function_evaluation = 'force_enable'",
		maxRows, maxSeconds)
}

func splitAddresses(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == '|' || r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
}

func clampBit(n int64) int {
	if n != 0 {
		return 1
	}
	return 0
}

// defaultIncludeInternal is the transfer family's default: 1 on Arc (most USDC
// moves through contract code there), 0 elsewhere.
func defaultIncludeInternal(chain string) int {
	if chain == "arc" {
		return 1
	}
	return 0
}

var timeLayouts = []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02"}

// parseTime reads a UTC instant from a parameter; ("", false, nil) when empty.
func parseTime(s string) (time.Time, bool, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false, nil
	}
	if unix, err := strconv.ParseInt(s, 10, 64); err == nil && len(s) == 10 {
		return time.Unix(unix, 0).UTC(), true, nil
	}
	for _, l := range timeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC(), true, nil
		}
	}
	return time.Time{}, false, fmt.Errorf("not a time: %q (use YYYY-MM-DD, an RFC3339 timestamp or 10-digit unix seconds)", s)
}

func sqlTime(t time.Time) string {
	return fmt.Sprintf("toDateTime('%s','UTC')", t.UTC().Format("2006-01-02 15:04:05"))
}

func sqlTimePlain(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05")
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// emptyTime turns ClickHouse's zero DateTime into "".
func emptyTime(s string) string {
	if s == "" || strings.HasPrefix(s, "1970-01-01") {
		return ""
	}
	return s
}

func asFloatPtr(v any) *float64 {
	switch x := v.(type) {
	case nil:
		return nil
	case float64:
		return &x
	case float32:
		f := float64(x)
		return &f
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return nil
		}
		return &f
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return nil
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil
		}
		return &f
	}
	return nil
}

func asFloat(v any) float64 {
	if p := asFloatPtr(v); p != nil {
		return *p
	}
	return 0
}
