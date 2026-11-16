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

// Tests for the walk ticket gaps (BIT-16046): the hub decision and the chunk-cap
// bisect through the production fetch, the per_node / max_cost / share parameters, the
// address || addresses seed alias, the edges CSV in the text content, and the isError
// codes. The engine-level walk tests are in walk_test.go.

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"text/template"

	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/tools/bitquery/chainexec"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

// tronA (a valid Tron base58 address, a seed valid on another chain) is declared in
// tron_test.go in this package.

// --- Gap 2: the walk knobs --------------------------------------------------

func TestParseWalkKnobs(t *testing.T) {
	if parsePerNode(map[string]any{}) != defaultPerNode {
		t.Errorf("per_node default = %d, want %d", parsePerNode(map[string]any{}), defaultPerNode)
	}
	if parsePerNode(map[string]any{"per_node": int64(0)}) != 1 {
		t.Error("per_node must clamp low to 1")
	}
	if parsePerNode(map[string]any{"per_node": int64(99999)}) != walkPerNodeRows {
		t.Errorf("per_node must clamp high to %d", walkPerNodeRows)
	}
	if parsePerNode(map[string]any{"per_node": int64(7)}) != 7 {
		t.Error("per_node explicit value")
	}

	// max_cost defaults to a BOUNDED, non-zero value — never none (T17.1) — itself
	// capped to the cfg Budget.ReportRowsBudget ceiling.
	if got, err := parseMaxCost(map[string]any{}, 100_000_000); err != nil || got != defaultWalkMaxCost {
		t.Errorf("max_cost default = %d (err %v), want %d (bounded, not none)", got, err, defaultWalkMaxCost)
	}
	if got, err := parseMaxCost(map[string]any{}, 1_000_000); err != nil || got != 1_000_000 {
		t.Errorf("max_cost default must clamp down to the ceiling, got %d (err %v)", got, err)
	}
	if got, err := parseMaxCost(map[string]any{"max_cost": int64(0)}, 100_000_000); err != nil || got != 1 {
		t.Errorf("max_cost must clamp low to 1, got %d (err %v)", got, err)
	}
	// A value ABOVE the ceiling is REFUSED with a coded error, not silently clamped.
	if _, err := parseMaxCost(map[string]any{"max_cost": int64(999_000_000)}, 100_000_000); err == nil {
		t.Error("max_cost above the ceiling must be a coded refusal, not a clamp")
	} else if !strings.HasPrefix(err.Error(), "bad_request: ") {
		t.Errorf("max_cost-above-ceiling error %q must carry the bad_request code", err.Error())
	}
	if got, err := parseMaxCost(map[string]any{"max_cost": int64(12345)}, 100_000_000); err != nil || got != 12345 {
		t.Errorf("max_cost explicit = %d (err %v)", got, err)
	}

	if !parseShare(map[string]any{}) {
		t.Error("share must default to true")
	}
	if parseShare(map[string]any{"share": false}) {
		t.Error("share=false")
	}
	if parseShare(map[string]any{"share": "false"}) {
		t.Error("share='false' string")
	}
	if !parseShare(map[string]any{"share": "true"}) {
		t.Error("share='true' string")
	}
}

// --- Gap 3: the address || addresses seed alias -----------------------------

func TestWalkSeedInput(t *testing.T) {
	if walkSeedInput(map[string]any{"address": addrA}) != addrA {
		t.Error("address must be accepted when addresses is absent (the app's Expand sends address)")
	}
	if walkSeedInput(map[string]any{"addresses": addrB, "address": addrA}) != addrB {
		t.Error("addresses must win when both are present")
	}
	if walkSeedInput(map[string]any{}) != "" {
		t.Error("neither given -> empty")
	}
}

// TestParseParamsAddressAliasReachesSeed: through real param parsing (the server
// path), a client's `address` reaches the seed when `addresses` is absent.
func TestParseParamsAddressAliasReachesSeed(t *testing.T) {
	// addresses and address are OPTIONAL on a walk tool (either may be sent).
	ps := parameters.Parameters{
		parameters.NewStringParameter("chain", "c", parameters.WithStringRequired(true)),
		parameters.NewStringParameter("addresses", "a", parameters.WithStringRequired(false)),
		parameters.NewStringParameter("address", "alias", parameters.WithStringRequired(false)),
	}
	pv, err := parameters.ParseParams(ps, map[string]any{"chain": "ethereum", "address": addrA}, nil)
	if err != nil {
		t.Fatalf("ParseParams: %v", err)
	}
	if got := walkSeedInput(pv.AsMap()); got != addrA {
		t.Fatalf("walkSeedInput = %q, want %q (address reaches the seed)", got, addrA)
	}
	pv2, _ := parameters.ParseParams(ps, map[string]any{"chain": "ethereum", "addresses": addrB, "address": addrA}, nil)
	if got := walkSeedInput(pv2.AsMap()); got != addrB {
		t.Fatalf("walkSeedInput = %q, want addresses %q to win", got, addrB)
	}
}

// --- Gap 2/3: the load contract through Config.Initialize --------------------

