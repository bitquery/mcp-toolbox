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
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"

	"github.com/googleapis/mcp-toolbox/internal/sources"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

// fakeBtcChain answers the Bitcoin statements from canned rows and records every
// statement it is sent. It mirrors the real gateway's shapes: an address-keyed
// read returns one 'agg' row and the per-transaction 'tx' rows; a transaction
// read returns the inputs or outputs of the transactions named in its tuple
// set; the price read returns one Price string ("" = no price).
type fakeBtcChain struct {
	recvAgg  map[string]any
	recvTx   []map[string]any
	spendAgg map[string]any
	spendTx  []map[string]any
	inputs   map[btcTxKey][]map[string]any
	outputs  map[btcTxKey][]map[string]any
	price    string
	// priceAsOf is the RFC3339 hour the canned price is as of (the newest priced
	// hour the real fallback statement would return); empty lets priceBitcoin
	// label it the pin.
	priceAsOf string
	// addrXfers is the lifetime-totals row (whole-history summary).
	addrXfers map[string]any
	// aggErr is returned for a full agg (windowed summary) read; partsErr for any
	// transaction inputs/outputs read — to exercise the error-to-cut / refuse paths.
	aggErr   error
	partsErr error
	// errKey makes a transaction read fail only when it names this key, so a side
	// that reads earlier chunks and then hits a refused one can be exercised.
	errKey    *btcTxKey
	errKeyErr error

	statements []string
	headers    []string
	urls       []string
}

func (f *fakeBtcChain) SourceType() string             { return "http" }
func (f *fakeBtcChain) ToConfig() sources.SourceConfig { return nil }
func (f *fakeBtcChain) HttpBaseURL() string            { return "http://btc-gateway.test:8123" }
func (f *fakeBtcChain) HttpDefaultHeaders() map[string]string {
	return map[string]string{"X-Test": "1"}
}
func (f *fakeBtcChain) HttpQueryParams() map[string]string {
	return map[string]string{"default_format": "JSONEachRow", "wait_end_of_query": "0"}
}
func (f *fakeBtcChain) BitqueryLegacyBillingPaths() string { return "" }

// partsErrorFor returns the error a transaction read should fail with: partsErr
// always, or errKeyErr only when the statement names errKey (so earlier chunks
// succeed and a later one is refused).
func (f *fakeBtcChain) partsErrorFor(s string) error {
	if f.partsErr != nil {
		return f.partsErr
	}
	if f.errKey != nil {
		k := *f.errKey
		if strings.Contains(s, fmt.Sprintf("(%d, %d)", k.Block, k.TxIndex)) ||
			strings.Contains(s, fmt.Sprintf("block = %d AND tx_index = %d", k.Block, k.TxIndex)) {
			return f.errKeyErr
		}
	}
	return nil
}

func (f *fakeBtcChain) partsFor(s string, parts map[btcTxKey][]map[string]any) []map[string]any {
	var rows []map[string]any
	for k, rs := range parts {
		// A single transaction is an equality, several are a tuple set.
		tuple := fmt.Sprintf("(%d, %d)", k.Block, k.TxIndex)
		eq := fmt.Sprintf("block = %d AND tx_index = %d", k.Block, k.TxIndex)
		if strings.Contains(s, tuple) || strings.Contains(s, eq) {
			rows = append(rows, rs...)
		}
	}
	return rows
}

