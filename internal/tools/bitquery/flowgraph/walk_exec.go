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

// The production wiring of the walk: the per-hop fetch backed by chainexec, the
// edge-row decoder, the walk-config defaults, and runWalk — the Tool.Invoke
// branch taken when the tool is configured walk: true. The engine itself is in
// walk.go and is exercised offline with mock fetchers; this file is the only
// place that touches the live sources, and the live cfg flow_edges statement is a
// later step (runWalk renders whatever statement the chain is configured with).

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/tools/bitquery/chainexec"
	"github.com/googleapis/mcp-toolbox/internal/util"
)

const (
	// maxWalkSeeds caps the seed set; defaultHops / maxHops bound the depth.
	maxWalkSeeds = 10
	defaultHops  = 2
	maxHops      = 3
	// defaultPerNode is the per_node default (the LIMIT per_node BY Source); its hard
	// ceiling is walkPerNodeRows, which also bounds the rows one node's flow_edges
	// fetch may return.
	defaultPerNode  = 10
	walkPerNodeRows = 2000
	// defaultWalkMaxCost is the max_cost default: a BOUNDED, non-zero row budget. A
	// 3-hop walk is a heavy, resource-costly call (T17.1), so max_cost never defaults
	// to none — the default is this, clamped down to the cfg Budget.ReportRowsBudget.
	defaultWalkMaxCost = 50_000_000
	// Walk-config defaults, used when cfg leaves a knob at zero.
	defaultHubN      = 100
	defaultChunkSize = 100
	defaultEdgeCap   = 500
	// defaultWalkChunkRowBudget caps the rows one chunk's flow_edges read may scan, as
	// max_rows_to_read, so an over-budget chunk is refused before start (Code 158) and
	// bisected rather than running to the edge read's full cost.
	//
	// FLAGGED PLACEHOLDER: no cold-safe value is measured. Warm anchors (BIT-16046,
	// 2026-11-13): ~33M rows ≈ 27-29s, ~60M ≈ 56s. Pick a measured value before ship.
	defaultWalkChunkRowBudget int64 = 40_000_000
	// defaultWalkMaxRefusals caps the Code-158 chunk-cap refusals per call; past it the
	// remaining senders are left unexpanded (a graceful partial) instead of bisecting
	// further. FLAGGED PLACEHOLDER: not tuned against a live frontier.
	defaultWalkMaxRefusals = 64
)

// defaultStopGroups is the stop_at group -> label types map when cfg sets none:
// exchanges, mixers and bridges (plan section 2).
var defaultStopGroups = map[string][]string{
	"exchange": {"cex-deposit-address", "cex-hot-wallet", "cex-cold-wallet", "cex-unknown-type"},
	"mixer":    {"mixer"},
	"bridge":   {"bridge"},
}

// resolveWalkConfig applies the cfg walk knobs over the defaults.
func resolveWalkConfig(cfg Config) walkConfig {
	wc := walkConfig{
		hubN:           cfg.HubMaxCounterparties,
		chunkSize:      cfg.WalkChunkSize,
		edgeCap:        cfg.WalkEdgeCap,
		chunkRowBudget: cfg.WalkChunkRowBudget,
		maxRefusals:    cfg.WalkMaxRefusals,
		stopGroups:     cfg.StopAtGroups,
	}
	if wc.hubN <= 0 {
		wc.hubN = defaultHubN
	}
	if wc.chunkSize <= 0 {
		wc.chunkSize = defaultChunkSize
	}
	if wc.edgeCap <= 0 {
		wc.edgeCap = defaultEdgeCap
	}
	if wc.chunkRowBudget <= 0 {
		wc.chunkRowBudget = defaultWalkChunkRowBudget
	}
	if wc.maxRefusals <= 0 {
		wc.maxRefusals = defaultWalkMaxRefusals
	}
	if len(wc.stopGroups) == 0 {
		wc.stopGroups = defaultStopGroups
	}
	return wc
}