func walkCfgBase(ps parameters.Parameters, walk bool) Config {
	return Config{
		ConfigBase:   tools.ConfigBase{Name: "fg", Description: "flow graph"},
		Type:         resourceType,
		Source:       "eth_src",
		ChainSources: map[string]string{"ethereum": "eth_src"},
		LabelsSource: "labels_src",
		Statements:   map[string]string{"ethereum": "SELECT {{.TopK}} FROM t WHERE x IN {{.SetLiteral}}"},
		Walk:         walk,
		Parameters:   ps,
	}
}

func TestInitializeWalkAddressAliasLoads(t *testing.T) {
	ps := parameters.Parameters{
		parameters.NewStringParameter("chain", "c", parameters.WithStringRequired(true)),
		parameters.NewStringParameter("address", "seed"),
	}
	cfg := walkCfgBase(ps, true)
	if _, err := cfg.Initialize(context.Background()); err != nil {
		t.Fatalf("a walk tool declaring address (not addresses) must load: %v", err)
	}
}

func TestInitializeWalkNeedsSeedParam(t *testing.T) {
	ps := parameters.Parameters{parameters.NewStringParameter("chain", "c", parameters.WithStringRequired(true))}
	cfg := walkCfgBase(ps, true)
	if _, err := cfg.Initialize(context.Background()); err == nil {
		t.Fatal("a walk tool with neither addresses nor address must be refused")
	}
}

func TestInitializeWalkOnlyParamRejectedOnNonWalk(t *testing.T) {
	ps := parameters.Parameters{
		parameters.NewStringParameter("chain", "c", parameters.WithStringRequired(true)),
		parameters.NewStringParameter("addresses", "a"),
		parameters.NewStringParameter("address", "alias"), // walk-only
	}
	cfg := walkCfgBase(ps, false) // non-walk (counterparties)
	if _, err := cfg.Initialize(context.Background()); err == nil {
		t.Fatal("a non-walk tool must not declare the walk-only parameter address")
	}
}

// --- Gap 4: the edges CSV in the text content -------------------------------

// TestWalkContentCSVInjectionAndShare: the content carries the summary, the edges as
// CSV, and the view_url line only when share is on. The Symbol column is on-chain,
// attacker-controlled data, so a symbol with a comma, a quote and a newline must be
// quoted by encoding/csv and round-trip through a CSV reader as ONE field — it must
// not forge a new row or column.
func TestWalkContentCSVInjectionAndShare(t *testing.T) {
	nasty := "a,b\"c\nd"
	u := 123.45
	res := &walkResult{
		Schema: walkSchema, Chain: "eth",
		Params: walkParamsOut{Address: []string{addr(1)}, Hops: 2},
		Nodes:  []walkNode{{ID: addr(1), Role: "seed"}},
		Edges: []edgeRow{
			{From: addr(1), To: addr(2), Token: "native", Symbol: nasty, Amount: "1", AmountUsd: &u, UsdPricedShare: 1, Transfers: 1, FirstSeen: "2026-07-21T00:00:00Z", LastSeen: "2026-07-22T00:00:00Z"},
			{From: addr(1), To: addr(3), Token: "0xdead", Symbol: "X", Amount: "9", UsdPricedShare: 0, Transfers: 1},
		},
	}

	withShare := res.content(true)
	if !strings.Contains(withShare, "from,to,token,symbol") {
		t.Fatal("content must carry the edges CSV header")
	}
	if !strings.Contains(withShare, "view_url:") {
		t.Error("share=true must include the view_url line")
	}
	if strings.Contains(res.content(false), "view_url:") {
		t.Error("share=false must omit the view_url line")
	}

	// Parse the CSV block (from the header on) and confirm the nasty symbol survives
	// as a single field on its edge's row.
	idx := strings.Index(withShare, "from,to,token,symbol")
	r := csv.NewReader(strings.NewReader(withShare[idx:]))
	r.FieldsPerRecord = -1 // the trailing view_url line is a 1-field record
	recs, err := r.ReadAll()
	if err != nil {
		t.Fatalf("the CSV must parse (encoding/csv must have quoted the symbol): %v", err)
	}
	found := false
	for _, rec := range recs {
		if len(rec) >= 4 && rec[0] == addr(1) && rec[1] == addr(2) {
			found = true
			if rec[3] != nasty {
				t.Errorf("symbol field = %q, want the nasty symbol verbatim (quoted, one field)", rec[3])
			}
		}
	}
	if !found {
		t.Error("the nasty-symbol edge row was not found in the parsed CSV")
	}
}

// --- Gap 5: the three isError codes -----------------------------------------

func TestCodedWalkErr(t *testing.T) {
	e := codedWalkErr("budget_exceeded", "x=%d", 5)
	if e.Category() != util.CategoryAgent {
		t.Errorf("code category = %s, want %s (an isError result, not a protocol error)", e.Category(), util.CategoryAgent)
	}
	if !strings.HasPrefix(e.Error(), "budget_exceeded: ") {
		t.Errorf("message %q must start with the machine code", e.Error())
	}
}

