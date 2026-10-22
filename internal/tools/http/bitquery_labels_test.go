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

package http

// Bitquery: bitqueryLabels must add the labels of every address in the result
// without moving or changing any value the statement returned, look each
// address up once, in literal batches, and leave the data whole when the
// lookup fails.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	nethttp "net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/googleapis/mcp-toolbox/internal/log"
	"github.com/googleapis/mcp-toolbox/internal/sources"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

var (
	lbEvmA    = "0x" + strings.Repeat("a", 40)
	lbEvmB    = "0x" + strings.Repeat("b", 40)
	lbEvmC    = "0x" + strings.Repeat("c", 40)
	lbEvmSelf = "0x" + strings.Repeat("1", 40)
)

const (
	lbBtcHot  = "bc1qxy2kgdygjrsqtzq2n0yrf2493p83kkfjhx0wlh"
	lbBtcPool = "1BoatSLRHtKNngkdXEeobR76b53LETtpyT"
	lbBtcSelf = "3J98t1WpEZ73CNmQviecrnyiWrnqRhWNLy"
)

// lbFakeLabels stands in for the clickhouse labels source: it answers a label
// statement from a fixed table, honouring the statement's address and chain
// lists, and records what it was sent.
type lbFakeLabels struct {
	mu         sync.Mutex
	statements []string
	// queryIDs records, per statement, whether the call's context carried the
	// caller identity the clickhouse source builds its query_id from.
	queryIDs []bool
	labels   map[string][][3]string // address -> (chain, label_type, label_value)
	err      error
	block    bool
}

func (f *lbFakeLabels) SourceType() string             { return "clickhouse" }
func (f *lbFakeLabels) ToConfig() sources.SourceConfig { return nil }

var (
	lbAddressListRe = regexp.MustCompile(`address IN \(([^)]*)\)`)
	lbChainListRe   = regexp.MustCompile(`chain IN \(([^)]*)\)`)
)

func lbList(re *regexp.Regexp, statement string) []string {
	m := re.FindStringSubmatch(statement)
	if m == nil {
		return nil
	}
	var out []string
	for _, q := range strings.Split(m[1], ", ") {
		out = append(out, strings.Trim(q, "'"))
	}
	return out
}

func (f *lbFakeLabels) RunSQL(ctx context.Context, statement string, _ parameters.ParamValues) (any, error) {
	_, hasID := util.BitqueryClickhouseQueryID(ctx)
	f.mu.Lock()
	f.statements = append(f.statements, statement)
	f.queryIDs = append(f.queryIDs, hasID)
	f.mu.Unlock()
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.err != nil {
		return nil, f.err
	}
	chains := lbList(lbChainListRe, statement)
	rows := []any{}
	for _, a := range lbList(lbAddressListRe, statement) {
		for _, l := range f.labels[a] {
			if slices.Contains(chains, l[0]) {
				rows = append(rows, map[string]any{"address": a, "chain": l[0], "label_type": l[1], "label_value": l[2]})
			}
		}
	}
	return rows, nil
}

func (f *lbFakeLabels) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.statements...)
}

// lbFixture is a data backend answering one fixed body, the http source over
// it, and the labels source.
type lbFixture struct {
	ctx    context.Context
	logs   *bytes.Buffer
	data   *bitqueryBackend
	body   string
	labels *lbFakeLabels
	srcs   tools.SourceMap
}

func newLBFixture(t *testing.T, body string) *lbFixture {
	t.Helper()
	logs := &bytes.Buffer{}
	logger, err := log.NewLogger("standard", log.Info, logs, logs)
	if err != nil {
		t.Fatalf("failed to create logger: %v", err)
	}
	f := &lbFixture{ctx: util.WithLogger(context.Background(), logger), logs: logs, body: body, data: &bitqueryBackend{}}
	f.data.server = httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		got, _ := io.ReadAll(r.Body)
		f.data.mu.Lock()
		f.data.hits = append(f.data.hits, bitqueryHit{body: string(got), query: r.URL.RawQuery})
		f.data.mu.Unlock()
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.data.server.Close)
	f.labels = &lbFakeLabels{labels: map[string][][3]string{
		lbEvmA: {
			{"ethereum", "token-contract", "X"},
			{"ethereum", "cex-hot-wallet", "Binance"},
			{"bsc", "cex-hot-wallet", "Binance"},
			{"tron", "scam", "not read: the chain is not listed"},
		},
		lbEvmC:    {{"ethereum", "dex-router", "Uniswap"}},
		lbEvmSelf: {{"ethereum", "cex-deposit-address", "Kraken"}},
		lbBtcHot:  {{"bitcoin", "cex-hot-wallet", "Kraken"}},
		lbBtcPool: {{"bitcoin", "mixer", "Bar"}, {"bitcoin", "mining-pool", "Foo"}},
		lbBtcSelf: {{"bitcoin", "cex-deposit-address", "Bybit"}},
	}}
	f.srcs = tools.SourceMap{
		"gw-eth":            bitqueryRoutesSource(t, f.ctx, "gw-eth", f.data.server.URL, nil, ""),
		"clickhouse-labels": f.labels,
	}
	return f
}