func (f *fakeBtcChain) RunRequest(_ context.Context, req *http.Request) (any, error) {
	body, _ := io.ReadAll(req.Body)
	s := string(body)
	f.statements = append(f.statements, s)
	f.headers = append(f.headers, req.Header.Get("X-Bitquery-Table"))
	f.urls = append(f.urls, req.URL.String())
	// A full agg statement carries the 'agg' marker; a list-only one does not.
	full := strings.Contains(s, "'agg' AS kind")
	var rows []map[string]any
	switch {
	case strings.Contains(s, "dictHas('market_price'"):
		// The real statement returns the newest priced hour at or before the pin,
		// or no row when none in the day has a price. Mirror that: a row with the
		// canned price and its as-of hour, or no row when price is unset.
		if f.price != "" {
			r := map[string]any{"price": f.price}
			if f.priceAsOf != "" {
				r["asof"] = f.priceAsOf
			}
			rows = []map[string]any{r}
		}
	case strings.Contains(s, "bitcoin_flow.address_transfers"):
		if f.addrXfers != nil {
			rows = []map[string]any{f.addrXfers}
		}
	case strings.Contains(s, btcOutputsToAddress):
		if full && f.aggErr != nil {
			return nil, f.aggErr
		}
		if full {
			rows = append([]map[string]any{f.recvAgg}, f.recvTx...)
		} else {
			rows = append(rows, f.recvTx...)
		}
	case strings.Contains(s, btcInputsByAddress):
		if full && f.aggErr != nil {
			return nil, f.aggErr
		}
		if full {
			rows = append([]map[string]any{f.spendAgg}, f.spendTx...)
		} else {
			rows = append(rows, f.spendTx...)
		}
	case strings.Contains(s, btcTxInputs):
		if err := f.partsErrorFor(s); err != nil {
			return nil, err
		}
		rows = f.partsFor(s, f.inputs)
	case strings.Contains(s, btcTxOutputs):
		if err := f.partsErrorFor(s); err != nil {
			return nil, err
		}
		rows = f.partsFor(s, f.outputs)
	}
	data := make([]any, len(rows))
	for i, r := range rows {
		data[i] = r
	}
	return map[string]any{"data": data, "rows": float64(len(rows)),
		"statistics": map[string]any{"rows_read": float64(1000), "bytes_read": float64(20000), "elapsed": 0.01}}, nil
}

func btcAggRowM(amtW, amtAll string, n int, firstW, lastW, firstAll, lastAll string) map[string]any {
	return map[string]any{"kind": "agg", "blk": "", "txi": "", "amt_w": amtW, "amt_all": amtAll, "n_w": fmt.Sprint(n),
		"first_w": firstW, "last_w": lastW, "first_all": firstAll, "last_all": lastAll, "tx": ""}
}

func btcTxRowM(block, txi uint64, amtW string, n int, firstW, tx string) map[string]any {
	return map[string]any{"kind": "tx", "blk": fmt.Sprint(block), "txi": fmt.Sprint(txi), "amt_w": amtW, "amt_all": "", "n_w": fmt.Sprint(n),
		"first_w": firstW, "last_w": "", "first_all": "", "last_all": "", "tx": tx}
}

func btcPartRowM(block, txi uint64, addr, val, si, tx string) map[string]any {
	r := map[string]any{"blk": fmt.Sprint(block), "txi": fmt.Sprint(txi), "addr": addr, "val": val, "si": si}
	if tx != "" {
		r["tx"] = tx
	}
	return r
}

func btcXfersRowM(received, sent, firstIn, lastIn, firstOut, lastOut string) map[string]any {
	return map[string]any{"received": received, "sent": sent,
		"first_in": firstIn, "last_in": lastIn, "first_out": firstOut, "last_out": lastOut}
}