// execFetcher is the production hopFetcher: it renders the chain's flow_edges
// statement with the frontier chunk as the address set and runs it through
// chainexec (one billed statement, the MayStart deadline gate and the MaxRows row
// budget), then decodes the edge rows and their per-source Counterparties count.
type execFetcher struct {
	tmpl *template.Template
	// chunkRowBudget caps the rows one chunk's read may scan (max_rows_to_read), min
	// with the call budget left. When it is the binding limit and the statement
	// refuses before start (Code 158), fetchEdges returns errChunkTooBig so the engine
	// bisects. See the walkConfig doc.
	chunkRowBudget int64
	codec          chainCodec
	ex             *chainexec.Exec
	base           stmtVars
	header         string
	legacy         bool
}

// fetchEdges renders and runs the chain's flow_edges statement over the sources,
// bounded by max_rows_to_read. chunkCapped=true caps it at min(chunkRowBudget,
// remaining call budget): if the chunk budget is the binding limit (chunkRowBudget <=
// remaining) and the statement refuses before start with Code 158, it returns
// errChunkTooBig so the engine bisects. chunkCapped=false (the lone-seed path) caps it
// at the remaining call budget only, so a Code 158 there is a genuine call-budget stop
// (a normal stop error), never a bisect. Every other error — a timeout (159), a memory
// kill (241), a spent budget — passes through unchanged for isStopError to classify.
func (f *execFetcher) fetchEdges(ctx context.Context, sources []string, chunkCapped bool) (fetchResult, error) {
	setLit, err := f.codec.setLiteral(sources)
	if err != nil {
		return fetchResult{}, err
	}
	v := f.base
	v.SetLiteral = setLit
	stmt, err := render(f.tmpl, v)
	if err != nil {
		return fetchResult{}, err
	}
	remaining, err := f.ex.MaxRows()
	if err != nil {
		return fetchResult{}, err // ErrBudget: the call budget is spent -> isStopError stop.
	}
	limit := remaining
	chunkBinding := false
	if chunkCapped && f.chunkRowBudget > 0 && f.chunkRowBudget <= remaining {
		// The per-chunk budget is the binding limit: a Code-158 refusal here is the
		// chunk being too big, to be bisected — not the whole call's budget.
		limit = f.chunkRowBudget
		chunkBinding = true
	}
	stmt += settingsClause(limit, f.ex.Secs(ctx), f.legacy)
	doc, qerr := f.ex.QueryDoc(ctx, "flow_edges", f.header, stmt)
	if qerr != nil {
		if chunkBinding && isRowsOverflow(qerr) {
			return fetchResult{}, errChunkTooBig
		}
		return fetchResult{}, qerr
	}
	edges, counts, derr := parseEdgeRows(doc)
	if derr != nil {
		return fetchResult{}, derr
	}
	return fetchResult{edges: edges, counts: counts}, nil
}

// isRowsOverflow reports whether err is a ClickHouse rows-limit overflow (Code 158,
// TOO_MANY_ROWS) — the pre-start refusal max_rows_to_read raises, which reads 0 rows.
// It is deliberately NARROWER than isStopError: a timeout (159) or a memory kill (241)
// already ran and is a walk stop, not a bisect, so only a 158 is tagged here.
func isRowsOverflow(err error) bool {
	if err == nil {
		return false
	}
	var dbErr *util.BitqueryDatabaseError
	if errors.As(err, &dbErr) && dbErr.Code() == 158 {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "Code: 158") || strings.Contains(s, "TOO_MANY_ROWS")
}