func (f *lbFixture) tool(t *testing.T, cfg Config) Tool {
	t.Helper()
	tl, err := cfg.Initialize(tools.WithSourceLookup(f.ctx, f.srcs))
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return tl.(Tool)
}

func (f *lbFixture) invokeCtx(t *testing.T, ctx context.Context, tl Tool, args map[string]any) []any {
	t.Helper()
	params, err := parameters.ParseParams(tl.StaticParameters, args, nil)
	if err != nil {
		t.Fatalf("parse params: %v", err)
	}
	res, terr := tl.Invoke(ctx, f.srcs["gw-eth"], params, "")
	if terr != nil {
		t.Fatalf("invoke: %v", terr)
	}
	rows, ok := res.([]any)
	if !ok {
		t.Fatalf("result is %T, want rows", res)
	}
	return rows
}

func (f *lbFixture) invoke(t *testing.T, tl Tool, args map[string]any) []any {
	t.Helper()
	return f.invokeCtx(t, f.ctx, tl, args)
}

// decoded is the fixture body as the http source decodes it.
func (f *lbFixture) decoded(t *testing.T) []any {
	t.Helper()
	var v []any
	if err := json.Unmarshal([]byte(f.body), &v); err != nil {
		t.Fatalf("bad fixture body: %v", err)
	}
	return v
}

func lbToolConfig(labels *LabelsConfig, addressParam ...parameters.Parameter) Config {
	params := parameters.Parameters{parameters.NewStringParameter("address", "address")}
	if len(addressParam) > 0 {
		params = parameters.Parameters(addressParam)
	}
	return Config{
		ConfigBase:  tools.ConfigBase{Name: "flow_summary", Description: "flow summary"},
		Type:        resourceType,
		Source:      "gw-eth",
		Path:        "/",
		Method:      "POST",
		RequestBody: "SELECT {{.address}}",
		BodyParams:  params,
		Labels:      labels,
	}
}

func lbEVMLabels() *LabelsConfig {
	return &LabelsConfig{
		Source: "clickhouse-labels",
		Chains: []string{"ethereum", "bsc"},
		Fields: []LabelsField{
			{Column: "Top_Receivers", Key: "counterparty"},
			{Column: "Top_Senders", Key: "counterparty"},
		},
		AddressParam:  "address",
		AddressColumn: "Address_Labels",
		Priority:      []string{"sanctioned-entity", "cex-hot-wallet", "cex-deposit-address", "dex-router"},
	}
}

func lbBTCLabels() *LabelsConfig {
	return &LabelsConfig{
		Source: "clickhouse-labels",
		Chains: []string{"bitcoin"},
		Fields: []LabelsField{
			{Column: "Top_Receivers", NamesColumn: "Top_Fields", Key: "counterparty"},
			{Column: "Top_Senders", NamesColumn: "Top_Fields", Key: "counterparty"},
		},
		AddressParam:  "address",
		AddressColumn: "Address_Labels",
		Priority:      []string{"sanctioned-entity", "cex-hot-wallet", "cex-deposit-address"},
	}
}

func lbEntry(a string, amount int) string {
	return fmt.Sprintf(`{"counterparty":"%s","amount":%d.5,"currency":"USDT","symbol_nonascii":0,"contract":"0xdac17f958d2ee523a2206206994597c13d831ec7","transfers":2}`, a, amount)
}

func lbEVMBody(receivers, senders []string) string {
	var r, s []string
	for i, a := range receivers {
		r = append(r, lbEntry(a, 10-i))
	}
	for i, a := range senders {
		s = append(s, lbEntry(a, 5-i))
	}
	return `[{"Sent_Transfers":3,"Received_Transfers":2,"Role":"relay","Top_Receivers":[` + strings.Join(r, ",") +
		`],"Top_Senders":[` + strings.Join(s, ",") + `]}]`
}

const lbBTCBody = `[{"Role":"relay",` +
	`"Top_Fields":["counterparty","amount","txs","pro_rata_amount"],` +
	`"Top_Receivers":[["` + lbBtcHot + `",0.5,3,0.25],["m-a1b2c3",0.1,1,0.1]],` +
	`"Top_Senders":[["` + lbBtcPool + `",2,1,2]]}]`

func lbRow(t *testing.T, rows []any) map[string]any {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	return rows[0].(map[string]any)
}

func lbObj(row map[string]any, column string, i int) map[string]any {
	return row[column].([]any)[i].(map[string]any)
}

func lbStrings(v ...string) []any {
	out := make([]any, len(v))
	for i, s := range v {
		out[i] = s
	}
	return out
}

func lbCheckEntry(t *testing.T, where string, labels, label, status any) func(got [3]any) {
	return func(got [3]any) {
		t.Helper()
		want := [3]any{labels, label, status}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %#v, want %#v", where, got, want)
		}
	}
}

func lbObjValues(o map[string]any) [3]any {
	return [3]any{o["labels"], o["label"], o["label_status"]}
}

