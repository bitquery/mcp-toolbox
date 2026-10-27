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
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/googleapis/mcp-toolbox/internal/sources"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

// fakeEthChain answers the Ethereum statements from canned rows and records
// every statement it is sent. The shapes mirror the real gateway: the assets
// statement groups by (Native, SC) and the counterparty statement carries the
// per-direction window totals; the price read returns one Price string ("" = a
// NULL price). It matches on the statement text the ETH kind builds.
type fakeEthChain struct {
	assets, edges, other []map[string]any
	price                string
	statements           []string
	headers              []string
}

func (f *fakeEthChain) SourceType() string             { return "http" }
func (f *fakeEthChain) ToConfig() sources.SourceConfig { return nil }
func (f *fakeEthChain) HttpBaseURL() string            { return "http://eth-gateway.test:8123" }
func (f *fakeEthChain) HttpDefaultHeaders() map[string]string {
	return map[string]string{"X-Bitquery-Database": "eth_api"}
}
func (f *fakeEthChain) HttpQueryParams() map[string]string {
	return map[string]string{"default_format": "JSONEachRow", "wait_end_of_query": "0"}
}
func (f *fakeEthChain) BitqueryLegacyBillingPaths() string { return "" }

func (f *fakeEthChain) RunRequest(_ context.Context, req *http.Request) (any, error) {
	body, _ := io.ReadAll(req.Body)
	s := string(body)
	f.statements = append(f.statements, s)
	f.headers = append(f.headers, req.Header.Get("X-Bitquery-Table"))
	var rows []map[string]any
	switch {
	case strings.Contains(s, "prices_eth_dict"):
		var p any
		if f.price != "" {
			p = f.price
		}
		rows = []map[string]any{{"Price": p}}
	case strings.Contains(s, "AS Dir_Counterparties"):
		for _, r := range f.edges {
			r["R"] = float64(1)
			rows = append(rows, r)
		}
		for _, r := range f.other {
			r["R"] = float64(0)
			rows = append(rows, r)
		}
	case strings.Contains(s, "GROUP BY Native, SC"):
		rows = f.assets
	}
	data := make([]any, len(rows))
	for i, r := range rows {
		data[i] = r
	}
	return map[string]any{"data": data, "rows": float64(len(rows)),
		"statistics": map[string]any{"rows_read": float64(1000), "bytes_read": float64(20000), "elapsed": 0.01}}, nil
}

// ethNativeAssetRow is the assets row of a native-ETH-only address.
func ethNativeAssetRow(recv, sent string, nIn, nOut int) map[string]any {
	return map[string]any{"Native": "1", "Contract": "", "Symbol": "ETH", "Decimals": float64(18),
		"Received": recv, "Sent": sent, "Received_All": recv, "Sent_All": sent, "n_in": float64(nIn), "n_out": float64(nOut), "txs": float64(nIn + nOut),
		"First": "2026-08-01 00:00:00", "Last": "2026-09-08 08:00:03", "First_All": "2026-08-01 00:00:00", "Last_All": "2026-09-08 08:00:03",
		"First_In": "2026-08-01 00:00:00"}
}

// ethTokenAssetRow is the assets row of an ERC-20-only address.
func ethTokenAssetRow(contract, symbol, recv, sent string, nIn, nOut int) map[string]any {
	return map[string]any{"Native": "0", "Contract": contract, "Symbol": symbol, "Decimals": float64(6),
		"Received": recv, "Sent": sent, "Received_All": recv, "Sent_All": sent, "n_in": float64(nIn), "n_out": float64(nOut), "txs": float64(nIn + nOut),
		"First": "2026-08-01 00:00:00", "Last": "2026-09-08 08:00:03", "First_All": "2026-08-01 00:00:00", "Last_All": "2026-09-08 08:00:03",
		"First_In": "2026-08-01 00:00:00"}
}