func TestWalkPrecheckCodes(t *testing.T) {
	tl := testTool()
	cases := []struct {
		name, code string
		p          map[string]any
	}{
		{"empty chain", "chain_mismatch", map[string]any{"address": addrA}},
		{"unknown chain", "chain_mismatch", map[string]any{"chain": "dogecoin", "address": addrA}},
		{"unconfigured chain", "chain_mismatch", map[string]any{"chain": "bsc", "address": addrA}},
		{"seed valid on another chain", "chain_mismatch", map[string]any{"chain": "ethereum", "address": tronA}},
		{"garbage seed", "bad_address", map[string]any{"chain": "ethereum", "address": "garbage"}},
		{"empty seed", "bad_address", map[string]any{"chain": "ethereum", "address": ""}},
	}
	for _, c := range cases {
		err := tl.walkPrecheck(c.p)
		if err == nil {
			t.Errorf("%s: want an error", c.name)
			continue
		}
		if !strings.HasPrefix(err.Error(), c.code+": ") {
			t.Errorf("%s: got %q, want code %s", c.name, err.Error(), c.code)
		}
	}
	if err := tl.walkPrecheck(map[string]any{"chain": "ethereum", "address": addrA}); err != nil {
		t.Errorf("a valid EVM seed must pass: %v", err)
	}
	if err := tl.walkPrecheck(map[string]any{"chain": "ethereum", "addresses": addrA + "," + addrB}); err != nil {
		t.Errorf("a valid plural seed set must pass: %v", err)
	}
}

func TestBudgetRefusal(t *testing.T) {
	// A budget stop with no edges is a hard refusal (budget_exceeded).
	if !budgetRefusal(&walkResult{budgetStopped: true, Edges: nil}) {
		t.Error("a budget stop with no edges must be a refusal")
	}
	// A budget stop WITH edges is a graceful partial, not a refusal.
	if budgetRefusal(&walkResult{budgetStopped: true, Edges: []edgeRow{{}}}) {
		t.Error("a budget stop with edges must NOT be a refusal (graceful partial)")
	}
	// A clean finish is never a refusal.
	if budgetRefusal(&walkResult{budgetStopped: false, Edges: nil}) {
		t.Error("a clean finish must never be a refusal")
	}
}

// TestWalkBudgetStopNoEdges: the seed's own fetch runs out of time, so the walk
// gathers no edges — the engine records a budget stop (not from the ledger, which a
// thrown overflow never increments), and budgetRefusal turns it into budget_exceeded.
func TestWalkBudgetStopNoEdges(t *testing.T) {
	S := addr(1)
	mock := &mockFetcher{
		out:   map[string][]edgeRow{S: {edge(S, addr(2), 10)}},
		errOn: map[string]error{S: chainexec.ErrNoTime},
	}
	in := baseInput(t, "ethereum", []string{S}, 2, mock)
	res, err := runWalkGraph(context.Background(), in)
	if err != nil {
		t.Fatalf("a budget stop must not be an error: %v", err)
	}
	if !res.Partial || !res.budgetStopped || len(res.Edges) != 0 {
		t.Fatalf("partial=%v budgetStopped=%v edges=%d, want partial / budget-stopped / 0 edges", res.Partial, res.budgetStopped, len(res.Edges))
	}
	if !budgetRefusal(res) {
		t.Error("no edges after a budget stop must be a budget_exceeded refusal")
	}
}

// --- Gap 1: the hub decision + chunk-cap bisect through the production execFetcher ---

// fakeFlowSource is a chainexec.HTTPSource that records every statement posted and
// answers it from respond. A success answer carries no statistics block, so rows_read
// stays 0 and the remaining call budget does not shrink between statements. readCost
// models a per-source scan cost: a statement whose sources sum to more than its
// max_rows_to_read is refused BEFORE it starts (Code 158), as the server does — the
// same pattern as riskreport/report_test.go's cost(). stopOn forces a non-158 stop
// error (stopCode, default 159 TIMEOUT; 241 MEMORY) for a source regardless of rows, to
// prove isRowsOverflow is narrow — a 159/241 is a walk stop, never a chunk-cap bisect.
type fakeFlowSource struct {
	stmts    []string
	respond  func(stmt string) []map[string]any
	readCost map[string]int64 // per-source literal -> scan rows
	stopOn   map[string]bool  // per-source literal -> force stopCode (a non-158 stop)
	stopCode int              // the CH error code for stopOn (0 -> 159 TIMEOUT)
}

var fgMaxRowsRe = regexp.MustCompile(`max_rows_to_read = ([0-9]+)`)

