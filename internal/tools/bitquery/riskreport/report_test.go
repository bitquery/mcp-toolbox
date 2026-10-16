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
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

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

func (f *fakeChain) SourceType() string                    { return "http" }
func (f *fakeChain) ToConfig() sources.SourceConfig        { return nil }
func (f *fakeChain) HttpBaseURL() string                   { return "http://gateway.test:8123" }
func (f *fakeChain) HttpDefaultHeaders() map[string]string { return map[string]string{"X-Test": "1"} }
func (f *fakeChain) HttpQueryParams() map[string]string {
	return map[string]string{"default_format": "JSONEachRow"}
}
func (f *fakeChain) BitqueryLegacyBillingPaths() string { return "" }
func (f *fakeChain) RunRequest(ctx context.Context, req *http.Request) (any, error) {
	body, _ := io.ReadAll(req.Body)
	s := string(body)
	f.statements = append(f.statements, s)
	f.urls = append(f.urls, req.URL.String())
	var rows []map[string]any
	read := int64(1000)
	switch {
	case strings.Contains(s, "GROUP BY Native, SC, AssetId"):
		rows = f.assets
	case strings.Contains(s, "AS first_in"):
		for _, r := range f.edges {
			r["R"] = float64(1)
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
		"First": "2026-08-01 00:00:00", "Last": "2026-09-08 08:00:03", "First_All": "2026-08-01 00:00:00", "Last_All": "2026-09-08 08:00:03"}
}

func edgeRow(dir, addr, raw, rawMin, rawMax string, n int, dirTotals [4]string, tx ...string) map[string]any {
	txs := make([]any, len(tx))
	for i, x := range tx {
		txs[i] = x
	}
	return map[string]any{"Dir": dir, "Counterparty": addr, "n": float64(n), "Raw": raw, "n_min": float64(n), "Raw_Above_Min": rawMin,
		"n_dust": float64(0), "Raw_Max": rawMax, "Raw_Early": "0", "First": "2026-08-30 10:36:03", "Last": "2026-09-08 08:00:03",
		"Tx_Above_Dust": txs, "Tx": txs, "First_Receipt": "2026-08-01 00:00:00",
		"Dir_Counterparties": dirTotals[0], "Dir_Transfers": dirTotals[1], "Dir_Raw": dirTotals[2], "Dir_Raw_Above_Min": dirTotals[3]}
}

func decode(t *testing.T, v any) map[string]any {
	t.Helper()
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

func mustParams(t *testing.T, tool Tool, p map[string]any) parameters.ParamValues {
	t.Helper()
	pv, err := parameters.ParseParams(tool.StaticParameters, p, nil)
	if err != nil {
		t.Fatalf("ParseParams(%v): %v", p, err)
	}
	return pv
}