func newEthTestTool(t *testing.T, eth *fakeEthChain, labels *fakeLabels) Tool {
	t.Helper()
	mc := loadMethodConfig(t)
	cfg := Config{
		ConfigBase:   tools.ConfigBase{Name: "address_risk_report", Description: "risk report"},
		Type:         resourceType,
		Source:       "eth-src",
		ChainSources: map[string]string{"tron": "tron-src", "ethereum": "eth-src"},
		LabelsSource: "labels-src",
		Parameters: parameters.Parameters{
			parameters.NewStringParameter("chain", "chain"),
			parameters.NewStringParameter("address", "address"),
			parameters.NewStringParameter("asset", "asset", parameters.WithStringDefault("")),
			parameters.NewStringParameter("direction", "direction", parameters.WithStringDefault("both")),
			// The production cfg defaults view to "all"; on ethereum that is coerced
			// to the direct view (a bare call must not fail), so mirror it here.
			parameters.NewStringParameter("view", "view", parameters.WithStringDefault("all")),
			parameters.NewStringParameter("format", "format", parameters.WithStringDefault("")),
			parameters.NewStringParameter("after_time", "after_time", parameters.WithStringDefault("")),
			parameters.NewStringParameter("before_time", "before_time", parameters.WithStringDefault("")),
			parameters.NewIntParameter("max_hops", "max_hops", parameters.WithIntRequired(false)),
			parameters.NewFloatParameter("min_amount_usd", "min_amount_usd", parameters.WithFloatRequired(false)),
		},
		MethodConfig: mc,
	}
	ctx := tools.WithSourceLookup(context.Background(), tools.SourceMap{"tron-src": &fakeChain{}, "eth-src": eth, "labels-src": labels})
	tool, err := cfg.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return tool.(Tool)
}

// Canonical valid addresses for the Ethereum tests (0x + 40 hex).
const (
	ethTarget = "0x1111111111111111111111111111111111111111"
	ethSanc   = "0x2222222222222222222222222222222222222222" // a sanctioned source (received)
	ethPlain  = "0x3333333333333333333333333333333333333333" // an unlabelled source (received)
	ethUSDT   = "0xdac17f958d2ee523a2206206994597c13d831ec7" // a token contract
)