func TestBitqueryLabelsObjectEntries(t *testing.T) {
	f := newLBFixture(t, lbEVMBody([]string{lbEvmA, lbEvmB}, []string{lbEvmC, lbEvmA}))
	tl := f.tool(t, lbToolConfig(lbEVMLabels()))
	row := lbRow(t, f.invoke(t, tl, map[string]any{"address": lbEvmSelf}))

	binance := lbStrings("bsc/cex-hot-wallet:Binance", "ethereum/cex-hot-wallet:Binance", "ethereum/token-contract:X")
	// cex-hot-wallet ranks before token-contract (not listed); the two Binance
	// labels differ only by chain, and bsc sorts first.
	lbCheckEntry(t, "receiver 0", binance, "bsc/cex-hot-wallet:Binance", "labeled")(lbObjValues(lbObj(row, "Top_Receivers", 0)))
	lbCheckEntry(t, "receiver 1", []any{}, nil, "none")(lbObjValues(lbObj(row, "Top_Receivers", 1)))
	lbCheckEntry(t, "sender 0", lbStrings("ethereum/dex-router:Uniswap"), "ethereum/dex-router:Uniswap", "labeled")(lbObjValues(lbObj(row, "Top_Senders", 0)))
	lbCheckEntry(t, "sender 1", binance, "bsc/cex-hot-wallet:Binance", "labeled")(lbObjValues(lbObj(row, "Top_Senders", 1)))
	if got := row["Address_Labels"]; !reflect.DeepEqual(got, lbStrings("ethereum/cex-deposit-address:Kraken")) {
		t.Errorf("Address_Labels = %#v", got)
	}

	// Everything the statement returned is still there, unchanged.
	want := lbRow(t, f.decoded(t))
	for k, v := range want {
		if k == "Top_Receivers" || k == "Top_Senders" {
			continue
		}
		if !reflect.DeepEqual(row[k], v) {
			t.Errorf("column %s = %#v, want %#v", k, row[k], v)
		}
	}
	for _, col := range []string{"Top_Receivers", "Top_Senders"} {
		for i, e := range want[col].([]any) {
			got := lbObj(row, col, i)
			for k, v := range e.(map[string]any) {
				if !reflect.DeepEqual(got[k], v) {
					t.Errorf("%s[%d].%s = %#v, want %#v", col, i, k, got[k], v)
				}
			}
			if len(got) != len(e.(map[string]any))+3 {
				t.Errorf("%s[%d] has %d keys, want the original %d plus 3", col, i, len(got), len(e.(map[string]any)))
			}
		}
	}

	if n := len(f.labels.sent()); n != 1 {
		t.Fatalf("label statements = %d, want 1", n)
	}
}

func TestBitqueryLabelsPositionalEntries(t *testing.T) {
	f := newLBFixture(t, lbBTCBody)
	tl := f.tool(t, lbToolConfig(lbBTCLabels()))
	row := lbRow(t, f.invoke(t, tl, map[string]any{"address": lbBtcSelf}))

	// The names column is extended once, although two columns use it.
	wantNames := lbStrings("counterparty", "amount", "txs", "pro_rata_amount", "labels", "label", "label_status")
	if !reflect.DeepEqual(row["Top_Fields"], wantNames) {
		t.Fatalf("Top_Fields = %#v, want %#v", row["Top_Fields"], wantNames)
	}
	want := lbRow(t, f.decoded(t))
	check := func(col string, i int, labels, label, status any) {
		t.Helper()
		got := row[col].([]any)[i].([]any)
		orig := want[col].([]any)[i].([]any)
		if len(got) != len(orig)+3 {
			t.Fatalf("%s[%d] = %#v: want the original %d positions plus 3", col, i, got, len(orig))
		}
		if !reflect.DeepEqual(got[:len(orig)], orig) {
			t.Errorf("%s[%d]: existing positions changed: %#v, want %#v", col, i, got[:len(orig)], orig)
		}
		lbCheckEntry(t, col, labels, label, status)([3]any{got[4], got[5], got[6]})
	}
	check("Top_Receivers", 0, lbStrings("bitcoin/cex-hot-wallet:Kraken"), "bitcoin/cex-hot-wallet:Kraken", "labeled")
	// A script placeholder is not an address: it is never sent and reads none.
	check("Top_Receivers", 1, []any{}, nil, "none")
	// Neither type is listed: the pick goes by type name.
	check("Top_Senders", 0, lbStrings("bitcoin/mining-pool:Foo", "bitcoin/mixer:Bar"), "bitcoin/mining-pool:Foo", "labeled")
	if got := row["Address_Labels"]; !reflect.DeepEqual(got, lbStrings("bitcoin/cex-deposit-address:Bybit")) {
		t.Errorf("Address_Labels = %#v", got)
	}
	if row["Role"] != "relay" {
		t.Errorf("Role = %#v", row["Role"])
	}

	sent := f.labels.sent()
	if len(sent) != 1 || strings.Contains(sent[0], "m-a1b2c3") {
		t.Fatalf("statements = %q, want one without the placeholder", sent)
	}
}