func (f *fakeFlowSource) HttpBaseURL() string                   { return "http://gw.test:8123" }
func (f *fakeFlowSource) HttpDefaultHeaders() map[string]string { return map[string]string{} }
func (f *fakeFlowSource) HttpQueryParams() map[string]string    { return map[string]string{} }
func (f *fakeFlowSource) BitqueryLegacyBillingPaths() string    { return "" }
func (f *fakeFlowSource) RunRequest(ctx context.Context, req *http.Request) (any, error) {
	body, _ := io.ReadAll(req.Body)
	s := string(body)
	f.stmts = append(f.stmts, s)
	// A forced non-158 stop (159 TIMEOUT / 241 MEMORY): it already ran, so it is a walk
	// stop — NOT a chunk-cap bisect — even when the per-chunk budget was binding.
	for a := range f.stopOn {
		if strings.Contains(s, a) {
			code, name, msg := 159, "TIMEOUT_EXCEEDED", "Timeout exceeded: elapsed 61 s."
			if f.stopCode != 0 {
				code = f.stopCode
			}
			if code == 241 {
				name, msg = "MEMORY_LIMIT_EXCEEDED", "Memory limit (total) exceeded"
			}
			return nil, util.BitqueryDatabaseErrorResponse(ctx, "fake", 500,
				[]byte(fmt.Sprintf("Code: %d. DB::Exception: %s. (%s) (version 26.8.7.19)", code, msg, name)))
		}
	}
	// A rows-overflow refusal (Code 158) when the summed scan cost of the statement's
	// sources exceeds its max_rows_to_read — refused before it starts, reading 0 rows.
	if f.readCost != nil {
		var c int64
		for a, n := range f.readCost {
			if strings.Contains(s, a) {
				c += n
			}
		}
		if m := fgMaxRowsRe.FindStringSubmatch(s); m != nil && c > 0 {
			if limit, _ := strconv.ParseInt(m[1], 10, 64); c > limit {
				return nil, util.BitqueryDatabaseErrorResponse(ctx, "fake", 500,
					[]byte(fmt.Sprintf("Code: 158. DB::Exception: Limit for rows (controlled by 'max_rows_to_read' setting) exceeded, max rows: %d, current rows: %d. (TOO_MANY_ROWS) (version 26.8.7.19)", limit, c)))
			}
		}
	}
	rows := f.respond(s)
	data := make([]any, len(rows))
	for i, r := range rows {
		data[i] = r
	}
	return map[string]any{"data": data}, nil
}

type fakeFlowLabels struct{}

func (fakeFlowLabels) RunSQL(_ context.Context, _ string, _ parameters.ParamValues) (any, error) {
	return []any{}, nil
}

func prodWalkTool(src *fakeFlowSource, edge string) Tool {
	return prodWalkToolCfg(src, edge, Config{})
}

// prodWalkToolCfg builds a walk Tool over a fake source with the given walk knobs
// (WalkChunkRowBudget / WalkMaxRefusals), so a test can make the per-chunk budget bind
// (a small WalkChunkRowBudget) or the call budget bind (a small max_cost param).
func prodWalkToolCfg(src *fakeFlowSource, edge string, wcfg Config) Tool {
	et := template.Must(template.New("e").Option("missingkey=error").Parse(edge))
	lm, _ := compileLabelMethod(nil, nil, nil, nil, nil)
	return Tool{
		stmts:       map[string]*template.Template{"ethereum": et},
		chains:      map[string]chainexec.HTTPSource{"ethereum": src},
		labels:      fakeFlowLabels{},
		budget:      Budget{}.withDefaults(),
		lm:          lm,
		labelChains: map[string][]string{"ethereum": {"ethereum"}},
		noCoverage:  map[string]bool{},
		walk:        true,
		walkCfg:     resolveWalkConfig(wcfg),
	}
}

// edgeDocRow is one flow_edges document row from -> to, with the source's per-source
// Counterparties count (the same value on every row of a source).
func edgeDocRow(from, to string, usd float64, counterparties int) map[string]any {
	return map[string]any{
		"From": from, "To": to, "Token": "native", "Symbol": "ETH",
		"Amount": "1", "Amount_Usd": usd, "Usd_Priced_Share": 1.0,
		"Transfers": "1", "First_Seen": "2026-07-21 00:00:00", "Last_Seen": "2026-07-22 00:00:00",
		"Counterparties": counterparties,
	}
}

const edgeTmplT = "SELECT 'EDGE' /*per_node={{.TopK}}*/ FROM transfers WHERE Source IN {{.SetLiteral}}"

func hexOf(a string) string { return strings.TrimPrefix(a, "0x") }