// A direct-view native-ETH report end to end: a sanctioned received counterparty
// above the dust bar sets the score to 100 and raises direct_sanctions, an
// unnamed one is carried into unattributed, and the price comes from the ETH
// store at the pin. The statements are the Ethereum shapes (0x hex codec, the ETH
// price dictionary), not Tron's.
func TestEthereumDirectReport(t *testing.T) {
	eth := &fakeEthChain{
		price:  "2500",
		assets: []map[string]any{ethNativeAssetRow("3000000000000000000", "0", 2, 0)},
		edges: []map[string]any{
			// 2 ETH from the sanctioned source, 1 ETH from the unnamed one, both above min.
			edgeRow("in", ethSanc, "2000000000000000000", "2000000000000000000", "2000000000000000000", 1,
				[4]string{"2", "2", "3000000000000000000", "3000000000000000000"}, "aa11"),
			edgeRow("in", ethPlain, "1000000000000000000", "1000000000000000000", "1000000000000000000", 1,
				[4]string{"2", "2", "3000000000000000000", "3000000000000000000"}, "bb22"),
		},
	}
	labels := &fakeLabels{rows: map[string][]map[string]any{
		ethSanc: {{"label_type": "sanctioned-entity", "label_value": "sanctioned-evilcorp", "source": "ofac", "recorded_at": "2026-01-01 00:00:00"}},
	}}
	tool := newEthTestTool(t, eth, labels)
	res, err := tool.Invoke(context.Background(), eth, mustParams(t, tool, map[string]any{"chain": "ethereum", "address": ethTarget}), "")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := decode(t, res)

	sum := out["summary"].(map[string]any)
	if sum["chain"] != "ethereum" || sum["received"] != "3.000000000000000000" || sum["balance"] != "3.000000000000000000" {
		t.Errorf("summary: %v", sum)
	}
	if sum["asset"].(map[string]any)["symbol"] != "ETH" {
		t.Errorf("asset not ETH: %v", sum["asset"])
	}

	// The report is the direct view (view all was coerced) and says so.
	p := out["params"].(map[string]any)
	if p["view"] != "direct" {
		t.Errorf("params.view = %v, want direct", p["view"])
	}
	if usd, _ := p["usd_conversion"].(string); !strings.HasPrefix(usd, "store: 1 ETH = 2500 USD") {
		t.Errorf("usd_conversion = %q, want the ETH store price", usd)
	}

	// Connection: Sanctions (ethSanc, received 2 ETH).
	var sc map[string]any
	for _, c := range out["connections"].([]any) {
		if cm := c.(map[string]any); cm["entity_type"] == "Sanctions" {
			sc = cm
		}
	}
	if sc == nil || sc["received"] != "2.000000000000000000" {
		t.Errorf("Sanctions connection: %v", out["connections"])
	}

	// Unnamed: ethPlain, received 1 ETH, carried into unattributed.
	un := out["unnamed_connections"].([]any)
	if len(un) != 1 || un[0].(map[string]any)["address"] != ethPlain || un[0].(map[string]any)["received"] != "1.000000000000000000" {
		t.Errorf("unnamed connections: %v", un)
	}
	if ua := out["unattributed"].(map[string]any); ua["received"] != "1.000000000000000000" {
		t.Errorf("unattributed: %v", ua)
	}

	// Score: a sanctions source above dust sets the direct floor to 100.
	score := out["score"].(map[string]any)
	if score["value"].(float64) != 100 || score["direct_floor"].(float64) != 100 {
		t.Errorf("score: %v", score)
	}
	found := false
	for _, f := range out["flags"].([]any) {
		if f.(map[string]any)["type"] == "direct_sanctions" {
			found = true
		}
	}
	if !found {
		t.Errorf("no direct_sanctions flag: %v", out["flags"])
	}

	// Labels were fetched for the Ethereum counterparties, chain = 'ethereum'.
	labelStmt := strings.Join(tool.labels.(*fakeLabels).statements, "\n")
	if !strings.Contains(labelStmt, "chain = 'ethereum'") || !strings.Contains(labelStmt, "'"+ethSanc+"'") {
		t.Errorf("labels not fetched for ethereum counterparties: %q", labelStmt)
	}

	assertEthStatements(t, eth)
}

// assertEthStatements checks the Ethereum statements use the ETH tables and the
// 0x hex codec (unhex / hex), never Tron's base58, and end FORMAT JSON; and that
// the price read is the ETH dictionary with the native key cityHash64(”).
func assertEthStatements(t *testing.T, eth *fakeEthChain) {
	t.Helper()
	if len(eth.statements) == 0 {
		t.Fatal("no ethereum statements were sent")
	}
	sawPrice := false
	for _, s := range eth.statements {
		if strings.Contains(s, "base58") {
			t.Errorf("ethereum statement uses the Tron base58 codec:\n%s", s)
		}
		if !strings.HasSuffix(strings.TrimSpace(s), "FORMAT JSON") {
			t.Errorf("ethereum statement does not end FORMAT JSON:\n%s", s)
		}
		if strings.Contains(s, "prices_eth_dict") {
			sawPrice = true
			if !strings.Contains(s, "cityHash64('')") {
				t.Errorf("ethereum native price read is not keyed on cityHash64(''):\n%s", s)
			}
		} else if !strings.Contains(s, "eth_api.transfers_") || !strings.Contains(s, "unhex(") {
			t.Errorf("ethereum statement is not an eth_api/hex read:\n%s", s)
		}
	}
	if !sawPrice {
		t.Error("the ethereum report read no price")
	}
}