// Every row of a result is labelled from the one lookup: its own names column
// extended once, and the address column on each row.
func TestBitqueryLabelsSeveralRows(t *testing.T) {
	body := `[{"Top_Fields":["counterparty","amount"],"Top_Receivers":[["` + lbBtcHot + `",1]]},` +
		`{"Top_Fields":["counterparty","amount"],"Top_Receivers":[["` + lbBtcPool + `",2]],"Top_Senders":[["` + lbBtcHot + `",3]]}]`
	f := newLBFixture(t, body)
	rows := f.invoke(t, f.tool(t, lbToolConfig(lbBTCLabels())), map[string]any{"address": lbBtcSelf})
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	wantNames := lbStrings("counterparty", "amount", "labels", "label", "label_status")
	for i, r := range rows {
		row := r.(map[string]any)
		if !reflect.DeepEqual(row["Top_Fields"], wantNames) {
			t.Errorf("row %d: Top_Fields = %#v", i, row["Top_Fields"])
		}
		if !reflect.DeepEqual(row["Address_Labels"], lbStrings("bitcoin/cex-deposit-address:Bybit")) {
			t.Errorf("row %d: Address_Labels = %#v", i, row["Address_Labels"])
		}
	}
	second := rows[1].(map[string]any)
	if got := second["Top_Receivers"].([]any)[0].([]any); len(got) != 5 || got[3] != "bitcoin/mining-pool:Foo" {
		t.Errorf("row 1 receiver = %#v", got)
	}
	if got := second["Top_Senders"].([]any)[0].([]any); len(got) != 5 || got[4] != "labeled" {
		t.Errorf("row 1 sender = %#v", got)
	}
	if n := len(f.labels.sent()); n != 1 {
		t.Errorf("label statements = %d, want 1 for the whole result", n)
	}
}

// A names column that already names one of the three values (Solana's carries
// an inline label) keeps it: the value is neither overwritten nor named twice,
// unless outputNames renames the pick.
func TestBitqueryLabelsNameAlreadyTaken(t *testing.T) {
	body := `[{"Top_Fields":["counterparty","label","amount"],"Top_Receivers":[["` + lbBtcHot + `","Inline",1]]}]`
	cfg := lbBTCLabels()
	cfg.Fields = cfg.Fields[:1]
	cfg.AddressParam, cfg.AddressColumn = "", ""

	f := newLBFixture(t, body)
	row := lbRow(t, f.invoke(t, f.tool(t, lbToolConfig(cfg)), map[string]any{"address": lbBtcSelf}))
	if want := lbStrings("counterparty", "label", "amount", "labels", "label_status"); !reflect.DeepEqual(row["Top_Fields"], want) {
		t.Fatalf("Top_Fields = %#v, want %#v", row["Top_Fields"], want)
	}
	got := row["Top_Receivers"].([]any)[0].([]any)
	want := []any{lbBtcHot, "Inline", 1.0, lbStrings("bitcoin/cex-hot-wallet:Kraken"), "labeled"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entry = %#v, want %#v", got, want)
	}
	if !strings.Contains(f.logs.String(), "WARN") || !strings.Contains(f.logs.String(), "already names") {
		t.Errorf("no warning about the taken name: %s", f.logs.String())
	}

	cfg.OutputNames = &LabelsOutputNames{Label: "label_pick"}
	f = newLBFixture(t, body)
	row = lbRow(t, f.invoke(t, f.tool(t, lbToolConfig(cfg)), map[string]any{"address": lbBtcSelf}))
	if want := lbStrings("counterparty", "label", "amount", "labels", "label_pick", "label_status"); !reflect.DeepEqual(row["Top_Fields"], want) {
		t.Fatalf("renamed: Top_Fields = %#v, want %#v", row["Top_Fields"], want)
	}
	if got := row["Top_Receivers"].([]any)[0].([]any); len(got) != 6 || got[1] != "Inline" || got[4] != "bitcoin/cex-hot-wallet:Kraken" {
		t.Fatalf("renamed: entry = %#v", got)
	}

	// An object entry keeps a key it already has.
	f = newLBFixture(t, `[{"Top_Senders":[{"counterparty":"`+lbEvmC+`","label":"Inline"}]}]`)
	ecfg := lbEVMLabels()
	row = lbRow(t, f.invoke(t, f.tool(t, lbToolConfig(ecfg)), map[string]any{"address": lbEvmSelf}))
	obj := lbObj(row, "Top_Senders", 0)
	if obj["label"] != "Inline" || obj["label_status"] != "labeled" || !reflect.DeepEqual(obj["labels"], lbStrings("ethereum/dex-router:Uniswap")) {
		t.Fatalf("object entry = %#v", obj)
	}
}