// TestWalkProdHubPostRead proves the hub decision fires in PRODUCTION (through
// execFetcher and a fake HTTP source) from the edge statement's Counterparties column:
// a hop-1 hub is read, its count marks it a hub, and its edges are discarded. It also
// checks per_node reaches the rendered statement and max_cost becomes max_rows_to_read.
func TestWalkProdHubPostRead(t *testing.T) {
	S, H, N, M := addr(1), addr(2), addr(3), addr(4)
	src := &fakeFlowSource{}
	src.respond = func(s string) []map[string]any {
		var rows []map[string]any
		if strings.Contains(s, hexOf(S)) {
			rows = append(rows, edgeDocRow(S, H, 100, 2), edgeDocRow(S, N, 50, 2))
		}
		if strings.Contains(s, hexOf(H)) {
			rows = append(rows, edgeDocRow(H, M, 10, 500)) // H has 500 counterparties -> a hub
		}
		if strings.Contains(s, hexOf(N)) {
			rows = append(rows, edgeDocRow(N, M, 10, 5)) // N is not a hub
		}
		return rows
	}
	pt := prodWalkTool(src, edgeTmplT)
	res, err := pt.runWalk(context.Background(), map[string]any{
		"chain": "ethereum", "address": S, "hops": int64(2),
		"per_node": int64(7), "max_cost": int64(12345678),
	})
	if err != nil {
		t.Fatalf("runWalk: %v", err)
	}

	sawPerNode, sawMaxRows := false, false
	for _, s := range src.stmts {
		if strings.Contains(s, "per_node=7") {
			sawPerNode = true
		}
		if strings.Contains(s, "max_rows_to_read = 12345678") {
			sawMaxRows = true
		}
	}
	if !sawPerNode {
		t.Error("per_node did not reach the flow_edges statement (TopK)")
	}
	if !sawMaxRows {
		t.Error("max_cost did not become max_rows_to_read in SETTINGS")
	}
	sr, ok := res.(tools.StructuredResult)
	if !ok {
		t.Fatalf("result type %T, want StructuredResult", res)
	}
	b, _ := json.Marshal(sr.Structured)
	js := string(b)
	if !strings.Contains(js, `"stop_reason":"hub"`) {
		t.Error("the structured result must mark the hub node stop_reason hub")
	}
	// The hub H is read (post-read decision), but its own target edge H->M is discarded;
	// M is still discovered via the non-hub N->M.
	if !strings.Contains(js, hexOf(H)) {
		t.Error("the hub H must appear as a node (it was read, then its edges discarded)")
	}
}

// TestWalkProdBisectChunkCap proves the chunk-cap bisect in PRODUCTION: with a small
// per-chunk row budget, a hop-1 frontier whose combined scan exceeds it is refused
// before start (Code 158) and bisected; each sender, read alone, fits and expands.
func TestWalkProdBisectChunkCap(t *testing.T) {
	S, A, B, TA, TB := addr(1), addr(2), addr(3), addr(11), addr(12)
	src := &fakeFlowSource{
		readCost: map[string]int64{
			hexOf(S): 100, hexOf(A): 800, hexOf(B): 800, // {A,B}=1600 > 1000; each alone < 1000
		},
	}
	src.respond = func(s string) []map[string]any {
		var rows []map[string]any
		if strings.Contains(s, hexOf(S)) {
			rows = append(rows, edgeDocRow(S, A, 100, 2), edgeDocRow(S, B, 90, 2))
		}
		if strings.Contains(s, hexOf(A)) {
			rows = append(rows, edgeDocRow(A, TA, 10, 1))
		}
		if strings.Contains(s, hexOf(B)) {
			rows = append(rows, edgeDocRow(B, TB, 10, 1))
		}
		return rows
	}
	// A small per-chunk budget; a large call budget so the chunk budget is the binding limit.
	pt := prodWalkToolCfg(src, edgeTmplT, Config{WalkChunkRowBudget: 1000})
	res, err := pt.runWalk(context.Background(), map[string]any{
		"chain": "ethereum", "address": S, "hops": int64(2), "max_cost": int64(50_000_000),
	})
	if err != nil {
		t.Fatalf("runWalk: %v", err)
	}
	sr := res.(tools.StructuredResult)
	b, _ := json.Marshal(sr.Structured)
	js := string(b)
	if strings.Contains(js, `"partial":true`) {
		t.Errorf("the walk must not be partial after a successful bisect: %s", js)
	}
	for _, target := range []string{TA, TB} {
		if !strings.Contains(js, hexOf(target)) {
			t.Errorf("target %s must be discovered after the bisect; graph=%s", short(target), js)
		}
	}
	// A statement carrying BOTH A and B was sent (the refused chunk), and separate
	// single-source A / B statements (the bisected halves).
	var sawBoth, sawAAlone, sawBAlone bool
	for _, s := range src.stmts {
		hasA, hasB := strings.Contains(s, hexOf(A)), strings.Contains(s, hexOf(B))
		if hasA && hasB {
			sawBoth = true
		}
		if hasA && !hasB {
			sawAAlone = true
		}
		if hasB && !hasA {
			sawBAlone = true
		}
	}
	if !sawBoth || !sawAAlone || !sawBAlone {
		t.Errorf("expected a {A,B} chunk then split A / B reads; sawBoth=%v A=%v B=%v", sawBoth, sawAAlone, sawBAlone)
	}
}

