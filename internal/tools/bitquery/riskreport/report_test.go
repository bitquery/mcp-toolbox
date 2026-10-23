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

package riskreport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math/big"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/googleapis/mcp-toolbox/internal/sources"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

// fakeChain answers the Tron statements from canned rows and records every
// statement it is sent. The multi-hop search's statements are answered from
// transfers: each one is a receipt of its receiver and a send of its sender.
type fakeChain struct {
	assets, edges, other []map[string]any
	statements           []string
	urls                 []string
	headers              []string // X-Bitquery-Table of each statement
	// price is what the price-store statement returns for the report asset;
	// "" stands for a NULL (no price).
	price string

	// lateRaw is a transfer to the target that lands while the report runs,
	// after the assets statement and before the counterparty statement, at
	// lateTime: a counterparty statement sees it unless it reads only before
	// a pin at or before lateTime. nil: no such transfer.
	lateRaw  *big.Int
	lateTime time.Time

	transfers []fakeTransfer
	// readCost is the rows a history read of the address costs on each table.
	// A statement whose addresses cost more than its max_rows_to_read is
	// refused before it starts (Code 158), as the server does.
	readCost  map[string]int64
	granules  map[string]int  // EXPLAIN: primary-key granules (else 1)
	noGranule map[string]bool // EXPLAIN: a text without a granule count
}

// explainRows answers EXPLAIN indexes = 1 of one address in the server's text
// layout, one line per row.
func (f *fakeChain) explainRows(s string) []map[string]any {
	addr := ""
	for _, a := range f.addresses() {
		if mentioned(s, a) {
			addr = a
		}
	}
	g, ok := f.granules[addr]
	if !ok {
		g = 1
	}
	lines := []string{"Expression ((Project names + Projection))", "  Expression", "    ReadFromMergeTree (tron_api.transfers)",
		"    Indexes:", "      MinMax", "        Condition: true", "        Parts: 120/120", "        Granules: 3000000/3000000",
		"      Partition", "        Condition: true", "        Parts: 120/120", "        Granules: 3000000/3000000",
		"      PrimaryKey", "        Keys:", "          Transfer_Sender", "        Condition: (Transfer_Sender in [x, x])",
		"        Parts: 40/120", fmt.Sprintf("        Granules: %d/3000000", g)}
	if f.noGranule[addr] {
		lines = lines[:len(lines)-1]
	}
	rows := make([]map[string]any, len(lines))
	for i, l := range lines {
		rows[i] = map[string]any{"explain": l}
	}
	return rows
}

type fakeTransfer struct {
	from, to string
	raw      string
	block    uint64
	time     string
	tx       string
}

// mentioned is whether a statement names an address as a literal.
func mentioned(s, addr string) bool { return strings.Contains(s, "'"+addr+"'") }