// A row whose names column does not name the configured position is left
// exactly as it came: neither the entries nor the names column change.
func TestBitqueryLabelsUnknownPositionLeavesRow(t *testing.T) {
	body := `[{"Top_Fields":["address","amount"],"Top_Receivers":[["` + lbBtcHot + `",1]],"Top_Senders":[["` + lbBtcPool + `",2]]}]`
	f := newLBFixture(t, body)
	cfg := lbBTCLabels()
	cfg.AddressParam, cfg.AddressColumn = "", ""
	rows := f.invoke(t, f.tool(t, lbToolConfig(cfg)), map[string]any{"address": lbBtcSelf})
	if !reflect.DeepEqual(rows, f.decoded(t)) {
		t.Fatalf("rows = %#v, want them unchanged", rows)
	}
	if n := len(f.labels.sent()); n != 0 {
		t.Errorf("label statements = %d, want 0", n)
	}
}

func TestBitqueryLabelsDedupAndBatches(t *testing.T) {
	addr := func(i int) string { return "0x" + strings.Repeat(string(rune('d'+i)), 40) }
	receivers := []string{addr(0), addr(1), addr(2), addr(0), lbEvmSelf}
	senders := []string{addr(1), addr(3), addr(2), addr(1)}
	f := newLBFixture(t, lbEVMBody(receivers, senders))
	cfg := lbEVMLabels()
	cfg.Batch = 3
	// The parameter, normalized, is the same address as a receiver.
	f.invoke(t, f.tool(t, lbToolConfig(cfg)), map[string]any{"address": "0X" + strings.ToUpper(lbEvmSelf[2:])})

	sent := f.labels.sent()
	if len(sent) != 2 {
		t.Fatalf("statements = %d, want 2 (5 distinct addresses in batches of 3)", len(sent))
	}
	var all []string
	for i, s := range sent {
		list := lbList(lbAddressListRe, s)
		if len(list) > 3 {
			t.Errorf("statement %d looks up %d addresses, more than the batch", i, len(list))
		}
		all = append(all, list...)
	}
	want := []string{lbEvmSelf, addr(0), addr(1), addr(2), addr(3)}
	slices.Sort(all)
	slices.Sort(want)
	if !slices.Equal(all, want) {
		t.Fatalf("addresses looked up = %q, want each of %q once", all, want)
	}
}

func TestBitqueryLabelsBatchesStayUnderTheQuerySize(t *testing.T) {
	l := &bitqueryLabels{batch: 100_000, timeout: time.Second, maxRows: 1, chainList: "'x'"}
	fake := &lbFakeLabels{}
	l.source = fake
	// 6000 addresses of 50 characters: about 324 KB as one literal list.
	var addrs []string
	for i := range 6000 {
		addrs = append(addrs, fmt.Sprintf("T%049d", i))
	}
	if _, err := l.lookup(context.Background(), addrs); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	sent := fake.sent()
	if len(sent) < 2 {
		t.Fatalf("statements = %d, want the list split", len(sent))
	}
	for i, s := range sent {
		if len(s) > maxLabelsListBytes+1000 {
			t.Errorf("statement %d is %d bytes", i, len(s))
		}
	}
}