// TestWalkProdCallBudgetStops proves the discrimination: when the CALL budget is the
// binding limit (max_cost below the chunk budget), a Code-158 refusal is a genuine
// budget stop — NOT a bisect. The seed's read exceeds a tiny max_cost and the walk
// returns budget_exceeded without splitting.
func TestWalkProdCallBudgetStops(t *testing.T) {
	S := addr(1)
	src := &fakeFlowSource{readCost: map[string]int64{hexOf(S): 1000}}
	src.respond = func(s string) []map[string]any {
		if strings.Contains(s, hexOf(S)) {
			return []map[string]any{edgeDocRow(S, addr(2), 100, 1)}
		}
		return nil
	}
	// A tiny call budget, well below the default chunk budget, so the call budget binds.
	pt := prodWalkTool(src, edgeTmplT)
	_, err := pt.runWalk(context.Background(), map[string]any{
		"chain": "ethereum", "address": S, "hops": int64(1), "max_cost": int64(500),
	})
	if err == nil {
		t.Fatal("a call-budget refusal with no edges must be a coded error")
	}
	if !strings.HasPrefix(err.Error(), "budget_exceeded: ") {
		t.Errorf("err %q must carry the budget_exceeded code (a call-budget 158 is a stop, not a bisect)", err.Error())
	}
	// Exactly one flow_edges statement ran: the 158 was NOT bisected.
	if len(src.stmts) != 1 {
		t.Errorf("flow_edges statements = %d, want 1 (a call-budget stop is not bisected); stmts=%v", len(src.stmts), src.stmts)
	}
}

// TestWalkProdStopCodesNotBisected proves isRowsOverflow is narrow: a timeout (Code 159)
// or a memory kill (Code 241) returned even when the per-chunk budget was the binding
// limit is NOT tagged errChunkTooBig — it is a walk stop, never a bisect. It uses TWO
// hop-1 senders so a mis-tag is visible: a correct stop reads the {A,B} chunk once and
// halts (two flow_edges statements, {S} then {A,B}); a mis-tag would bisect it into {A}
// and {B} (four statements).
func TestWalkProdStopCodesNotBisected(t *testing.T) {
	for _, code := range []int{159, 241} {
		t.Run(fmt.Sprintf("code-%d", code), func(t *testing.T) {
			S, A, B := addr(1), addr(2), addr(3)
			src := &fakeFlowSource{
				readCost: map[string]int64{hexOf(S): 100},
				stopOn:   map[string]bool{hexOf(A): true, hexOf(B): true},
				stopCode: code,
			}
			src.respond = func(s string) []map[string]any {
				if strings.Contains(s, hexOf(S)) {
					return []map[string]any{edgeDocRow(S, A, 100, 1), edgeDocRow(S, B, 90, 1)}
				}
				return nil
			}
			// A small per-chunk budget so the {A,B} read is chunk-binding — yet a 159/241
			// there must STOP the walk, not be mis-tagged as a chunk-cap bisect.
			pt := prodWalkToolCfg(src, edgeTmplT, Config{WalkChunkRowBudget: 1000})
			res, err := pt.runWalk(context.Background(), map[string]any{
				"chain": "ethereum", "address": S, "hops": int64(2), "max_cost": int64(50_000_000),
			})
			if err != nil {
				t.Fatalf("a code-%d stop must not be a hard error (S->A,S->B were gathered): %v", code, err)
			}
			sr := res.(tools.StructuredResult)
			b, _ := json.Marshal(sr.Structured)
			if !strings.Contains(string(b), `"partial":true`) {
				t.Errorf("code %d must mark the walk partial; graph=%s", code, string(b))
			}
			// The {A,B} read stopped the walk and was NOT bisected: exactly two flow_edges
			// statements ran ({S}, then {A,B}); a mis-tag would split into {A},{B} (four).
			if len(src.stmts) != 2 {
				t.Errorf("flow_edges statements = %d, want 2 ({S},{A,B}); a code-%d stop must not bisect; stmts=%v", len(src.stmts), code, src.stmts)
			}
		})
	}
}

// TestWalkProdLoneSeedCallBudget proves design point 5 in PRODUCTION: a lone seed the
// per-chunk budget refuses is re-read with max_rows_to_read = the REMAINING CALL BUDGET
// (not the chunk budget), so a heavy seed is still read rather than left empty.
func TestWalkProdLoneSeedCallBudget(t *testing.T) {
	S, A := addr(1), addr(2)
	src := &fakeFlowSource{readCost: map[string]int64{hexOf(S): 5000}} // the seed costs 5000
	src.respond = func(s string) []map[string]any {
		if strings.Contains(s, hexOf(S)) {
			return []map[string]any{edgeDocRow(S, A, 100, 1)}
		}
		return nil
	}
	// chunk budget 1000 < the seed's 5000, so the capped read refuses (158); the uncapped
	// re-read is bounded by max_cost (50M) and fits.
	pt := prodWalkToolCfg(src, edgeTmplT, Config{WalkChunkRowBudget: 1000})
	res, err := pt.runWalk(context.Background(), map[string]any{
		"chain": "ethereum", "address": S, "hops": int64(1), "max_cost": int64(50_000_000),
	})
	if err != nil {
		t.Fatalf("a lone seed over the chunk budget must still be read: %v", err)
	}
	// Two statements: the capped refusal (max_rows_to_read = 1000) then the uncapped
	// re-read (max_rows_to_read = 50000000 = the remaining call budget).
	var sawCapped, sawCallBudget bool
	for _, s := range src.stmts {
		if strings.Contains(s, "max_rows_to_read = 1000") {
			sawCapped = true
		}
		if strings.Contains(s, "max_rows_to_read = 50000000") {
			sawCallBudget = true
		}
	}
	if !sawCapped {
		t.Errorf("expected the chunk-capped seed read (max_rows_to_read = 1000); stmts=%v", src.stmts)
	}
	if !sawCallBudget {
		t.Errorf("expected the lone-seed re-read at the call budget (max_rows_to_read = 50000000); stmts=%v", src.stmts)
	}
	sr := res.(tools.StructuredResult)
	b, _ := json.Marshal(sr.Structured)
	if !strings.Contains(string(b), hexOf(A)) {
		t.Errorf("the heavy seed's edge/target must be returned; graph=%s", string(b))
	}
}