func (f *fakeChain) addresses() []string {
	seen := map[string]bool{}
	var out []string
	add := func(a string) {
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	for _, t := range f.transfers {
		add(t.from)
		add(t.to)
	}
	for a := range f.readCost {
		add(a)
	}
	return out
}

var maxRowsRe = regexp.MustCompile(`max_rows_to_read = ([0-9]+)`)

// cost is what a history statement reads, and whether it is refused for it.
func (f *fakeChain) cost(ctx context.Context, s string) (int64, error) {
	var c int64
	for a, n := range f.readCost {
		if mentioned(s, a) {
			c += n
		}
	}
	if m := maxRowsRe.FindStringSubmatch(s); m != nil {
		if limit, _ := strconv.ParseInt(m[1], 10, 64); c > limit {
			return c, util.BitqueryDatabaseErrorResponse(ctx, "fake", 500, []byte(fmt.Sprintf("Code: 158. DB::Exception: Limit for rows (controlled by 'max_rows_to_read' setting) exceeded, max rows: %d, current rows: %d. (TOO_MANY_ROWS) (version 26.8.7.19 (official build))", limit, c)))
		}
	}
	return c, nil
}

func (f *fakeChain) historyRows(s string) []map[string]any {
	in := strings.Contains(s, "FROM "+tronReceiver)
	var rows []map[string]any
	for _, t := range f.transfers {
		self, peer := t.from, t.to
		if in {
			self, peer = t.to, t.from
		}
		if !mentioned(s, self) {
			continue
		}
		// 64-bit integers arrive quoted, as ClickHouse prints them.
		rows = append(rows, map[string]any{"Address": self, "Peer": peer, "Raw": t.raw,
			"Block_Number": fmt.Sprint(t.block), "Transaction_Index": float64(0), "Call_Index": float64(0), "Log_Index": float64(0),
			"Transfer_Index": float64(0), "Time": t.time, "Tx": t.tx})
	}
	return rows
}

// pinRe finds the report's upper bound in a statement.
var pinRe = regexp.MustCompile(`Block_Time < toDateTime\('([0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2})', 'UTC'\)`)

// seesLate is whether a statement reads up to a moment after t: it carries no
// pin, or a pin later than t.
func seesLate(s string, t time.Time) bool {
	m := pinRe.FindStringSubmatch(s)
	if m == nil {
		return true
	}
	pin, err := time.Parse(time.DateTime, m[1])
	return err != nil || pin.After(t)
}

func (f *fakeChain) SourceType() string                    { return "http" }
func (f *fakeChain) ToConfig() sources.SourceConfig        { return nil }
func (f *fakeChain) HttpBaseURL() string                   { return "http://gateway.test:8123" }
func (f *fakeChain) HttpDefaultHeaders() map[string]string { return map[string]string{"X-Test": "1"} }

// HttpQueryParams turns wait_end_of_query off: the report must set it itself.
func (f *fakeChain) HttpQueryParams() map[string]string {
	return map[string]string{"default_format": "JSONEachRow", "wait_end_of_query": "0"}
}
func (f *fakeChain) BitqueryLegacyBillingPaths() string { return "" }
func (f *fakeChain) RunRequest(ctx context.Context, req *http.Request) (any, error) {
	body, _ := io.ReadAll(req.Body)
	s := string(body)
	f.statements = append(f.statements, s)
	f.urls = append(f.urls, req.URL.String())
	f.headers = append(f.headers, req.Header.Get("X-Bitquery-Table"))
	var rows []map[string]any
	read := int64(1000)
	switch {
	case strings.Contains(s, "dictGetOrNull"):
		// The price statement returns exactly one row; "" is a NULL price.
		var p any
		if f.price != "" {
			p = f.price
		}
		rows = []map[string]any{{"Price": p}}
	case strings.Contains(s, "GROUP BY Native, SC, AssetId"):
		rows = f.assets
	case strings.Contains(s, "AS Dir_Counterparties"):
		late := f.lateRaw != nil && seesLate(s, f.lateTime)
		for _, r := range f.edges {
			r["R"] = float64(1)
			if late && r["Dir"] == "in" {
				// The late receipt is in this statement's totals.
				c := maps.Clone(r)
				c["Dir_Raw"] = new(big.Int).Add(asBig(r["Dir_Raw"]), f.lateRaw).String()
				r = c
			}
			rows = append(rows, r)
		}
		for _, r := range f.other {
			r["R"] = float64(0)
			rows = append(rows, r)
		}
	case strings.HasPrefix(s, "EXPLAIN"):
		rows = f.explainRows(s)
	case strings.Contains(s, "AS Peer"):
		c, err := f.cost(ctx, s)
		if err != nil {
			return nil, err
		}
		if c > 0 {
			read = c
		}
		rows = f.historyRows(s)
	}
	data := make([]any, len(rows))
	for i, r := range rows {
		data[i] = r
	}
	return map[string]any{"data": data, "rows": float64(len(rows)),
		"statistics": map[string]any{"rows_read": float64(read), "bytes_read": float64(20000), "elapsed": 0.01}}, nil
}

// fakeLabels answers label statements from a map of address -> rows.
type fakeLabels struct {
	rows       map[string][]map[string]any
	statements []string
}

func (f *fakeLabels) SourceType() string             { return "clickhouse" }
func (f *fakeLabels) ToConfig() sources.SourceConfig { return nil }
func (f *fakeLabels) RunSQL(_ context.Context, s string, _ parameters.ParamValues) (any, error) {
	f.statements = append(f.statements, s)
	var out []any
	for addr, rows := range f.rows {
		if strings.Contains(s, "'"+addr+"'") {
			for _, r := range rows {
				r["address"] = addr
				out = append(out, r)
			}
		}
	}
	return out, nil
}

func newTestTool(t *testing.T, chain *fakeChain, labels *fakeLabels) Tool {
	t.Helper()
	return newTestToolWith(t, chain, labels, nil)
}

// newTestToolWith builds the tool with a change to its method block.
func newTestToolWith(t *testing.T, chain *fakeChain, labels *fakeLabels, change func(*MethodConfig)) Tool {
	t.Helper()
	mc := loadMethodConfig(t)
	if change != nil {
		change(&mc)
	}
	cfg := Config{
		ConfigBase:   tools.ConfigBase{Name: "address_risk_report", Description: "risk report"},
		Type:         resourceType,
		Source:       "tron-src",
		ChainSources: map[string]string{"tron": "tron-src"},
		LabelsSource: "labels-src",
		Parameters: parameters.Parameters{
			parameters.NewStringParameter("chain", "chain"),
			parameters.NewStringParameter("address", "address"),
			parameters.NewStringParameter("asset", "asset", parameters.WithStringDefault("")),
			parameters.NewStringParameter("direction", "direction", parameters.WithStringDefault("both")),
			parameters.NewStringParameter("view", "view", parameters.WithStringDefault("direct")),
			parameters.NewStringParameter("format", "format", parameters.WithStringDefault("")),
			parameters.NewIntParameter("max_hops", "max_hops", parameters.WithIntRequired(false)),
			parameters.NewFloatParameter("min_amount_usd", "min_amount_usd", parameters.WithFloatRequired(false)),
		},
		MethodConfig: mc,
	}
	ctx := tools.WithSourceLookup(context.Background(), tools.SourceMap{"tron-src": chain, "labels-src": labels})
	tool, err := cfg.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return tool.(Tool)
}

const (
	usdtContract = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
	at5Target    = "TDNbVbjmT4seJMfGUg37Zf3PYj2NvNxodf"
	at5Xinbi     = "TXHX2NUvcgft4zs6MkkUtdNVERPTavHDWN"
	plainSender  = "TUrdaveFV4bnTgLxSDkttBMvKSTt36e1A8"
)

func usdtAssetRow(recv, sent string, nIn, nOut int) map[string]any {
	return map[string]any{"Native": "0", "Contract": usdtContract, "AssetId": "", "Symbol": "USDT", "Decimals": float64(6),
		"Received": recv, "Sent": sent, "Received_All": recv, "Sent_All": sent, "n_in": float64(nIn), "n_out": float64(nOut), "txs": float64(nIn + nOut),
		"First": "2026-08-01 00:00:00", "Last": "2026-09-08 08:00:03", "First_All": "2026-08-01 00:00:00", "Last_All": "2026-09-08 08:00:03",
		"First_In": "2026-08-01 00:00:00"}
}

func edgeRow(dir, addr, raw, rawMin, rawMax string, n int, dirTotals [4]string, tx ...string) map[string]any {
	txs := make([]any, len(tx))
	for i, x := range tx {
		txs[i] = x
	}
	return map[string]any{"Dir": dir, "Counterparty": addr, "n": float64(n), "Raw": raw, "n_min": float64(n), "Raw_Above_Min": rawMin,
		"n_dust": float64(0), "Raw_Max": rawMax, "Raw_Early": "0", "First": "2026-08-30 10:36:03", "Last": "2026-09-08 08:00:03",
		"Tx_Above_Dust": txs, "Tx": txs,
		"Dir_Counterparties": dirTotals[0], "Dir_Transfers": dirTotals[1], "Dir_Raw": dirTotals[2], "Dir_Raw_Above_Min": dirTotals[3]}
}

// decode returns a result's report map. The structural tests read the json
// format (mustParams defaults to it), which is the report object itself; the
// markdown format is a text document, exercised separately by
// TestFormatJSONAndMarkdown.
func decode(t *testing.T, v any) map[string]any {
	t.Helper()
	if items, structured := tools.SplitResult(v); structured != nil {
		return structured
	} else if len(items) == 1 {
		v = items[0]
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// AT5: a direct counterparty on a sanctions list sets the score to 100 and
// raises direct_sanctions with both transactions as evidence.
func TestDirectSanctionsReport(t *testing.T) {
	chain := &fakeChain{
		assets: []map[string]any{usdtAssetRow("89866000000", "2968000000", 2, 1)},
		edges: []map[string]any{
			edgeRow("in", at5Xinbi, "88866000000", "88866000000", "88866000000", 1, [4]string{"2", "2", "89866000000", "89866000000"}, "07e48307f31c1206dad43bc11abb4abb4ab50cf64ade74182ff546cc4d8353bd573e"),
			edgeRow("in", plainSender, "1000000000", "1000000000", "1000000000", 1, [4]string{"2", "2", "89866000000", "89866000000"}, "aa"),
			edgeRow("out", at5Xinbi, "2968000000", "2968000000", "2968000000", 1, [4]string{"1", "1", "2968000000", "2968000000"}, "50a4da05a81dd30e6a146b140c1cdad98835af2b5c6ad1c92e964b822691d753"),
		},
	}
	labels := &fakeLabels{rows: map[string][]map[string]any{
		at5Xinbi: {
			{"label_type": "sanctioned-entity", "label_value": "sanctioned-xinbi-guarantee", "source": "ofac", "recorded_at": "2026-05-01 00:00:00"},
			{"label_type": "sanctioned-entity", "label_value": "sanctioned-by-uk-xinbi-company-limited", "source": "uk-ofsi", "recorded_at": "2026-09-01 00:00:00"},
			{"label_type": "issuer-blocked-usdt", "label_value": "banned-by-usdt", "source": "workflow:stablecoin-issuer-ban-v1", "recorded_at": "2026-05-01 00:00:00"},
			{"label_type": "risk-flag", "label_value": "risk-flag-xinbi-guarantee", "source": "vendor-x", "recorded_at": "2026-05-01 00:00:00"},
			{"label_type": "contract", "label_value": "contract", "source": "vendor-x", "recorded_at": "2026-05-01 00:00:00"},
		},
	}}
	tool := newTestTool(t, chain, labels)
	res, err := tool.Invoke(context.Background(), chain, mustParams(t, tool, map[string]any{"chain": "tron", "address": at5Target}), "")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := decode(t, res)
	score := out["score"].(map[string]any)
	if score["value"].(float64) != 100 || score["direct_floor"].(float64) != 100 {
		t.Errorf("score: %v", score)
	}
	var sanctions []map[string]any
	for _, f := range out["flags"].([]any) {
		if fm := f.(map[string]any); fm["type"] == "direct_sanctions" {
			sanctions = append(sanctions, fm)
		}
	}
	if len(sanctions) != 2 {
		t.Fatalf("direct_sanctions flags = %d, want 2 (received and sent): %v", len(sanctions), out["flags"])
	}
	var ev []string
	for _, f := range sanctions {
		for _, tx := range f["evidence_txs"].([]any) {
			ev = append(ev, tx.(string))
		}
	}
	for _, want := range []string{"07e48307f31c1206dad43bc11abb4abb4ab50cf64ade74182ff546cc4d8353bd573e", "50a4da05a81dd30e6a146b140c1cdad98835af2b5c6ad1c92e964b822691d753"} {
		if !slices.Contains(ev, want) {
			t.Errorf("evidence lacks %s: %v", want, ev)
		}
	}
	for _, f := range sanctions {
		if e := f["detail"].(map[string]any)["entity"]; e != "xinbi-guarantee" {
			t.Errorf("the flag names %v, the connection must name the same entity", e)
		}
	}
	if ff, _ := score["direct_floor_from"].(map[string]any); ff["counterparty"] != at5Xinbi {
		t.Errorf("direct_floor_from: %v", score["direct_floor_from"])
	}
	conns := out["connections"].([]any)
	if len(conns) != 1 {
		t.Fatalf("connections: %v", conns)
	}
	c := conns[0].(map[string]any)
	if c["entity"] != "xinbi-guarantee" || c["entity_type"] != "Sanctions" || c["received"] != "88866.000000" || c["sent"] != "2968.000000" ||
		c["label_source_class"] != "sanctions-list" {
		t.Errorf("connection: %v", c)
	}
	if os, _ := c["other_sources"].([]any); len(os) != 1 || os[0] != "uk-ofsi" || c["label_source"] != "ofac" {
		t.Errorf("sources: %v / %v (the first preferred list names the entity, whatever was recorded later)", c["label_source"], c["other_sources"])
	}
	if ot, _ := c["other_types"].([]any); len(ot) != 1 || ot[0] != "Illegal Service" {
		t.Errorf("other types: %v", c["other_types"])
	}
	if out["exposure"] != nil {
		t.Errorf("exposure set on a labelled report: %v", out["exposure"])
	}
	params := out["params"].(map[string]any)
	if fs := params["flags_status"].(map[string]any); fs["frozen"] != "not_evaluated" || fs["direct_sanctions"] != "found" {
		t.Errorf("flags_status: %v", fs)
	}
	if out["timing_ms"] == nil {
		t.Errorf("no timing_ms")
	}
	cost := out["cost"].(map[string]any)
	if cost["statements"].(float64) != 3 {
		t.Errorf("statements = %v, want 3 (assets, counterparties, labels)", cost["statements"])
	}
	// Every statement is bounded and carries only validated literals.
	for _, s := range append(slices.Clone(chain.statements), labels.statements...) {
		if !strings.Contains(s, "max_rows_to_read") || !strings.Contains(s, "max_execution_time") {
			t.Errorf("unbounded statement:\n%s", s)
		}
	}
	for _, s := range chain.statements {
		if !strings.Contains(s, "max_result_rows") || !strings.HasSuffix(strings.TrimSpace(s), "FORMAT JSON") {
			t.Errorf("statement without a result cap or FORMAT JSON:\n%s", s)
		}
		if strings.Contains(s, "IN (SELECT") {
			t.Errorf("statement builds a set from a subquery:\n%s", s)
		}
	}
	for _, u := range chain.urls {
		if strings.Contains(u, "query_id") {
			t.Errorf("query_id stamped without a caller identity: %s", u)
		}
	}
}

// AT10: nothing labelled gives "no exposure found in our labels",
// attributed_pct 0 and a score equal to weight_unattributed.
func TestNoLabelReport(t *testing.T) {
	chain := &fakeChain{
		assets: []map[string]any{usdtAssetRow("1000000000", "0", 1, 0)},
		edges:  []map[string]any{edgeRow("in", plainSender, "1000000000", "1000000000", "1000000000", 1, [4]string{"1", "1", "1000000000", "1000000000"}, "aa")},
	}
	tool := newTestTool(t, chain, &fakeLabels{})
	res, err := tool.Invoke(context.Background(), chain, mustParams(t, tool, map[string]any{"chain": "tron", "address": at5Target}), "")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := decode(t, res)
	if out["exposure"] != noExposure {
		t.Errorf("exposure = %v", out["exposure"])
	}
	if s := out["score"].(map[string]any)["value"].(float64); s != 45 {
		t.Errorf("score = %v, want 45", s)
	}
	if p := out["summary"].(map[string]any)["attributed_pct"].(float64); p != 0 {
		t.Errorf("attributed_pct = %v", p)
	}
	un := out["unnamed_connections"].([]any)
	if len(un) != 1 || un[0].(map[string]any)["address"] != plainSender || un[0].(map[string]any)["received"] != "1000.000000" ||
		out["unattributed"].(map[string]any)["received"] != "1000.000000" {
		t.Errorf("unnamed connections do not carry the unattributed amount: %v / %v", un, out["unattributed"])
	}
	if out["summary"].(map[string]any)["coverage_tier"] != "strong" {
		t.Errorf("coverage tier: %v", out["summary"])
	}
}

// P2-2: no labelled connection in the report asset (so "no exposure") but a
// risk flag was raised (here an other-asset contact labelled high-risk) — the
// exposure line must point at the flags, not read as "nothing found".
func TestNoExposureStillPointsAtFlags(t *testing.T) {
	const illegal = "TB8hKVScKC9w8QnDnNKJyDwjAPf37rJQfB"
	chain := &fakeChain{
		assets: []map[string]any{usdtAssetRow("1000000000", "0", 1, 0)},
		// report-asset flow: one receipt from an UNLABELLED sender → no connection.
		edges: []map[string]any{edgeRow("in", plainSender, "1000000000", "1000000000", "1000000000", 1, [4]string{"1", "1", "1000000000", "1000000000"}, "aa")},
		// another asset: a labelled high-risk contact → an other_asset_direct_risk flag.
		other: []map[string]any{edgeRow("out", illegal, "500", "500", "500", 9, [4]string{"1", "9", "500", "500"}, "ff")},
	}
	labels := &fakeLabels{rows: map[string][]map[string]any{
		illegal: {{"label_type": "money-laundering", "label_value": "ml-entity", "source": "vendor-x", "recorded_at": "2026-01-01 00:00:00"}},
	}}
	tool := newTestTool(t, chain, labels)
	res, err := tool.Invoke(context.Background(), chain, mustParams(t, tool, map[string]any{"chain": "tron", "address": at5Target}), "")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := decode(t, res)
	if out["exposure"] != noExposureFlagged {
		t.Errorf("exposure = %v, want %q (no labelled connection but a risk flag)", out["exposure"], noExposureFlagged)
	}
	found := false
	for _, f := range out["flags"].([]any) {
		if f.(map[string]any)["type"] == "other_asset_direct_risk" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an other_asset_direct_risk flag: %v", out["flags"])
	}
}

// P1-1: format json returns the report object unchanged (no structured
// envelope); the default (markdown) returns a concise text document as a raw
// text item (a tools.TextResult, no structured content); an unknown format is
// refused.
func TestFormatJSONAndMarkdown(t *testing.T) {
	newChain := func() *fakeChain {
		return &fakeChain{
			assets: []map[string]any{usdtAssetRow("1000000000", "0", 1, 0)},
			edges:  []map[string]any{edgeRow("in", plainSender, "1000000000", "1000000000", "1000000000", 1, [4]string{"1", "1", "1000000000", "1000000000"}, "aa")},
		}
	}
	tool := newTestTool(t, newChain(), &fakeLabels{})

	// json: no structured envelope, the report decodes directly.
	res, err := tool.Invoke(context.Background(), newChain(), mustParams(t, tool, map[string]any{"chain": "tron", "address": at5Target, "format": "json"}), "")
	if err != nil {
		t.Fatalf("json Invoke: %v", err)
	}
	if _, structured := tools.SplitResult(res); structured != nil {
		t.Errorf("format json carried a structured envelope")
	}
	j := decode(t, res)
	if j["score"] == nil {
		t.Errorf("format json did not decode to the report")
	}
	// The leading at_a_glance carries the fields a truncated client trusts.
	ag := j["at_a_glance"].(map[string]any)
	if ag["score"] != j["score"].(map[string]any)["value"] {
		t.Errorf("at_a_glance.score %v != score.value %v", ag["score"], j["score"].(map[string]any)["value"])
	}
	if ag["data_as_of"] != j["params"].(map[string]any)["data_as_of"] {
		t.Errorf("at_a_glance.data_as_of %v != params.data_as_of %v", ag["data_as_of"], j["params"].(map[string]any)["data_as_of"])
	}

	// markdown (the default): ONE text document, and NO structured envelope — a
	// host that reads structuredContent would show the model that and drop the
	// text, so the document must be the text and the only thing returned.
	res, err = tool.Invoke(context.Background(), newChain(), mustParams(t, tool, map[string]any{"chain": "tron", "address": at5Target, "format": "markdown"}), "")
	if err != nil {
		t.Fatalf("markdown Invoke: %v", err)
	}
	items, structured := tools.SplitResult(res)
	if structured != nil {
		t.Errorf("format markdown carried a structured envelope: %v", structured)
	}
	if len(items) != 1 {
		t.Fatalf("markdown returned %d content items, want 1", len(items))
	}
	// The one item is a tools.TextResult, so the handler sends it as text
	// verbatim — not a plain string, which would be JSON-encoded into a quoted
	// literal with escaped newlines.
	tr, ok := items[0].(tools.TextResult)
	if !ok {
		t.Fatalf("markdown item is %T, want tools.TextResult (sent as raw text)", items[0])
	}
	md := tr.Text
	if !strings.Contains(md, "# Address risk report") {
		t.Errorf("markdown content is not the document:\n%s", md)
	}
	// data_as_of must lead the document (a truncating client still sees it).
	if i := strings.Index(md, "Data as of: "); i < 0 || i > 500 {
		t.Errorf("data_as_of not near the top of the markdown (index %d)", i)
	}

	// An unknown format is refused before any statement runs.
	for _, f := range []string{"csv", "bogus"} {
		bad := newChain()
		bt := newTestTool(t, bad, &fakeLabels{})
		if _, err := bt.Invoke(context.Background(), bad, mustParams(t, bt, map[string]any{"chain": "tron", "address": at5Target, "format": f}), ""); err == nil {
			t.Errorf("format %q was accepted", f)
		}
		if len(bad.statements) != 0 {
			t.Errorf("format %q ran %d statements", f, len(bad.statements))
		}
	}
}

func trxAssetRow(recv, sent string, nIn, nOut int) map[string]any {
	firstIn := "2026-08-01 00:00:00"
	if nIn == 0 {
		firstIn = ""
	}
	return map[string]any{"Native": "1", "Contract": "", "AssetId": "", "Symbol": "TRX", "Decimals": float64(6),
		"Received": recv, "Sent": sent, "Received_All": recv, "Sent_All": sent, "n_in": float64(nIn), "n_out": float64(nOut), "txs": float64(nIn + nOut),
		"First": "2026-08-01 00:00:00", "Last": "2026-09-08 08:00:03", "First_All": "2026-08-01 00:00:00", "Last_All": "2026-09-08 08:00:03",
		"First_In": firstIn}
}

// P1-2 / P1-3: a native asset (TRX) with no constant price is priced from the
// chain's price store, so usd_conversion reads "store: …", the USD thresholds
// apply, and a labelled 1-hop Illegal-Service contact above the dust threshold
// sets direct_floor 100 (not 0, the value a disabled threshold produced).
func TestNativePriceEnablesDirectFloor(t *testing.T) {
	const illegal = "TB8hKVScKC9w8QnDnNKJyDwjAPf37rJQfB"
	chain := &fakeChain{
		price:  "0.33075464",
		assets: []map[string]any{trxAssetRow("0", "1000000000", 0, 1)},
		edges: []map[string]any{
			edgeRow("out", illegal, "1000000000", "1000000000", "1000000000", 1, [4]string{"1", "1", "1000000000", "1000000000"}, "ff"),
		},
	}
	labels := &fakeLabels{rows: map[string][]map[string]any{
		illegal: {{"label_type": "money-laundering", "label_value": "ml-entity", "source": "vendor-x", "recorded_at": "2026-01-01 00:00:00"}},
	}}
	tool := newTestTool(t, chain, labels)
	res, err := tool.Invoke(context.Background(), chain, mustParams(t, tool, map[string]any{"chain": "tron", "address": at5Target}), "")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := decode(t, res)
	score := out["score"].(map[string]any)
	if score["direct_floor"].(float64) != 100 || score["value"].(float64) != 100 {
		t.Errorf("direct_floor %v, value %v, want 100/100 (priced Illegal Service above dust)", score["direct_floor"], score["value"])
	}
	params := out["params"].(map[string]any)
	if usd, _ := params["usd_conversion"].(string); !strings.HasPrefix(usd, "store: 1 TRX = 0.33 USD (as of ") {
		t.Errorf("usd_conversion = %q, want a store price", usd)
	}
	priced := false
	for _, d := range out["cost"].(map[string]any)["detail"].([]any) {
		if d.(map[string]any)["name"] == "price" {
			priced = true
		}
	}
	if !priced {
		t.Errorf("no price statement counted in cost.detail: %v", out["cost"])
	}
	// The price statement is bounded, carries the checked key (native TRX = '0')
	// and builds no set from a subquery, like every other statement.
	var priceStmt string
	for _, s := range chain.statements {
		if strings.Contains(s, "dictGetOrNull") {
			priceStmt = s
		}
	}
	if priceStmt == "" {
		t.Fatalf("no price statement was sent")
	}
	for _, want := range []string{"cityHash64('0')", "max_rows_to_read", "max_result_rows", "max_execution_time"} {
		if !strings.Contains(priceStmt, want) {
			t.Errorf("price statement missing %q:\n%s", want, priceStmt)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(priceStmt), "FORMAT JSON") || strings.Contains(priceStmt, "IN (SELECT") {
		t.Errorf("price statement shape:\n%s", priceStmt)
	}
}

// A 1-hop counterparty with a weight-100 label whose transfers are all at or
// below min_amount_usd is not a connection, and does not set the direct floor
// either: sub-min dust must not pin the score to a high-risk type it only
// brushed against (poisoning-shaped). The same counterparty above
// min_amount_usd is a connection and does floor, so the connection threshold is
// the only thing deciding it. Price 0.33 USD/TRX: 60.5 TRX ~= $20 (above the
// $5 dust bar, below the $100 min), 1000 TRX ~= $330 (above min).
func TestSubMinContactDoesNotFloor(t *testing.T) {
	const illegal = "TB8hKVScKC9w8QnDnNKJyDwjAPf37rJQfB"
	run := func(raw, rawMin string) map[string]any {
		chain := &fakeChain{
			price:  "0.33075464",
			assets: []map[string]any{trxAssetRow("0", raw, 0, 1)},
			edges: []map[string]any{
				edgeRow("out", illegal, raw, rawMin, raw, 1, [4]string{"1", "1", raw, rawMin}, "ff"),
			},
		}
		labels := &fakeLabels{rows: map[string][]map[string]any{
			illegal: {{"label_type": "money-laundering", "label_value": "ml-entity", "source": "vendor-x", "recorded_at": "2026-01-01 00:00:00"}},
		}}
		tool := newTestTool(t, chain, labels)
		res, err := tool.Invoke(context.Background(), chain, mustParams(t, tool, map[string]any{"chain": "tron", "address": at5Target}), "")
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		return decode(t, res)
	}

	// Sub-min ($20): the contact is above the dust bar, so it would have floored
	// under the old rule, but it is not a connection — no floor, no floor_from.
	out := run("60500000", "0")
	score := out["score"].(map[string]any)
	if score["direct_floor"].(float64) != 0 {
		t.Errorf("sub-min direct_floor = %v, want 0", score["direct_floor"])
	}
	if score["direct_floor_from"] != nil {
		t.Errorf("sub-min direct_floor_from = %v, want nil", score["direct_floor_from"])
	}
	if conns, _ := out["connections"].([]any); len(conns) != 0 {
		t.Errorf("sub-min connections = %d, want 0 (below min_amount_usd)", len(conns))
	}
	if score["value"].(float64) != 0 {
		t.Errorf("sub-min score value = %v, want 0", score["value"])
	}

	// The same counterparty above min ($330): a connection, and it floors to 100.
	out = run("1000000000", "1000000000")
	score = out["score"].(map[string]any)
	if score["direct_floor"].(float64) != 100 {
		t.Errorf("above-min direct_floor = %v, want 100 (Illegal Service weight 100)", score["direct_floor"])
	}
	if ff, _ := score["direct_floor_from"].(map[string]any); ff == nil || ff["counterparty"] != illegal {
		t.Errorf("above-min direct_floor_from = %v, want the illegal counterparty", score["direct_floor_from"])
	}
}

// P3-4: a transfer to the target's own entity is marked same_entity, left out
// of the score, and kept in the output and breakdown. The same-entity leg here
// is on the sent side under a type with sending level None, so the score (set
// by the received Illegal Service) is unchanged by the exclusion.
func TestSameEntityExcludedFromScore(t *testing.T) {
	const mlAddr = "TUrdaveFV4bnTgLxSDkttBMvKSTt36e1A8" // received: Illegal Service
	const seAddr = "TB8hKVScKC9w8QnDnNKJyDwjAPf37rJQfB" // sent: the target's own entity
	chain := &fakeChain{
		assets: []map[string]any{usdtAssetRow("1000000000", "1000000000", 1, 1)},
		edges: []map[string]any{
			edgeRow("in", mlAddr, "1000000000", "1000000000", "1000000000", 1, [4]string{"1", "1", "1000000000", "1000000000"}, "aa"),
			edgeRow("out", seAddr, "1000000000", "1000000000", "1000000000", 1, [4]string{"1", "1", "1000000000", "1000000000"}, "bb"),
		},
	}
	labels := &fakeLabels{rows: map[string][]map[string]any{
		at5Target: {{"label_type": "custodian", "label_value": "myexchange", "source": "vendor-x", "recorded_at": "2026-01-01 00:00:00"}},
		mlAddr:    {{"label_type": "money-laundering", "label_value": "ml-entity", "source": "vendor-x", "recorded_at": "2026-01-01 00:00:00"}},
		seAddr:    {{"label_type": "custodian", "label_value": "myexchange", "source": "vendor-x", "recorded_at": "2026-01-01 00:00:00"}},
	}}
	tool := newTestTool(t, chain, labels)
	res, err := tool.Invoke(context.Background(), chain, mustParams(t, tool, map[string]any{"chain": "tron", "address": at5Target}), "")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := decode(t, res)
	if owner := out["summary"].(map[string]any)["labels"].(map[string]any)["owner"]; owner != "myexchange" {
		t.Fatalf("owner = %v, want myexchange", owner)
	}
	score := out["score"].(map[string]any)
	if score["value"].(float64) != 100 {
		t.Errorf("score.value = %v, want 100 (received Illegal Service, unchanged by the exclusion)", score["value"])
	}
	if score["sent_score"] != nil {
		t.Errorf("sent_score = %v, want null (its only leg is the same-entity self-transfer)", score["sent_score"])
	}
	var se map[string]any
	for _, c := range out["connections"].([]any) {
		cm := c.(map[string]any)
		if cm["entity"] == "myexchange" && cm["entity_type"] == "Online Wallet" {
			se = cm
		}
	}
	if se == nil {
		t.Fatalf("same-entity connection missing from the output: %v", out["connections"])
	}
	if se["same_entity"] != true {
		t.Errorf("connection not marked same_entity: %v", se)
	}
	found := false
	for _, r := range out["breakdown"].(map[string]any)["sent"].([]any) {
		if r.(map[string]any)["entity_type"] == "Online Wallet" {
			found = true
		}
	}
	if !found {
		t.Errorf("same-entity leg missing from the sent breakdown: %v", out["breakdown"])
	}
}

// P3-4 (Option A): removing a same-entity leg from a direction's total is an
// equivalence — the report scores exactly as one built without that leg at all
// (the self flow contributes nothing and the other connection's share is taken
// over the total less the self amount). A dilution bug (keeping the self leg in
// the total) would lower sent_score here, so the test compares sent_score, not
// only the floor-dominated value.
func TestSameEntityOptionAEquivalence(t *testing.T) {
	const gamblingAddr = "TUrdaveFV4bnTgLxSDkttBMvKSTt36e1A8" // sent: Gambling (sending High)
	const seAddr = "TXHX2NUvcgft4zs6MkkUtdNVERPTavHDWN"       // sent: the target's own entity
	build := func(withSelf bool) map[string]any {
		sentTot, nOut, cps := "1000000000", 1, "1"
		if withSelf {
			sentTot, nOut, cps = "2000000000", 2, "2"
		}
		dir := [4]string{cps, cps, sentTot, sentTot}
		edges := []map[string]any{edgeRow("out", gamblingAddr, "1000000000", "1000000000", "1000000000", 1, dir, "g1")}
		if withSelf {
			edges = append(edges, edgeRow("out", seAddr, "1000000000", "1000000000", "1000000000", 1, dir, "s1"))
		}
		chain := &fakeChain{assets: []map[string]any{usdtAssetRow("0", sentTot, 0, nOut)}, edges: edges}
		labels := &fakeLabels{rows: map[string][]map[string]any{
			at5Target:    {{"label_type": "custodian", "label_value": "myexchange", "source": "vendor-x", "recorded_at": "2026-01-01 00:00:00"}},
			gamblingAddr: {{"label_type": "gambling", "label_value": "casino-x", "source": "vendor-x", "recorded_at": "2026-01-01 00:00:00"}},
			seAddr:       {{"label_type": "custodian", "label_value": "myexchange", "source": "vendor-x", "recorded_at": "2026-01-01 00:00:00"}},
		}}
		tool := newTestTool(t, chain, labels)
		res, err := tool.Invoke(context.Background(), chain, mustParams(t, tool, map[string]any{"chain": "tron", "address": at5Target, "direction": "sent"}), "")
		if err != nil {
			t.Fatalf("Invoke(withSelf=%v): %v", withSelf, err)
		}
		return decode(t, res)
	}
	withS, without := build(true), build(false)
	sw, so := withS["score"].(map[string]any), without["score"].(map[string]any)
	if sw["value"] != so["value"] || fmt.Sprint(sw["sent_score"]) != fmt.Sprint(so["sent_score"]) {
		t.Errorf("Option A broken: with-self score %v, without-self score %v", sw, so)
	}
	// Gambling (75 x High = 75) over the total less the self amount.
	if sw["sent_score"].(float64) != 75 {
		t.Errorf("sent_score = %v, want 75 (Gambling on the self-reduced total)", sw["sent_score"])
	}
	// The same-entity connection is still in the output and the breakdown.
	var se map[string]any
	for _, c := range withS["connections"].([]any) {
		if cm := c.(map[string]any); cm["entity"] == "myexchange" && cm["entity_type"] == "Online Wallet" {
			se = cm
		}
	}
	if se == nil || se["same_entity"] != true {
		t.Errorf("same-entity connection missing or unmarked: %v", se)
	}
	found := false
	for _, r := range withS["breakdown"].(map[string]any)["sent"].([]any) {
		if r.(map[string]any)["entity_type"] == "Online Wallet" {
			found = true
		}
	}
	if !found {
		t.Errorf("same-entity leg missing from the sent breakdown: %v", withS["breakdown"])
	}
}

func TestRefusals(t *testing.T) {
	chain := &fakeChain{}
	tool := newTestTool(t, chain, &fakeLabels{})
	for _, p := range []map[string]any{
		{"chain": "bitcoin", "address": at5Target},
		{"chain": "solana", "address": at5Target},
		{"chain": "tron", "address": "TDNb'; DROP TABLE x; --"},
		{"chain": "tron", "address": at5Target, "view": "deep"},
		{"chain": "tron", "address": at5Target, "view": "all", "max_hops": 7},
		{"chain": "tron", "address": at5Target, "view": "all", "max_hops": 0},
		{"chain": "tron", "address": at5Target, "asset": "USDT"},
		{"chain": "tron", "address": at5Target, "direction": "sideways"},
	} {
		if _, err := tool.Invoke(context.Background(), chain, mustParams(t, tool, p), ""); err == nil {
			t.Errorf("accepted %v", p)
		}
	}
	if len(chain.statements) != 0 {
		t.Errorf("a refused call ran %d statements", len(chain.statements))
	}
}

// timeParamRe is the after_time / before_time pattern of the tool's
// configuration (risk.yaml).
var timeParamRe = regexp.MustCompile(`^([0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])([ T]([01][0-9]|2[0-3]):[0-5][0-9](:[0-5][0-9](\.[0-9]+)?)?(Z|[+-]([01][0-9]|2[0-3]):?[0-5][0-9])?)?|[0-9]{10})?$`)

// N20: every spelling the pattern admits is read, to the instant it names.
// The samples are the pattern's whole grammar: a date; a date and a time with
// T or a space, minutes, seconds or seconds with a fraction, and no zone, Z,
// +HH:MM or +HHMM; and unix seconds. The six the review found refused are
// among them.
func TestParseTimeReadsEverySpellingThePatternAdmits(t *testing.T) {
	type sample struct {
		in   string
		want time.Time
	}
	at := func(h, m, s int) time.Time { return time.Date(2026, 9, 20, h, m, s, 0, time.UTC) }
	samples := []sample{{"2026-09-20", at(0, 0, 0)}, {"1789862400", time.Unix(1789862400, 0).UTC()}}
	for _, sep := range []string{"T", " "} {
		for _, clock := range []struct {
			text string
			sec  int // a fraction rounds up to the next second
		}{{"00:00", 0}, {"00:00:00", 0}, {"00:00:00.250", 1}} {
			for _, zone := range []struct {
				text  string
				shift int
			}{{"", 0}, {"Z", 0}, {"+03:00", -3}, {"+0300", -3}, {"-0130", 1}} {
				in := "2026-09-20" + sep + clock.text + zone.text
				want := at(0, 0, clock.sec).Add(time.Duration(zone.shift) * time.Hour)
				if zone.text == "-0130" {
					want = at(1, 30, clock.sec)
				}
				samples = append(samples, sample{in, want})
			}
		}
	}
	for _, s := range []string{"2026-09-20 00:00:00Z", "2026-09-20 00:00:00+03:00", "2026-09-20T00:00Z",
		"2026-09-20T00:00:00+0300", "2026-09-20 00:00Z", "2026-09-20T00:00+03:00"} {
		found := false
		for _, x := range samples {
			found = found || x.in == s
		}
		if !found {
			t.Fatalf("the review's sample %q is not covered", s)
		}
	}
	for _, s := range samples {
		if !timeParamRe.MatchString(s.in) {
			t.Fatalf("sample %q does not pass the pattern", s.in)
		}
		got, err := parseTime(s.in)
		if err != nil {
			t.Errorf("%q passes the pattern and is refused: %v", s.in, err)
			continue
		}
		if !got.Equal(s.want) {
			t.Errorf("%q = %s, want %s", s.in, got, s.want)
		}
	}
	t.Logf("%d spellings read", len(samples))
	// The pattern also admits impossible dates; those are refused, not read.
	for _, s := range []string{"2026-02-30", "2026-09-31 00:00"} {
		if _, err := parseTime(s); err == nil || !timeParamRe.MatchString(s) {
			t.Errorf("%q: err = %v", s, err)
		}
	}
}

func mustParams(t *testing.T, tool Tool, p map[string]any) parameters.ParamValues {
	t.Helper()
	// The structural tests read the report as JSON; the default output is now the
	// markdown document (one text item, no structured envelope), so they ask for
	// json unless a test sets format itself. The markdown default is exercised by
	// TestFormatJSONAndMarkdown, which passes format explicitly.
	if _, ok := p["format"]; !ok {
		p = maps.Clone(p)
		p["format"] = "json"
	}
	pv, err := parameters.ParseParams(tool.StaticParameters, p, nil)
	if err != nil {
		t.Fatalf("ParseParams(%v): %v", p, err)
	}
	return pv
}