func TestBitqueryLabelsPick(t *testing.T) {
	l := &bitqueryLabels{rank: map[string]int{"sanctioned-entity": 0, "cex-hot-wallet": 1}}
	e := func(chain, typ, value string) labelsEntry { return labelsEntry{chain, typ, value} }
	cases := []struct {
		name    string
		entries []labelsEntry
		want    string
	}{
		{"listed type wins over an earlier value", []labelsEntry{e("ethereum", "cex-hot-wallet", "A"), e("ethereum", "sanctioned-entity", "Z")}, "ethereum/sanctioned-entity:Z"},
		{"listed type wins over an unlisted one", []labelsEntry{e("ethereum", "aaa", "A"), e("ethereum", "cex-hot-wallet", "Z")}, "ethereum/cex-hot-wallet:Z"},
		{"one type: smallest value", []labelsEntry{e("ethereum", "cex-hot-wallet", "Kraken"), e("ethereum", "cex-hot-wallet", "Binance"), e("ethereum", "cex-hot-wallet", "binance")}, "ethereum/cex-hot-wallet:Binance"},
		{"one type and value: smallest chain", []labelsEntry{e("polygon", "cex-hot-wallet", "B"), e("bsc", "cex-hot-wallet", "B")}, "bsc/cex-hot-wallet:B"},
		{"unlisted only: by type name", []labelsEntry{e("ethereum", "mixer", "A"), e("ethereum", "mining-pool", "Z")}, "ethereum/mining-pool:Z"},
	}
	for _, tc := range cases {
		if got := l.pick(tc.entries).text(); got != tc.want {
			t.Errorf("%s: pick = %q, want %q", tc.name, got, tc.want)
		}
		rev := slices.Clone(tc.entries)
		slices.Reverse(rev)
		if got := l.pick(rev).text(); got != tc.want {
			t.Errorf("%s (reversed): pick = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestBitqueryLabelsNone(t *testing.T) {
	f := newLBFixture(t, lbEVMBody([]string{lbEvmB}, nil))
	row := lbRow(t, f.invoke(t, f.tool(t, lbToolConfig(lbEVMLabels())), map[string]any{"address": lbEvmB}))
	lbCheckEntry(t, "receiver", []any{}, nil, "none")(lbObjValues(lbObj(row, "Top_Receivers", 0)))
	if got := row["Address_Labels"]; !reflect.DeepEqual(got, []any{}) {
		t.Errorf("Address_Labels = %#v, want an empty list", got)
	}
	if n := len(f.labels.sent()); n != 1 {
		t.Errorf("statements = %d, want 1", n)
	}
}

func TestBitqueryLabelsNoCoverage(t *testing.T) {
	f := newLBFixture(t, lbEVMBody([]string{lbEvmA}, []string{lbEvmC}))
	cfg := lbEVMLabels()
	cfg.NoCoverage, cfg.Source, cfg.Chains = true, "", nil
	row := lbRow(t, f.invoke(t, f.tool(t, lbToolConfig(cfg)), map[string]any{"address": lbEvmSelf}))
	lbCheckEntry(t, "receiver", nil, nil, "no_coverage")(lbObjValues(lbObj(row, "Top_Receivers", 0)))
	lbCheckEntry(t, "sender", nil, nil, "no_coverage")(lbObjValues(lbObj(row, "Top_Senders", 0)))
	if v, ok := row["Address_Labels"]; !ok || v != nil {
		t.Errorf("Address_Labels = %#v (present %v), want null", v, ok)
	}
	if n := len(f.labels.sent()); n != 0 {
		t.Fatalf("statements = %d, want none on a chain without coverage", n)
	}
}

func lbStripLabels(rows []any) []any {
	for _, r := range rows {
		row := r.(map[string]any)
		delete(row, "Address_Labels")
		for _, col := range []string{"Top_Receivers", "Top_Senders"} {
			entries, _ := row[col].([]any)
			for _, e := range entries {
				obj := e.(map[string]any)
				delete(obj, "labels")
				delete(obj, "label")
				delete(obj, "label_status")
			}
		}
	}
	return rows
}

func TestBitqueryLabelsFailOpen(t *testing.T) {
	f := newLBFixture(t, lbEVMBody([]string{lbEvmA, lbEvmB}, []string{lbEvmC}))
	f.labels.err = errors.New("unable to execute query: Code: 241. DB::Exception: Memory limit exceeded")
	rows := f.invoke(t, f.tool(t, lbToolConfig(lbEVMLabels())), map[string]any{"address": lbEvmSelf})
	row := lbRow(t, rows)
	for _, col := range []string{"Top_Receivers", "Top_Senders"} {
		for i := range row[col].([]any) {
			lbCheckEntry(t, col, nil, nil, "unavailable")(lbObjValues(lbObj(row, col, i)))
		}
	}
	if v, ok := row["Address_Labels"]; !ok || v != nil {
		t.Errorf("Address_Labels = %#v (present %v), want null", v, ok)
	}
	if got := lbStripLabels(rows); !reflect.DeepEqual(got, f.decoded(t)) {
		t.Fatalf("data changed by a failed lookup:\n got %#v\nwant %#v", got, f.decoded(t))
	}
	logs := f.logs.String()
	if !strings.Contains(logs, "WARN") || !strings.Contains(logs, "labels lookup failed") || !strings.Contains(logs, "Memory limit exceeded") {
		t.Errorf("no WARN with the error: %s", logs)
	}
}

func TestBitqueryLabelsTimeoutFailsOpen(t *testing.T) {
	f := newLBFixture(t, lbEVMBody([]string{lbEvmA}, nil))
	f.labels.block = true
	cfg := lbEVMLabels()
	cfg.TimeoutSec = 1
	start := time.Now()
	row := lbRow(t, f.invoke(t, f.tool(t, lbToolConfig(cfg)), map[string]any{"address": lbEvmSelf}))
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("the call took %v; the lookup must stop at its timeout", d)
	}
	lbCheckEntry(t, "receiver", nil, nil, "unavailable")(lbObjValues(lbObj(row, "Top_Receivers", 0)))
	if row["Role"] != "relay" || lbObj(row, "Top_Receivers", 0)["counterparty"] != lbEvmA {
		t.Errorf("data lost: %#v", row)
	}
	sent := f.labels.sent()
	if len(sent) != 1 || !strings.Contains(sent[0], "max_execution_time = 1,") {
		t.Errorf("statements = %q, want one with max_execution_time = 1", sent)
	}
	if !strings.Contains(f.logs.String(), "context deadline exceeded") {
		t.Errorf("no WARN with the timeout: %s", f.logs.String())
	}
}

func TestBitqueryLabelsOfflineToolFailsOpen(t *testing.T) {
	f := newLBFixture(t, lbEVMBody([]string{lbEvmA}, nil))
	tl, err := lbToolConfig(lbEVMLabels()).Initialize(f.ctx)
	if err != nil {
		t.Fatalf("offline initialize: %v", err)
	}
	row := lbRow(t, f.invoke(t, tl.(Tool), map[string]any{"address": lbEvmSelf}))
	lbCheckEntry(t, "receiver", nil, nil, "unavailable")(lbObjValues(lbObj(row, "Top_Receivers", 0)))
}

func TestBitqueryLabelsAddressNormalisation(t *testing.T) {
	tron := "TQn9Y2khEsLJW1ChVWFMSMeRDow5KcbLSE"
	f := newLBFixture(t, `[{"Top_Receivers":[`+
		`{"counterparty":"0xAbCdEf0123456789aBcDeF0123456789AbCdEf01"},`+
		`{"counterparty":"0XAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},`+
		`{"counterparty":"`+tron+`"},`+
		`{"counterparty":"x' OR 1=1 --"},`+
		`{"counterparty":""},`+
		`{"counterparty":42},`+
		`{"amount":1}]}]`)
	f.labels.labels[tron] = [][3]string{{"ethereum", "cex-deposit-address", "Binance"}}
	cfg := lbEVMLabels()
	cfg.Fields = cfg.Fields[:1]
	row := lbRow(t, f.invoke(t, f.tool(t, lbToolConfig(cfg)), map[string]any{"address": strings.ToUpper(lbEvmA)}))

	sent := f.labels.sent()
	if len(sent) != 1 {
		t.Fatalf("statements = %d, want 1", len(sent))
	}
	got := lbList(lbAddressListRe, sent[0])
	want := []string{lbEvmA, "0xabcdef0123456789abcdef0123456789abcdef01", tron}
	if !slices.Equal(got, want) {
		t.Fatalf("addresses looked up = %q, want %q", got, want)
	}
	// 0X… is the same address as 0x…, so its labels come back.
	if lbObj(row, "Top_Receivers", 1)["label_status"] != "labeled" {
		t.Errorf("0X address: %#v", lbObj(row, "Top_Receivers", 1))
	}
	if lbObj(row, "Top_Receivers", 2)["label"] != "ethereum/cex-deposit-address:Binance" {
		t.Errorf("base58 address (case kept): %#v", lbObj(row, "Top_Receivers", 2))
	}
	for i := 3; i < 7; i++ {
		lbCheckEntry(t, "not an address", []any{}, nil, "none")(lbObjValues(lbObj(row, "Top_Receivers", i)))
	}
	if strings.Contains(sent[0], "OR 1=1") {
		t.Fatalf("a value that is not an address reached the statement: %s", sent[0])
	}
}

// An address parameter escaped as a single-quoted literal (as on the XRP Ledger,
// Stellar and Cardano tools) reaches Invoke in quotes; the lookup unwraps it.
func TestBitqueryLabelsEscapedAddressParam(t *testing.T) {
	xrp := "rEb8TK3gBgk5auZkwc6sHnwrGVJH8DuaLh"
	f := newLBFixture(t, `[{"Top_Receivers":[]}]`)
	f.labels.labels[xrp] = [][3]string{{"ripple", "cex-hot-wallet", "Binance"}}
	cfg := lbEVMLabels()
	cfg.Chains = []string{"ripple"}
	param := parameters.NewStringParameter("address", "address", parameters.WithStringEscape("single-quotes"))
	tl := f.tool(t, lbToolConfig(cfg, param))
	row := lbRow(t, f.invoke(t, tl, map[string]any{"address": xrp}))
	if got := row["Address_Labels"]; !reflect.DeepEqual(got, lbStrings("ripple/cex-hot-wallet:Binance")) {
		t.Fatalf("Address_Labels = %#v", got)
	}
	if hits := f.data.calls(); len(hits) != 1 || hits[0].body != "SELECT '"+xrp+"'" {
		t.Fatalf("data statement = %+v, want the escaped literal", hits)
	}
}

func TestBitqueryLabelsStatement(t *testing.T) {
	f := newLBFixture(t, lbEVMBody([]string{lbEvmA, lbEvmB}, nil))
	cfg := lbEVMLabels()
	cfg.MaxRowsToRead = 1234567
	h := nethttp.Header{}
	h.Set("X-Bitquery-Payer-Id", "42")
	h.Set("X-Bitquery-User-Id", "7")
	ctx := util.WithBitqueryIdentity(f.ctx, h)
	f.invokeCtx(t, ctx, f.tool(t, lbToolConfig(cfg)), map[string]any{"address": lbEvmSelf})

	sent := f.labels.sent()
	if len(sent) != 1 {
		t.Fatalf("statements = %d, want 1", len(sent))
	}
	want := `SELECT address, chain, label_type, label_value
FROM directory.labels
WHERE address IN ('` + lbEvmSelf + `', '` + lbEvmA + `', '` + lbEvmB + `') AND chain IN ('ethereum', 'bsc')
GROUP BY address, chain, label_type, label_value
SETTINGS max_execution_time = 5, max_rows_to_read = 1234567, read_overflow_mode = 'throw', timeout_overflow_mode = 'throw'`
	if sent[0] != want {
		t.Fatalf("statement:\n%s\nwant:\n%s", sent[0], want)
	}
	if !f.labels.queryIDs[0] {
		t.Errorf("the label statement ran without the caller identity its query_id is built from")
	}
}

// A tool without bitqueryLabels sends the same request and returns exactly
// what the source decoded; it never reaches the labels source.
func TestBitqueryLabelsToolWithoutKeyUnchanged(t *testing.T) {
	f := newLBFixture(t, lbEVMBody([]string{lbEvmA, lbEvmB}, []string{lbEvmC}))
	plain := f.invoke(t, f.tool(t, lbToolConfig(nil)), map[string]any{"address": lbEvmSelf})
	if !reflect.DeepEqual(plain, f.decoded(t)) {
		t.Fatalf("result = %#v, want the decoded body", plain)
	}
	got, _ := json.Marshal(plain)
	want, _ := json.Marshal(f.decoded(t))
	if !bytes.Equal(got, want) {
		t.Fatalf("result JSON = %s, want %s", got, want)
	}
	if n := len(f.labels.sent()); n != 0 {
		t.Fatalf("label statements = %d, want 0", n)
	}
	f.invoke(t, f.tool(t, lbToolConfig(lbEVMLabels())), map[string]any{"address": lbEvmSelf})
	if hits := f.data.calls(); len(hits) != 2 || hits[0] != hits[1] {
		t.Fatalf("data requests differ with and without bitqueryLabels: %+v", hits)
	}
}

// A result that is not rows (a text answer) passes through untouched.
func TestBitqueryLabelsTextResultUntouched(t *testing.T) {
	l := &bitqueryLabels{fields: []LabelsField{{Column: "Top_Senders", Key: "counterparty"}}, source: &lbFakeLabels{}}
	if got := l.enrich(context.Background(), "plain text", nil); got != "plain text" {
		t.Fatalf("got %#v", got)
	}
	if got := l.enrich(context.Background(), []any{}, nil); !reflect.DeepEqual(got, []any{}) {
		t.Fatalf("got %#v", got)
	}
}

func TestBitqueryLabelsConfigErrors(t *testing.T) {
	f := newLBFixture(t, "[]")
	withLookup := tools.WithSourceLookup(f.ctx, f.srcs)
	cases := []struct {
		name   string
		ctx    context.Context
		mutate func(*Config)
		want   string
	}{
		{"no fields", withLookup, func(c *Config) { c.Labels.Fields = nil; c.Labels.AddressParam, c.Labels.AddressColumn = "", "" }, "fields is empty"},
		{"field without key", withLookup, func(c *Config) { c.Labels.Fields[0].Key = "" }, "needs column and key"},
		{"column twice", withLookup, func(c *Config) { c.Labels.Fields[1].Column = c.Labels.Fields[0].Column }, "listed twice"},
		{"names column is labelled", withLookup, func(c *Config) { c.Labels.Fields[0].NamesColumn = c.Labels.Fields[1].Column }, "is also a labelled column"},
		{"param without column", withLookup, func(c *Config) { c.Labels.AddressColumn = "" }, "go together"},
		{"unknown param", withLookup, func(c *Config) { c.Labels.AddressParam = "addr" }, `"addr" is not a parameter`},
		{"int param", withLookup, func(c *Config) {
			c.BodyParams = parameters.Parameters{parameters.NewIntParameter("address", "address")}
		}, "must be a string parameter"},
		{"other escape", withLookup, func(c *Config) {
			c.BodyParams = parameters.Parameters{parameters.NewStringParameter("address", "address", parameters.WithStringEscape("double-quotes"))}
		}, "only the single-quotes escape"},
		{"address column is labelled", withLookup, func(c *Config) { c.Labels.AddressColumn = "Top_Senders" }, "is also a labelled column"},
		{"priority twice", withLookup, func(c *Config) { c.Labels.Priority = []string{"mixer", "mixer"} }, `lists "mixer" twice`},
		{"output names collide", withLookup, func(c *Config) { c.Labels.OutputNames = &LabelsOutputNames{Label: "labels"} }, "three different names"},
		{"negative batch", withLookup, func(c *Config) { c.Labels.Batch = -1 }, "cannot be negative"},
		{"no source", withLookup, func(c *Config) { c.Labels.Source = "" }, "source is required"},
		{"no chains", withLookup, func(c *Config) { c.Labels.Chains = nil }, "chains is empty"},
		{"bad chain", withLookup, func(c *Config) { c.Labels.Chains = []string{"ethereum') OR ('1"} }, "is not a directory chain name"},
		{"chain twice", withLookup, func(c *Config) { c.Labels.Chains = []string{"bsc", "bsc"} }, `chain "bsc" is listed twice`},
		{"missing source", withLookup, func(c *Config) { c.Labels.Source = "clickhouse-labelz" }, `unable to retrieve labels source "clickhouse-labelz"`},
		{"not clickhouse", withLookup, func(c *Config) { c.Labels.Source = "gw-eth" }, `"gw-eth" is not a clickhouse source`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := lbToolConfig(lbEVMLabels())
			tc.mutate(&cfg)
			_, err := cfg.Initialize(tc.ctx)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}

	// noCoverage needs neither source nor chains.
	cfg := lbToolConfig(lbEVMLabels())
	cfg.Labels.NoCoverage, cfg.Labels.Source, cfg.Labels.Chains = true, "", nil
	if _, err := cfg.Initialize(withLookup); err != nil {
		t.Fatalf("noCoverage without source: %v", err)
	}
}