// --- F9: the two added walk codes, through the real parse -------------------

// TestWalkAfterBeforeCode: after_time after before_time comes from the SHARED t.parse
// (uncoded); runWalk wraps it with the bad_request code without touching t.parse. The
// refusal is before any fetch, so no statement runs.
func TestWalkAfterBeforeCode(t *testing.T) {
	S := addr(1)
	src := &fakeFlowSource{}
	src.respond = func(string) []map[string]any { return nil }
	pt := prodWalkTool(src, edgeTmplT)
	_, err := pt.runWalk(context.Background(), map[string]any{
		"chain": "ethereum", "address": S, "after_time": "2026-08-01", "before_time": "2026-07-01",
	})
	if err == nil {
		t.Fatal("after_time after before_time must be an error")
	}
	if !strings.HasPrefix(err.Error(), "bad_request: ") {
		t.Errorf("window-order error %q must carry the bad_request code", err.Error())
	}
	if len(src.stmts) != 0 {
		t.Errorf("a window-order refusal must run no statements, got %d", len(src.stmts))
	}
}

// TestWalkUnknownStopGroupCode: an unknown stop_at group is a coded bad_request that
// names the sorted allowed groups, before any fetch.
func TestWalkUnknownStopGroupCode(t *testing.T) {
	S := addr(1)
	src := &fakeFlowSource{}
	src.respond = func(string) []map[string]any { return nil }
	pt := prodWalkTool(src, edgeTmplT)
	_, err := pt.runWalk(context.Background(), map[string]any{
		"chain": "ethereum", "address": S, "stop_at": "bogus",
	})
	if err == nil {
		t.Fatal("an unknown stop group must be an error")
	}
	if !strings.HasPrefix(err.Error(), "bad_request: ") {
		t.Errorf("unknown-stop-group error %q must carry the bad_request code", err.Error())
	}
	if !strings.Contains(err.Error(), "unknown group") {
		t.Errorf("message %q must name the unknown-group condition", err.Error())
	}
	// The allowed list is the sorted default group names.
	if !strings.Contains(err.Error(), "allowed: bridge, exchange, mixer") {
		t.Errorf("message %q must list the sorted allowed groups", err.Error())
	}
	if len(src.stmts) != 0 {
		t.Errorf("an unknown-stop-group refusal must run no statements, got %d", len(src.stmts))
	}
}

// TestWalkPrecheckGrammar: the article (an/a) and the singular/plural seed(s)/address(es).
func TestWalkPrecheckGrammar(t *testing.T) {
	tl := testTool()
	if err := tl.walkPrecheck(map[string]any{"chain": "ethereum", "address": tronA}); err == nil {
		t.Fatal("a tron seed on ethereum must mismatch")
	} else if !strings.Contains(err.Error(), "is not an ethereum address") {
		t.Errorf("mismatch message %q must use the article 'an' before ethereum", err.Error())
	}
	if err := tl.walkPrecheck(map[string]any{"chain": "ethereum", "address": "garbage"}); err == nil {
		t.Fatal("a garbage seed must be rejected")
	} else if !strings.Contains(err.Error(), "seed garbage is not a valid ethereum address") {
		t.Errorf("single-garbage message %q must be singular", err.Error())
	}
	if err := tl.walkPrecheck(map[string]any{"chain": "ethereum", "addresses": "garbage1,garbage2"}); err == nil {
		t.Fatal("garbage seeds must be rejected")
	} else if !strings.Contains(err.Error(), "seeds garbage1, garbage2 are not valid ethereum addresses") {
		t.Errorf("plural-garbage message %q must be plural (seeds ... are not valid ... addresses)", err.Error())
	}
}

// --- F13: include_zero_value reaches the rendered flow_edges statement ------

// TestWalkIncludeZeroReachesStatement proves the hub-signal filter (include_zero_value)
// reaches the rendered statement. Its EFFECT on the per-source Counterparties count is
// a property of the cfg SQL, not of this engine, so it is NOT asserted here (that needs
// a live check against the cluster): the offline, honest claim is only that the
// parameter is wired through to the rendered statement.
func TestWalkIncludeZeroReachesStatement(t *testing.T) {
	S, A := addr(1), addr(2)
	src := &fakeFlowSource{}
	src.respond = func(s string) []map[string]any {
		if strings.Contains(s, hexOf(S)) {
			return []map[string]any{edgeDocRow(S, A, 100, 1)}
		}
		return nil
	}
	tmpl := "SELECT 'EDGE' /*izv={{.IncludeZeroValue}} per_node={{.TopK}}*/ FROM transfers WHERE Source IN {{.SetLiteral}}"
	pt := prodWalkTool(src, tmpl)
	if _, err := pt.runWalk(context.Background(), map[string]any{
		"chain": "ethereum", "address": S, "hops": int64(1), "include_zero_value": int64(1),
	}); err != nil {
		t.Fatalf("runWalk: %v", err)
	}
	var saw bool
	for _, s := range src.stmts {
		if strings.Contains(s, "izv=1") {
			saw = true
		}
	}
	if !saw {
		t.Errorf("include_zero_value=1 did not reach the flow_edges statement; stmts=%v", src.stmts)
	}
}