// Refusals: a non-ETH-hex address is refused before any statement runs; a token
// contract asset is accepted; view all is coerced to the direct view.
func TestEthereumRefusals(t *testing.T) {
	for _, p := range []map[string]any{
		{"chain": "ethereum", "address": at5Target},        // a Tron address on ethereum
		{"chain": "ethereum", "address": btcTarget},        // a Bitcoin address on ethereum
		{"chain": "ethereum", "address": "not an address"}, // junk
		{"chain": "ethereum", "address": ethTarget, "asset": "TRX"},
	} {
		eth := &fakeEthChain{price: "2500", assets: []map[string]any{ethNativeAssetRow("1000000000000000000", "0", 1, 0)}}
		tool := newEthTestTool(t, eth, &fakeLabels{})
		if _, err := tool.Invoke(context.Background(), eth, mustParams(t, tool, p), ""); err == nil {
			t.Errorf("accepted %v", p)
		}
		if len(eth.statements) != 0 {
			t.Errorf("a refused call %v ran %d statements", p, len(eth.statements))
		}
	}

	// A mixed-case 0x address is accepted (lower-cased) and a token contract asset
	// is accepted.
	eth := &fakeEthChain{price: "2500", assets: []map[string]any{ethNativeAssetRow("1000000000000000000", "0", 1, 0)}}
	tool := newEthTestTool(t, eth, &fakeLabels{})
	if _, err := tool.Invoke(context.Background(), eth, mustParams(t, tool,
		map[string]any{"chain": "ethereum", "address": strings.ToUpper("0X") + "1111111111111111111111111111111111111111"}), ""); err != nil {
		t.Errorf("mixed-case address refused: %v", err)
	}
}

// On a token report the native-ETH dust bar for other-asset contacts is a USD
// bar in wei, so the report reads the native ETH price (cityHash64(”)) even
// though the report asset is a token, and the counterparty statement's native
// dust threshold is that wei bar, not the unit-count fallback (10).
func TestEthereumTokenFetchesNativeDustPrice(t *testing.T) {
	eth := &fakeEthChain{
		price:  "2500",
		assets: []map[string]any{ethTokenAssetRow(ethUSDT, "USDT", "5000000000", "0", 1, 0)},
		edges: []map[string]any{
			edgeRow("in", ethSanc, "5000000000", "5000000000", "5000000000", 1,
				[4]string{"1", "1", "5000000000", "5000000000"}, "aa11"),
		},
	}
	labels := &fakeLabels{rows: map[string][]map[string]any{
		ethSanc: {{"label_type": "sanctioned-entity", "label_value": "sanctioned-evilcorp", "source": "ofac", "recorded_at": "2026-01-01 00:00:00"}},
	}}
	tool := newEthTestTool(t, eth, labels)
	if _, err := tool.Invoke(context.Background(), eth, mustParams(t, tool,
		map[string]any{"chain": "ethereum", "address": ethTarget, "asset": ethUSDT}), ""); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	// Two price reads: the report asset (the contract key) and native ETH ('').
	var tokenKey, nativeKey bool
	for _, s := range eth.statements {
		if !strings.Contains(s, "prices_eth_dict") {
			continue
		}
		if strings.Contains(s, "cityHash64('')") {
			nativeKey = true
		}
		if strings.Contains(s, "cityHash64('"+strings.TrimPrefix(ethUSDT, "0x")+"')") {
			tokenKey = true
		}
	}
	if !tokenKey || !nativeKey {
		t.Errorf("a token report must read both the token price and the native ETH price; got token=%v native=%v", tokenKey, nativeKey)
	}
	// The counterparty statement's native-dust bar is the wei bar, not the unit
	// count 10.
	var cp string
	for _, s := range eth.statements {
		if strings.Contains(s, "AS Dir_Counterparties") {
			cp = s
		}
	}
	if strings.Contains(cp, "Transfer_Currency_Native = 1 AND Transfer_Amount <= toUInt256(10)") {
		t.Errorf("token report used the unit-count native dust bar instead of the wei bar:\n%s", cp)
	}
}