func newBtcTestTool(t *testing.T, btc *fakeBtcChain, labels *fakeLabels) Tool {
	t.Helper()
	mc := loadMethodConfig(t)
	cfg := Config{
		ConfigBase:   tools.ConfigBase{Name: "address_risk_report", Description: "risk report"},
		Type:         resourceType,
		Source:       "btc-src",
		ChainSources: map[string]string{"tron": "tron-src", "bitcoin": "btc-src"},
		LabelsSource: "labels-src",
		Parameters: parameters.Parameters{
			parameters.NewStringParameter("chain", "chain"),
			parameters.NewStringParameter("address", "address"),
			parameters.NewStringParameter("asset", "asset", parameters.WithStringDefault("")),
			parameters.NewStringParameter("direction", "direction", parameters.WithStringDefault("both")),
			// The production cfg defaults view to "all"; on bitcoin that is coerced
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
	ctx := tools.WithSourceLookup(context.Background(), tools.SourceMap{"tron-src": &fakeChain{}, "btc-src": btc, "labels-src": labels})
	tool, err := cfg.Initialize(ctx)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return tool.(Tool)
}

// Canonical valid addresses for the Bitcoin tests.
const (
	btcTarget = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"         // the address reported on
	btcExch   = "3EktnHQD7RiAE6uzMj2ZifT9YgRrkSgzQX"         // a labelled source (received)
	btcPlain  = "bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq" // an unlabelled source (received)
	btcSanc   = "1BoatSLRHtKNngkdXEeobR76b53LETtpyT"         // a labelled destination (sent)
)

// btcSampleChain builds a report input: the target receives 1 BTC in one
// transaction funded 0.6 by btcExch and 0.4 by btcPlain, and spends 1 BTC in one
// transaction paying 0.7 to btcSanc, 0.29 back to itself (change) and 0.01 fee.
func btcSampleChain() *fakeBtcChain {
	return &fakeBtcChain{
		price:     "100000",
		addrXfers: btcXfersRowM("1.00000000", "1.00000000", "2026-09-01 00:00:00", "2026-09-01 00:00:00", "2026-09-05 00:00:00", "2026-09-05 00:00:00"),
		recvAgg:   btcAggRowM("1.00000000", "1.00000000", 1, "2026-09-01 00:00:00", "2026-09-01 00:00:00", "2026-09-01 00:00:00", "2026-09-01 00:00:00"),
		recvTx:    []map[string]any{btcTxRowM(100, 5, "1.00000000", 1, "2026-09-01 00:00:00", "aa11recv")},
		spendAgg:  btcAggRowM("1.00000000", "1.00000000", 1, "2026-09-05 00:00:00", "2026-09-05 00:00:00", "2026-09-05 00:00:00", "2026-09-05 00:00:00"),
		spendTx:   []map[string]any{btcTxRowM(200, 3, "1.00000000", 1, "2026-09-05 00:00:00", "")},
		inputs: map[btcTxKey][]map[string]any{
			{Block: 100, TxIndex: 5}: {
				btcPartRowM(100, 5, btcExch, "0.60000000", "1.00000000", ""),
				btcPartRowM(100, 5, btcPlain, "0.40000000", "1.00000000", ""),
			},
		},
		outputs: map[btcTxKey][]map[string]any{
			{Block: 200, TxIndex: 3}: {
				btcPartRowM(200, 3, btcSanc, "0.70000000", "1.00000000", "bb22sent"),
				btcPartRowM(200, 3, btcTarget, "0.29000000", "1.00000000", "bb22sent"),
			},
		},
	}
}

func btcSampleLabels() *fakeLabels {
	return &fakeLabels{rows: map[string][]map[string]any{
		btcExch: {{"label_type": "money-laundering", "label_value": "ml-entity", "source": "vendor-x", "recorded_at": "2026-01-01 00:00:00"}},
		btcSanc: {{"label_type": "sanctioned-entity", "label_value": "sanctioned-evilcorp", "source": "ofac", "recorded_at": "2026-01-01 00:00:00"}},
	}}
}

// A direct-view Bitcoin report end to end: received + sent counterparties, one
// labelled and one unnamed, the score and both output formats.
func TestBitcoinDirectReport(t *testing.T) {
	btc := btcSampleChain()
	tool := newBtcTestTool(t, btc, btcSampleLabels())
	res, err := tool.Invoke(context.Background(), btc, mustParams(t, tool, map[string]any{"chain": "bitcoin", "address": btcTarget}), "")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := decode(t, res)

	sum := out["summary"].(map[string]any)
	if sum["chain"] != "bitcoin" || sum["received"] != "1.00000000" || sum["sent"] != "1.00000000" || sum["balance"] != "0.00000000" {
		t.Errorf("summary: %v", sum)
	}
	if sum["asset"].(map[string]any)["symbol"] != "BTC" {
		t.Errorf("asset not BTC: %v", sum["asset"])
	}

	// Connections: Illegal Service (btcExch, received 0.6) and Sanctions (btcSanc, sent 0.7).
	conns := map[string]map[string]any{}
	for _, c := range out["connections"].([]any) {
		cm := c.(map[string]any)
		conns[cm["entity_type"].(string)] = cm
	}
	if il := conns["Illegal Service"]; il == nil || il["received"] != "0.60000000" {
		t.Errorf("Illegal Service connection: %v", conns)
	}
	if sc := conns["Sanctions"]; sc == nil || sc["sent"] != "0.70000000" {
		t.Errorf("Sanctions connection: %v", conns)
	}

	// Unnamed: btcPlain, received 0.4, carried into unattributed.
	un := out["unnamed_connections"].([]any)
	if len(un) != 1 || un[0].(map[string]any)["address"] != btcPlain || un[0].(map[string]any)["received"] != "0.40000000" {
		t.Errorf("unnamed connections: %v", un)
	}
	if ua := out["unattributed"].(map[string]any); ua["received"] != "0.40000000" || ua["sent"] != "0.00000000" {
		t.Errorf("unattributed: %v", ua)
	}

	// Score: a sanctions destination above dust sets the direct floor to 100.
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

	// Labels were fetched for the Bitcoin counterparties (the Tron-address guard
	// no longer drops them): the statement names chain = 'bitcoin' and the addresses.
	labelStmt := strings.Join(tool.labels.(*fakeLabels).statements, "\n")
	if !strings.Contains(labelStmt, "chain = 'bitcoin'") || !strings.Contains(labelStmt, "'"+btcExch+"'") || !strings.Contains(labelStmt, "'"+btcSanc+"'") {
		t.Errorf("labels not fetched for bitcoin counterparties: %q", labelStmt)
	}
	// The labels statement goes to the directory (modern engine), so it keeps the
	// client-cancel setting the Bitcoin gateway statements must not carry.
	if !strings.Contains(labelStmt, "cancel_http_readonly_queries_on_client_close") {
		t.Errorf("labels statement lost its modern settings: %q", labelStmt)
	}

	assertBtcStatements(t, btc)

	// markdown (the default) renders as one text document for the same report.
	res2, err := tool.Invoke(context.Background(), btc, mustParams(t, tool, map[string]any{"chain": "bitcoin", "address": btcTarget, "format": "markdown"}), "")
	if err != nil {
		t.Fatalf("markdown Invoke: %v", err)
	}
	items, structured := tools.SplitResult(res2)
	if structured != nil || len(items) != 1 {
		t.Fatalf("markdown: %d items, structured %v", len(items), structured)
	}
	md := items[0].(tools.TextResult).Text
	if !strings.Contains(md, "# Address risk report") || !strings.Contains(md, "Chain: bitcoin") {
		t.Errorf("markdown document:\n%s", md)
	}
}

// assertBtcStatements checks every Bitcoin gateway statement is 20.8-safe and
// bounded: no client-cancel setting, no modern JSON/date settings, the row /
// result / time caps present, FORMAT JSON, and no set built from a subquery.
func assertBtcStatements(t *testing.T, btc *fakeBtcChain) {
	t.Helper()
	if len(btc.statements) == 0 {
		t.Fatal("no bitcoin statements were sent")
	}
	for _, s := range btc.statements {
		if strings.Contains(s, "cancel_http_readonly_queries_on_client_close") {
			t.Errorf("bitcoin statement carries the client-cancel setting:\n%s", s)
		}
		if strings.Contains(s, "date_time_output_format") || strings.Contains(s, "output_format_json_array_of_rows") {
			t.Errorf("bitcoin statement carries a setting the engine rejects:\n%s", s)
		}
		for _, want := range []string{"max_rows_to_read", "max_result_rows", "max_execution_time"} {
			if !strings.Contains(s, want) {
				t.Errorf("bitcoin statement missing %q:\n%s", want, s)
			}
		}
		if !strings.HasSuffix(strings.TrimSpace(s), "FORMAT JSON") {
			t.Errorf("bitcoin statement does not end FORMAT JSON:\n%s", s)
		}
		if strings.Contains(s, "IN (SELECT") {
			t.Errorf("bitcoin statement builds a set from a subquery:\n%s", s)
		}
		// The sample's transaction reads are each for one transaction, so none may
		// use the one-element tuple IN the older engine misreads.
		if strings.Contains(s, "IN ((") {
			t.Errorf("bitcoin statement uses a one-element tuple IN:\n%s", s)
		}
	}
}

// The hop-1 attribution conserves value in exact satoshi: a receipt's parts add
// up to the receipt, a spend's parts (including the dropped change and fee) add
// up to the spend.
func TestBitcoinAttributionConserves(t *testing.T) {
	sat := func(btc int64) *big.Int { return big.NewInt(btc) }
	sumParts := func(m map[string]*big.Int) *big.Int {
		s := new(big.Int)
		for _, v := range m {
			s.Add(s, v)
		}
		return s
	}

	// Received: 1 BTC split 0.6 / 0.4 by input value — exact.
	v := sat(100000000)
	rec := attributeReceipt(v, []btcPart{{btcExch, sat(60000000)}, {btcPlain, sat(40000000)}})
	if rec[btcExch].Cmp(sat(60000000)) != 0 || rec[btcPlain].Cmp(sat(40000000)) != 0 || sumParts(rec).Cmp(v) != 0 {
		t.Errorf("received split: %v", rec)
	}

	// Received with a remainder: 1 satoshi over three equal inputs still sums to v.
	v2 := sat(100000000)
	rec2 := attributeReceipt(v2, []btcPart{{btcExch, sat(1)}, {btcPlain, sat(1)}, {btcSanc, sat(1)}})
	if sumParts(rec2).Cmp(v2) != 0 {
		t.Errorf("received split with remainder does not conserve: %v (sum %s)", rec2, sumParts(rec2))
	}

	// Sent: 1 BTC input, outputs 0.7 + 0.29 change + 0.01 fee. The counterparty
	// gets 0.7, the change and fee are split off but the parts still sum to the input.
	a := sat(100000000)
	spd := attributeSpend(a, sat(100000000), []btcPart{{btcSanc, sat(70000000)}, {btcTarget, sat(29000000)}})
	if spd[btcSanc].Cmp(sat(70000000)) != 0 || spd[btcTarget].Cmp(sat(29000000)) != 0 || spd[btcFeeKey].Cmp(sat(1000000)) != 0 {
		t.Errorf("sent split: %v", spd)
	}
	if sumParts(spd).Cmp(a) != 0 {
		t.Errorf("sent split does not conserve: %v (sum %s, want %s)", spd, sumParts(spd), a)
	}

	// A coinbase receipt (no inputs) attributes nothing.
	if got := attributeReceipt(sat(5000000000), nil); len(got) != 0 {
		t.Errorf("coinbase receipt attributed: %v", got)
	}
}

// A single transaction must be selected by an equality, not a one-element tuple
// IN, which the older engine reads as a bare tuple; several use the tuple set.
func TestBitcoinTxWhere(t *testing.T) {
	one := btcTxWhere([]btcTxKey{{Block: 100, TxIndex: 5}})
	if strings.Contains(one, "IN (") || one != "block = 100 AND tx_index = 5" {
		t.Errorf("single-key predicate must be an equality, not a one-element IN: %q", one)
	}
	many := btcTxWhere([]btcTxKey{{Block: 100, TxIndex: 5}, {Block: 200, TxIndex: 3}, {Block: 100, TxIndex: 9}})
	if !strings.Contains(many, "block IN (100, 200)") || !strings.Contains(many, "(block, tx_index) IN ((100, 5), (200, 3), (100, 9))") {
		t.Errorf("multi-key predicate shape: %q", many)
	}
}

// A non-BTC asset is refused on bitcoin before any statement runs; a Tron
// address on chain bitcoin is still refused (it fails the BTC regex). view all
// is NOT refused (it runs the direct view — see TestBitcoinViewDefaultRunsDirect).
func TestBitcoinRefusals(t *testing.T) {
	for _, p := range []map[string]any{
		{"chain": "bitcoin", "address": btcTarget, "asset": "USDT"},
		{"chain": "bitcoin", "address": at5Target}, // a Tron address on bitcoin
		{"chain": "bitcoin", "address": "not an address"},
	} {
		btc := btcSampleChain()
		tool := newBtcTestTool(t, btc, btcSampleLabels())
		if _, err := tool.Invoke(context.Background(), btc, mustParams(t, tool, p), ""); err == nil {
			t.Errorf("accepted %v", p)
		}
		if len(btc.statements) != 0 {
			t.Errorf("a refused call %v ran %d statements", p, len(btc.statements))
		}
	}

	// asset: BTC (the obvious spelling) is accepted.
	btc := btcSampleChain()
	tool := newBtcTestTool(t, btc, btcSampleLabels())
	if _, err := tool.Invoke(context.Background(), btc, mustParams(t, tool, map[string]any{"chain": "bitcoin", "address": btcTarget, "asset": "BTC"}), ""); err != nil {
		t.Errorf("asset BTC refused: %v", err)
	}
}

// A bare bitcoin call (view defaults to all) runs the direct view and says so,
// rather than failing: params.view is direct and a note explains it.
func TestBitcoinViewDefaultRunsDirect(t *testing.T) {
	btc := btcSampleChain()
	tool := newBtcTestTool(t, btc, btcSampleLabels())
	res, err := tool.Invoke(context.Background(), btc, mustParams(t, tool, map[string]any{"chain": "bitcoin", "address": btcTarget}), "")
	if err != nil {
		t.Fatalf("bare bitcoin call failed: %v", err)
	}
	out := decode(t, res)
	p := out["params"].(map[string]any)
	if p["view"] != "direct" {
		t.Errorf("params.view = %v, want direct", p["view"])
	}
	if ag := out["at_a_glance"].(map[string]any); ag["view"] != "direct" {
		t.Errorf("at_a_glance.view = %v, want direct", ag["view"])
	}
	noted := false
	for _, n := range p["notes"].([]any) {
		if strings.Contains(n.(string), "multi-hop tracing is not available on bitcoin") {
			noted = true
		}
	}
	if !noted {
		t.Errorf("missing the multi-hop-not-available note: %v", p["notes"])
	}
}

// btcLimitErr is a gateway limit error with a ClickHouse code (396 result cap,
// 158 row cap).
func btcLimitErr(code int, name string) error {
	return util.BitqueryDatabaseErrorResponse(context.Background(), "fake", 500,
		[]byte(fmt.Sprintf("Code: %d. DB::Exception: Limit exceeded. (%s) (version 20.8.7.15)", code, name)))
}

// A big address: the inputs/outputs read is refused for the gateway's result cap
// (code 396). The whole-history summary is still correct (from the lifetime
// table) and the counterparties degrade to a cut with complete:false — never a
// hard error.
func TestBitcoinBigAddressDegradesToCut(t *testing.T) {
	btc := btcSampleChain()
	btc.partsErr = btcLimitErr(396, "TOO_MANY_ROWS_OR_BYTES")
	tool := newBtcTestTool(t, btc, btcSampleLabels())
	res, err := tool.Invoke(context.Background(), btc, mustParams(t, tool, map[string]any{"chain": "bitcoin", "address": btcTarget}), "")
	if err != nil {
		t.Fatalf("a big address errored instead of degrading: %v", err)
	}
	out := decode(t, res)
	sum := out["summary"].(map[string]any)
	if sum["received"] != "1.00000000" || sum["sent"] != "1.00000000" {
		t.Errorf("summary not correct from the lifetime table: %v", sum)
	}
	if out["params"].(map[string]any)["complete"] != false {
		t.Errorf("complete = %v, want false on a degraded report", out["params"].(map[string]any)["complete"])
	}
	cut := false
	for _, c := range out["params"].(map[string]any)["cuts"].([]any) {
		if c.(map[string]any)["reason"] == "counterparty_limit" {
			cut = true
		}
	}
	if !cut {
		t.Errorf("no counterparty_limit cut on a degraded report: %v", out["params"].(map[string]any)["cuts"])
	}
	// No counterparties were attributable (both sides cut), so there is none.
	if conns := out["connections"].([]any); len(conns) != 0 {
		t.Errorf("connections built despite a cut read: %v", conns)
	}
}

// A windowed report on an address too large to total refuses honestly rather
// than returning a wrong/empty summary or a 500.
func TestBitcoinWindowedTooBigRefuses(t *testing.T) {
	btc := btcSampleChain()
	btc.aggErr = btcLimitErr(158, "TOO_MANY_ROWS")
	tool := newBtcTestTool(t, btc, btcSampleLabels())
	_, err := tool.Invoke(context.Background(), btc, mustParams(t, tool,
		map[string]any{"chain": "bitcoin", "address": btcTarget, "before_time": "2026-09-10"}), "")
	if err == nil {
		t.Fatalf("a windowed report on a huge address did not refuse")
	}
	if !strings.Contains(err.Error(), "too much history for a windowed") {
		t.Errorf("refusal message = %q, want the windowed-too-big message", err.Error())
	}
}

// A windowed report (before_time in the past) sums the bounded reads for its
// summary and builds counterparties normally.
func TestBitcoinWindowedSummary(t *testing.T) {
	btc := btcSampleChain()
	tool := newBtcTestTool(t, btc, btcSampleLabels())
	res, err := tool.Invoke(context.Background(), btc, mustParams(t, tool,
		map[string]any{"chain": "bitcoin", "address": btcTarget, "before_time": "2026-09-10"}), "")
	if err != nil {
		t.Fatalf("windowed Invoke: %v", err)
	}
	out := decode(t, res)
	if sum := out["summary"].(map[string]any); sum["received"] != "1.00000000" || sum["sent"] != "1.00000000" {
		t.Errorf("windowed summary from the bounded reads: %v", sum)
	}
	// The lifetime table must NOT be read for a windowed report.
	for _, s := range btc.statements {
		if strings.Contains(s, "address_transfers") {
			t.Errorf("a windowed report read the lifetime table:\n%s", s)
		}
	}
}

// The inputs/outputs read sets a result cap under the gateway's hard limit and
// its chunk is small enough to stay there.
func TestBitcoinPartsStayUnderCap(t *testing.T) {
	if btcPartsResultCap >= 25000 {
		t.Errorf("btcPartsResultCap %d is not under the gateway's ~25000 cap", btcPartsResultCap)
	}
	if btcPartsChunk > 400 {
		t.Errorf("btcPartsChunk %d is too large to stay under the result cap", btcPartsChunk)
	}
	keys := make([]btcTxKey, btcPartsChunk)
	for i := range keys {
		keys[i] = btcTxKey{Block: uint64(i), TxIndex: 1}
	}
	stmt := btcTxPartsStatement(btcTxInputs, keys, false, 1000, btcPartsResultCap, 30)
	if !strings.Contains(stmt, fmt.Sprintf("max_result_rows = %d", btcPartsResultCap)) {
		t.Errorf("parts statement does not carry the result cap:\n%s", stmt)
	}
}

// A side that reads several chunks and then hits a refused one keeps the
// counterparties it already read, and reports the rest as a cut — it does not
// drop everything on the error. The receipts span two chunks; the chunk holding
// the last transaction is refused, so the first btcPartsChunk are kept.
func TestBitcoinPartialSideKeepsReadChunks(t *testing.T) {
	const n = btcPartsChunk + 1
	recvTx := make([]map[string]any, n)
	inputs := map[btcTxKey][]map[string]any{}
	for i := 0; i < n; i++ {
		blk := uint64(1000 + i)
		recvTx[i] = btcTxRowM(blk, 1, "1.00000000", 1, "2026-09-01 00:00:00", fmt.Sprintf("tx%d", i))
		// Every receipt is funded by the same labelled address, so one connection
		// accrues the read transactions.
		inputs[btcTxKey{Block: blk, TxIndex: 1}] = []map[string]any{btcPartRowM(blk, 1, btcExch, "1.00000000", "1.00000000", "")}
	}
	lastKey := btcTxKey{Block: uint64(1000 + n - 1), TxIndex: 1}
	btc := &fakeBtcChain{
		price:     "100000",
		addrXfers: btcXfersRowM("301.00000000", "0.00000000", "2026-09-01 00:00:00", "2026-09-01 00:00:00", "", ""),
		recvTx:    recvTx,
		inputs:    inputs,
		errKey:    &lastKey,
		errKeyErr: btcLimitErr(396, "TOO_MANY_ROWS_OR_BYTES"),
	}
	tool := newBtcTestTool(t, btc, btcSampleLabels())
	res, err := tool.Invoke(context.Background(), btc, mustParams(t, tool, map[string]any{"chain": "bitcoin", "address": btcTarget}), "")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := decode(t, res)
	// The read chunk (btcPartsChunk transactions) is attributed to the labelled
	// connection; the refused chunk is a cut.
	var il map[string]any
	for _, c := range out["connections"].([]any) {
		if cm := c.(map[string]any); cm["entity_type"] == "Illegal Service" {
			il = cm
		}
	}
	if il == nil || il["received"] != fmt.Sprintf("%d.00000000", btcPartsChunk) {
		t.Errorf("read chunk not kept as a connection: %v", out["connections"])
	}
	if out["params"].(map[string]any)["complete"] != false {
		t.Errorf("complete = %v, want false (a chunk was refused)", out["params"].(map[string]any)["complete"])
	}
}

// When the Bitcoin price read lands on an earlier hour than the pin (the
// top-of-hour slot was briefly absent and the statement fell back to the newest
// present hour), the price still applies and usd_conversion reports the hour the
// price is actually as of, not the pin. The statement also carries the
// hour-walk-back shape, so a refactor that drops the fallback is caught.
func TestBtcPriceAsOfFallback(t *testing.T) {
	btc := btcSampleChain()
	btc.priceAsOf = "2026-09-05T00:00:00Z"
	tool := newBtcTestTool(t, btc, btcSampleLabels())
	res, err := tool.Invoke(context.Background(), btc, mustParams(t, tool, map[string]any{"chain": "bitcoin", "address": btcTarget}), "")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := decode(t, res)
	if usd, _ := out["params"].(map[string]any)["usd_conversion"].(string); usd != "store: 1 BTC = 100000 USD (as of 2026-09-05T00:00:00Z)" {
		t.Errorf("usd_conversion = %q, want the fallback hour", usd)
	}
	var priceStmt string
	for _, s := range btc.statements {
		if strings.Contains(s, "dictHas('market_price'") {
			priceStmt = s
		}
	}
	if !strings.Contains(priceStmt, "arrayJoin(range(") || !strings.Contains(priceStmt, "ORDER BY h DESC") {
		t.Errorf("btc price statement is not the hour-fallback shape:\n%s", priceStmt)
	}
}