// --- F6/F7 end-to-end through the production runWalk path -------------------

// TestWalkRunWalkParamsEcho exercises the EFFECTIVE-params echo through runWalk (not a
// hand-built result): the Params text and structuredContent carry the clamped hops,
// the effective per_node, the window END, stop_at and the token. It then re-sends the
// echoed params as the app's Expand would, to show the round-trip.
func TestWalkRunWalkParamsEcho(t *testing.T) {
	S, A := addr(1), addr(2)
	tokHex := "dac17f958d2ee523a2206206994597c13d831ec7"
	src := &fakeFlowSource{}
	src.respond = func(s string) []map[string]any {
		if strings.Contains(s, hexOf(S)) {
			return []map[string]any{edgeDocRow(S, A, 100, 1)}
		}
		return nil
	}
	pt := prodWalkTool(src, edgeTmplT)
	res, err := pt.runWalk(context.Background(), map[string]any{
		"chain": "ethereum", "address": S,
		"hops": int64(5), "per_node": int64(7), // hops clamps 5 -> maxHops (3)
		"stop_at": "exchange", "token": "0x" + tokHex,
		"after_time": "2026-07-01", "before_time": "2026-07-21",
		"include_zero_value": int64(1),
	})
	if err != nil {
		t.Fatalf("runWalk: %v", err)
	}
	sr := res.(tools.StructuredResult)
	text := sr.Content[0].(tools.TextResult).Text
	for _, want := range []string{
		"hops=3 (clamped from 5)",
		"per_node=7",
		"window 2026-07-01T00:00:00Z..2026-07-21T00:00:00Z",
		"stop_at=exchange",
		"token=" + tokHex,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("runWalk text missing %q\n%s", want, text)
		}
	}
	params := sr.Structured["params"].(map[string]any)
	for _, k := range []string{"before_time", "per_node", "stop_at", "token"} {
		if _, ok := params[k]; !ok {
			t.Errorf("structured params missing %q", k)
		}
	}
	if params["token"] != tokHex {
		t.Errorf("structured params token = %v, want the effective no-0x key %s", params["token"], tokHex)
	}

	// Expand re-sends every echoed param except address and hops. WITHOUT a token it
	// must succeed — which also proves the RFC3339 before_time echo parses back.
	expand := map[string]any{
		"chain": "ethereum", "address": A,
		"after_time":         params["after_time"],
		"before_time":        params["before_time"],
		"per_node":           params["per_node"],
		"stop_at":            params["stop_at"],
		"min_usd":            params["min_usd"],
		"include_internal":   params["include_internal"],
		"include_zero_value": int64(1),
	}
	if _, e := pt.runWalk(context.Background(), expand); e != nil {
		t.Errorf("Expand re-send (no token) must succeed (the RFC3339 before_time must parse): %v", e)
	}
	// WITH the echoed token (no-0x), evmCodec.tokenKey requires a 0x prefix, so this
	// Expand is REJECTED. This documents pre-existing codec behaviour that the token echo
	// surfaces (the brief keeps Token = req.token). TRIPWIRE: if this begins to pass, the
	// codec round-trip changed — revisit the token echo and update this test.
	expand["token"] = params["token"]
	if _, e := pt.runWalk(context.Background(), expand); e == nil {
		t.Error("Expand re-send WITH the echoed no-0x token is expected to be rejected by evmCodec.tokenKey (0x required)")
	}
}

// TestWalkRunWalkEmptyHint: a production walk that returns no edges carries the
// no-match hint with the effective include_zero_value, through runWalk.
func TestWalkRunWalkEmptyHint(t *testing.T) {
	S := addr(1)
	src := &fakeFlowSource{}
	src.respond = func(string) []map[string]any { return nil } // no edges
	pt := prodWalkTool(src, edgeTmplT)
	res, err := pt.runWalk(context.Background(), map[string]any{
		"chain": "ethereum", "address": S, "hops": int64(1), "include_zero_value": int64(1),
	})
	if err != nil {
		t.Fatalf("runWalk: %v", err)
	}
	text := res.(tools.StructuredResult).Content[0].(tools.TextResult).Text
	if !strings.Contains(text, "No outgoing transfers matched the filters") {
		t.Errorf("an empty production walk must carry the hint:\n%s", text)
	}
	if !strings.Contains(text, "include_zero_value=1") {
		t.Errorf("the empty-graph hint must echo the effective include_zero_value:\n%s", text)
	}
}