// parseEdgeRows decodes a flow_edges document into edge rows and the per-source
// distinct-counterparty count (the Counterparties column, the same value on every row
// of a source, keyed by From). symbol_nonascii is derived in Go from the symbol, the
// times are normalised to RFC3339 Z (ClickHouse gives "YYYY-MM-DD hh:mm:ss"), and a
// non-finite amount_usd becomes null (never 0, and json.Marshal would fail on a NaN or
// Inf). A row with an empty from/to, or with no Counterparties column, is a decode
// failure (most likely the statement's column names do not match): it is returned as
// an error, not swallowed, so a mismatch does not degrade to a silent seeds-only graph
// or silently disable the hub stop.
func parseEdgeRows(doc map[string]any) ([]edgeRow, map[string]int, error) {
	rows := chainexec.RowsOf(doc)
	out := make([]edgeRow, 0, len(rows))
	counts := make(map[string]int, len(rows))
	for i, r := range rows {
		sym := chainexec.AsStr(r["Symbol"])
		er := edgeRow{
			From:           chainexec.AsStr(r["From"]),
			To:             chainexec.AsStr(r["To"]),
			Token:          chainexec.AsStr(r["Token"]),
			Symbol:         sym,
			SymbolNonascii: boolToInt(hasNonASCII(sym)),
			Amount:         chainexec.AsStr(r["Amount"]),
			AmountUsd:      finitePtr(asFloatPtr(r["Amount_Usd"])),
			UsdPricedShare: asFloat(r["Usd_Priced_Share"]),
			Transfers:      chainexec.AsInt(r["Transfers"]),
			FirstSeen:      toRFC3339(chainexec.AsStr(r["First_Seen"])),
			LastSeen:       toRFC3339(chainexec.AsStr(r["Last_Seen"])),
		}
		if er.From == "" || er.To == "" {
			return nil, nil, fmt.Errorf("flow_edges row %d has an empty from/to (the statement's From/To columns did not decode)", i)
		}
		cp, ok := r["Counterparties"]
		if !ok {
			return nil, nil, fmt.Errorf("flow_edges row %d has no Counterparties (the statement's Counterparties column did not decode); the hub stop needs it", i)
		}
		counts[er.From] = int(chainexec.AsInt(cp))
		out = append(out, er)
	}
	return out, counts, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func hasNonASCII(s string) bool {
	for _, r := range s {
		if r > 127 {
			return true
		}
	}
	return false
}

// finitePtr nils a non-finite float: one NaN or Inf kills the whole JSON marshal.
func finitePtr(p *float64) *float64 {
	if p == nil {
		return nil
	}
	if math.IsNaN(*p) || math.IsInf(*p, 0) {
		return nil
	}
	return p
}

// toRFC3339 normalises a ClickHouse datetime string to RFC3339 Z; "" for an empty
// or epoch time, and "" when it cannot be parsed (never the raw, unvalidated text).
func toRFC3339(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "1970-01-01") {
		return ""
	}
	for _, l := range timeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return ""
}

// parseHops reads the hops parameter: absent/zero -> default, clamped to [1, max].
func parseHops(p map[string]any) int {
	h := int(chainexec.AsInt(p["hops"]))
	if h == 0 {
		return defaultHops
	}
	if h < 1 {
		return 1
	}
	if h > maxHops {
		return maxHops
	}
	return h
}

// activeStopTypes is the set of label types that stop a node for this call: the
// types of the selected stop_at groups, or of every configured group when stop_at
// is empty. An unknown group name is a coded bad_request refusal that names the
// allowed groups.
func (t Tool) activeStopTypes(p map[string]any) (map[string]bool, util.ToolboxError) {
	groups := splitAddresses(chainexec.AsStr(p["stop_at"]))
	var selected []string
	if len(groups) == 0 {
		for g := range t.walkCfg.stopGroups {
			selected = append(selected, g)
		}
	} else {
		for _, g := range groups {
			g = strings.ToLower(strings.TrimSpace(g))
			if _, ok := t.walkCfg.stopGroups[g]; !ok {
				return nil, codedWalkErr("bad_request", "stop_at: unknown group %q (allowed: %s)", g, allowedGroups(t.walkCfg.stopGroups))
			}
			selected = append(selected, g)
		}
	}
	out := map[string]bool{}
	for _, g := range selected {
		for _, lt := range t.walkCfg.stopGroups[g] {
			out[lt] = true
		}
	}
	return out, nil
}

// allowedGroups is the sorted, comma-joined list of configured stop-group names, for
// the unknown-group error. The names are generic (exchange, mixer, bridge by default)
// and user-safe; sorting makes the message deterministic across the map's random order.
func allowedGroups(groups map[string][]string) string {
	names := make([]string, 0, len(groups))
	for g := range groups {
		names = append(names, g)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// article picks the English indefinite article for a word ("an" before a vowel), so
// the chain-mismatch message reads "an ethereum address", not "a ethereum address".
func article(s string) string {
	if s == "" {
		return "a"
	}
	switch s[0] {
	case 'a', 'e', 'i', 'o', 'u', 'A', 'E', 'I', 'O', 'U':
		return "an"
	}
	return "a"
}

// walkSeedInput is the seed set string: addresses when given, else address — the
// single-node alias the in-chat app's Expand sends. Accepting both lets the initial
// call (addresses) and Expand (address) use the same tool.
func walkSeedInput(p map[string]any) string {
	if s := strings.TrimSpace(chainexec.AsStr(p["addresses"])); s != "" {
		return s
	}
	return strings.TrimSpace(chainexec.AsStr(p["address"]))
}

// parsePerNode reads per_node (the LIMIT per_node BY Source): absent -> default 10,
// clamped to [1, walkPerNodeRows].
func parsePerNode(p map[string]any) int {
	v, ok := p["per_node"]
	if !ok || v == nil {
		return defaultPerNode
	}
	n := int(chainexec.AsInt(v))
	if n < 1 {
		n = 1
	}
	if n > walkPerNodeRows {
		n = walkPerNodeRows
	}
	return n
}

// parseMaxCost reads max_cost (the call's row budget): absent -> defaultWalkMaxCost,
// never none, itself capped to the ceiling (the cfg Budget.ReportRowsBudget). A value
// ABOVE the ceiling is REFUSED with a coded error, not silently clamped, so a caller
// asking for more budget than the tool allows is told, rather than quietly getting
// less than requested. A too-small value clamps up to 1.
func parseMaxCost(p map[string]any, ceiling int64) (int64, util.ToolboxError) {
	def := int64(defaultWalkMaxCost)
	if ceiling > 0 && def > ceiling {
		def = ceiling
	}
	v, ok := p["max_cost"]
	if !ok || v == nil {
		return def, nil
	}
	n := chainexec.AsInt(v)
	if n < 1 {
		n = 1
	}
	if ceiling > 0 && n > ceiling {
		return 0, codedWalkErr("bad_request", "max_cost=%d exceeds the maximum allowed (%d); lower it", n, ceiling)
	}
	return n, nil
}

// parseShare reads share (default true). false skips the view_url line in the text
// content (view_url is "" in v1 regardless). It accepts a bool, a bool string, or an
// integer (non-zero true).
func parseShare(p map[string]any) bool {
	v, ok := p["share"]
	if !ok || v == nil {
		return true
	}
	switch x := v.(type) {
	case bool:
		return x
	case string:
		if b, err := strconv.ParseBool(strings.TrimSpace(x)); err == nil {
			return b
		}
		return true
	}
	return chainexec.AsInt(v) != 0
}

// codedWalkErr is a walk isError result carrying a machine-readable code as the
// first token of its message (bad_address / chain_mismatch / budget_exceeded, plan
// §2). It is an AgentError, so the server returns it as an isError tools/call result
// (text, IsError: true): the in-chat app shows the text and an API client can split
// the code off the front. AgentError has no structured code field and the error path
// carries no structuredContent, so the code lives in the text.
func codedWalkErr(code, format string, args ...any) util.ToolboxError {
	return util.NewAgentError(code+": "+fmt.Sprintf(format, args...), nil)
}

// validForAnotherChain reports whether addr is a valid address of some registered
// chain OTHER than exclude — so a seed rejected by the tool chain but accepted
// elsewhere is a chain mismatch, not garbage. EVM chains share one codec, so a valid
// EVM address is caught by the tool chain's own codec before this is consulted.
func validForAnotherChain(addr, exclude string) bool {
	for name, c := range codecs {
		if name == exclude {
			continue
		}
		if c.valid(addr) {
			return true
		}
	}
	return false
}

// walkPrecheck runs the coded (isError-with-a-code) validations before the shared
// parse, so the three walk refusals carry their machine code: chain_mismatch (the
// chain is missing/unsupported/unconfigured, or a seed is a valid address of another
// chain) and bad_address (a seed is not a valid address of any chain, or the set is
// empty). It reads only the tool's configured chains and the codecs — no live query.
func (t Tool) walkPrecheck(p map[string]any) util.ToolboxError {
	chain := strings.ToLower(strings.TrimSpace(chainexec.AsStr(p["chain"])))
	if chain == "" {
		return codedWalkErr("chain_mismatch", "chain is required")
	}
	codec, ok := codecFor(chain)
	if !ok {
		return codedWalkErr("chain_mismatch", "chain %q is not supported by this tool", chain)
	}
	if _, ok := t.stmts[chain]; !ok {
		return codedWalkErr("chain_mismatch", "chain %q is not available here", chain)
	}
	if _, ok := t.chains[chain]; !ok {
		return codedWalkErr("chain_mismatch", "chain %q is not available here", chain)
	}
	raw := splitAddresses(walkSeedInput(p))
	if len(raw) == 0 {
		return codedWalkErr("bad_address", "at least one seed address is required")
	}
	var mismatched, garbage []string
	valid := 0
	for _, a := range raw {
		switch {
		case codec.valid(a):
			valid++
		case validForAnotherChain(a, chain):
			mismatched = append(mismatched, a)
		default:
			garbage = append(garbage, a)
		}
	}
	if len(mismatched) > 0 {
		list := strings.Join(mismatched, ", ")
		if len(mismatched) == 1 {
			return codedWalkErr("chain_mismatch", "seed %s is not %s %s address (it is valid on another chain); set chain to match the address", list, article(chain), chain)
		}
		return codedWalkErr("chain_mismatch", "seeds %s are not %s addresses (they are valid on another chain); set chain to match the address", list, chain)
	}
	if len(garbage) > 0 {
		list := strings.Join(garbage, ", ")
		if len(garbage) == 1 {
			return codedWalkErr("bad_address", "seed %s is not a valid %s address", list, chain)
		}
		return codedWalkErr("bad_address", "seeds %s are not valid %s addresses", list, chain)
	}
	if valid == 0 {
		return codedWalkErr("bad_address", "no valid %s address in the seed set", chain)
	}
	return nil
}

// budgetRefusal reports whether a finished walk is a hard budget refusal (an
// isError budget_exceeded) rather than a graceful partial: it stopped on its
// budget/deadline (engine-recorded, NOT inferred from the cost ledger — a thrown
// overflow adds no rows to the ledger) and gathered no edges, so there is nothing to
// return. A budget stop WITH edges returns the partial graph as success instead.
func budgetRefusal(res *walkResult) bool {
	return res.budgetStopped && len(res.Edges) == 0
}

// runWalk is the walk entry (Tool.Invoke branches here when cfg walk: true). It
// accepts the seed set as addresses or address, runs the coded prechecks, reuses
// t.parse for the common validation, reads the walk knobs (per_node, max_cost,
// share), wires the production fetch and label reads, runs the engine, and returns the
// flow_graph/1 document as structuredContent beside a text/CSV summary — or an isError
// code (bad_address / chain_mismatch / budget_exceeded / bad_request).
func (t Tool) runWalk(ctx context.Context, p map[string]any) (any, util.ToolboxError) {
	if t.chains == nil || t.labels == nil {
		return nil, util.NewClientServerError("the flow_graph tool is not initialized with its sources", 500, nil)
	}
	// Accept the seed set as address (what Expand sends) when addresses is absent, so
	// both the initial call and Expand use the same tool.
	p["addresses"] = walkSeedInput(p)
	if cerr := t.walkPrecheck(p); cerr != nil {
		return nil, cerr
	}
	req, terr := t.parse(p)
	if terr != nil {
		// t.parse is shared with counterparties and returns an uncoded agent error. Give
		// the walk's window-order error a machine code without touching t.parse.
		if strings.Contains(terr.Error(), "after_time must be before before_time") {
			return nil, codedWalkErr("bad_request", "after_time must be before before_time")
		}
		return nil, terr
	}
	if len(req.invalid) > 0 {
		// walkPrecheck already rejects an invalid seed with a code; this is a guard.
		return nil, codedWalkErr("bad_address", "every seed address must be a valid %s address; %d invalid (%s)", req.chain, len(req.invalid), strings.Join(req.invalid, ", "))
	}
	if len(req.addrs) > maxWalkSeeds {
		return nil, agentErr("at most %d seed addresses, got %d", maxWalkSeeds, len(req.addrs))
	}
	rawHops := int(chainexec.AsInt(p["hops"]))
	hops := parseHops(p)
	perNode := parsePerNode(p)
	maxCost, mcerr := parseMaxCost(p, t.budget.ReportRowsBudget)
	if mcerr != nil {
		return nil, mcerr
	}
	share := parseShare(p)
	stopTypes, serr := t.activeStopTypes(p)
	if serr != nil {
		return nil, serr
	}
	codec, _ := codecFor(req.chain)

	ctx, cancel := context.WithTimeout(ctx, time.Duration(t.budget.DeadlineSeconds)*time.Second)
	defer cancel()
	ex := &chainexec.Exec{
		Src:                 t.chains[req.chain],
		StatementMaxSeconds: t.budget.StatementMaxSeconds,
		StatementMaxRows:    t.budget.StatementMaxRows,
		// max_cost IS the call's row budget (plan.md: the toolbox never sees points).
		ReportRowsBudget: maxCost,
	}
	// The walk follows OUTGOING money, so every fetch is the sender arm only.
	base := stmtVars{
		WStart:           sqlTime(req.wStart),
		WEnd:             sqlTime(req.wEnd),
		IncludeInternal:  req.includeInternal,
		IncludeZeroValue: req.includeZero,
		KeepSender:       true,
		KeepReceiver:     false,
		TokenFilter:      req.token,
		// per_node is the LIMIT per_node BY Source the walk reads at each node.
		TopK:   perNode,
		MinUsd: formatFloat(req.minUsd),
		MaxTxs: req.maxTxs,
	}
	fetch := &execFetcher{
		tmpl:           t.stmts[req.chain],
		chunkRowBudget: t.walkCfg.chunkRowBudget,
		codec:          codec,
		ex:             ex,
		base:           base,
		header:         headerSender,
		legacy:         isLegacyCluster(codec),
	}
	labelChains := t.labelChainsFor(req.chain)
	label := func(ctx context.Context, addrs []string) (map[string][]labelRow, bool, error) {
		return t.fetchLabels(ctx, ex, codec, labelChains, addrs)
	}
	in := walkInput{
		chain: req.chain,
		codec: codec,
		seeds: req.addrs,
		hops:  hops,
		params: walkParamsOut{
			Address:         req.addrs,
			Hops:            hops,
			AfterTime:       req.wStart.UTC().Format(time.RFC3339),
			BeforeTime:      req.wEnd.UTC().Format(time.RFC3339),
			MinUsd:          req.minUsd,
			IncludeInternal: req.includeInternal,
			PerNode:         perNode,
			StopAt:          strings.TrimSpace(chainexec.AsStr(p["stop_at"])),
			Token:           req.token,
		},
		dataAsOf:    time.Now().UTC(),
		fetch:       fetch,
		label:       label,
		lm:          t.lm,
		labelChains: labelChains,
		noCoverage:  t.noCoverageChain(req.chain),
		cfg:         t.walkCfg,
		stopTypes:   stopTypes,
		cost:        func() chainexec.CostOut { return ex.Cost },
	}
	res, werr := runWalkGraph(ctx, in)
	if werr != nil {
		return nil, util.ProcessGeneralError(werr)
	}
	// Text-only echoes for the summary (never part of the flow_graph/1 document): the raw
	// hops when it was clamped, and the effective include_zero_value for the empty hint.
	if rawHops != 0 && rawHops != hops {
		res.hopsClampedFrom = rawHops
	}
	res.includeZero = req.includeZero
	if budgetRefusal(res) {
		return nil, codedWalkErr("budget_exceeded", "the walk reached its row budget (max_cost=%d) before finding any edges; narrow it (a shorter window, a single token, or a higher min_usd) or raise max_cost", maxCost)
	}
	return tools.StructuredResult{Content: []any{tools.TextResult{Text: res.content(share)}}, Structured: res.toMap()}, nil
}
