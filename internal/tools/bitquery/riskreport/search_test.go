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
	"math"
	"math/big"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	bybitHot = "TU4vEruvZwLLkSfV9bNw12EJTPvNr7Pvaa"
	wellHot  = "TQgxyP1jDXGBvmgwqnJ7jKkEqWEogfRM5V"
)

const b58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// synth is a made-up address that passes the base58 shape check.
func synth(t *testing.T, tag string, i int) string {
	t.Helper()
	enc := ""
	for n := i; ; n /= 58 {
		enc = string(b58Alphabet[n%58]) + enc
		if n < 58 {
			break
		}
	}
	a := "T" + tag + strings.Repeat("1", 33-len(tag)-len(enc)) + enc
	if !tronAddressRe.MatchString(a) {
		t.Fatalf("synthetic address %q is not a Tron address", a)
	}
	return a
}

// usdt turns a decimal USDT amount into raw units (6 decimals).
func usdtRaw(t *testing.T, s string) *big.Int {
	t.Helper()
	whole, frac, _ := strings.Cut(s, ".")
	frac += strings.Repeat("0", 6-len(frac))
	v, ok := new(big.Int).SetString(whole+frac, 10)
	if !ok {
		t.Fatalf("amount %q", s)
	}
	return v
}

// rawOf reads an amount of the report back into raw units.
func rawOf(t *testing.T, v any) *big.Int {
	t.Helper()
	s, _ := v.(string)
	return usdtRaw(t, s)
}

func asFloat(t *testing.T, v any) float64 {
	t.Helper()
	f, err := strconv.ParseFloat(v.(string), 64)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

type at2Event struct {
	in       bool
	block    uint64
	amount   *big.Int
	toTarget bool
	source   int // 0 unlabelled, 1 the Bybit hot wallet, 2 the Well Wallet hot wallet
}

func loadAT2(t *testing.T) []at2Event {
	t.Helper()
	data, err := os.ReadFile("testdata/at2_events.csv")
	if err != nil {
		t.Fatal(err)
	}
	var out []at2Event
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r", ""), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, ",")
		if len(f) != 5 {
			t.Fatalf("line %q", line)
		}
		b, _ := strconv.ParseUint(f[1], 10, 64)
		src, _ := strconv.Atoi(f[4])
		out = append(out, at2Event{in: f[0] == "i", block: b, amount: usdtRaw(t, f[2]), toTarget: f[3] == "1", source: src})
	}
	if len(out) != 188 {
		t.Fatalf("AT2 events = %d, want 188", len(out))
	}
	return out
}

var at2Start = time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)

const at2FirstBlock = 84834062

func at2Time(block uint64) time.Time {
	return at2Start.Add(time.Duration(block-at2FirstBlock) * 3 * time.Second)
}

func txOf(block uint64) string { return fmt.Sprintf("%064x", block) }

func TestSplitIsExact(t *testing.T) {
	w := map[string]*big.Int{"a": big.NewInt(1), "b": big.NewInt(1), "c": big.NewInt(1)}
	for _, amount := range []int64{0, 1, 2, 3, 7, 100} {
		parts := split(big.NewInt(amount), w, big.NewInt(3))
		if got := sumOf(parts); got.Int64() != amount {
			t.Errorf("split(%d) adds up to %v", amount, got)
		}
		for k, p := range parts {
			if amount <= 3 && p.Cmp(w[k]) > 0 {
				t.Errorf("split(%d): part %s = %v over its weight", amount, k, p)
			}
		}
	}
	// The largest remainder gets the unit left over.
	parts := split(big.NewInt(10), map[string]*big.Int{"x": big.NewInt(2), "y": big.NewInt(1)}, big.NewInt(3))
	if parts["x"].Int64() != 7 || parts["y"].Int64() != 3 {
		t.Errorf("split(10, 2:1) = %v / %v, want 7 / 3", parts["x"], parts["y"])
	}
}

// The rule on its own: a mint is a source of its own, a send above the
// balance is history_incomplete, a receipt in the same frame comes first, and
// what is left adds up to the balance.
func TestReplayRules(t *testing.T) {
	ev := []histEvent{
		{In: true, Peer: tronZero, Raw: big.NewInt(100), Block: 1},
		{In: true, Peer: "S", Raw: big.NewInt(300), Block: 2},
		{In: false, Peer: "D", Raw: big.NewInt(200), Block: 3},
		{In: true, Peer: "S", Raw: big.NewInt(50), Block: 4},
		{In: false, Peer: tronZero, Raw: big.NewInt(500), Block: 4},
	}
	key := func(e histEvent) string {
		if e.Peer == tronZero {
			if e.In {
				return keyMint
			}
			return keyBurn
		}
		return e.Peer
	}
	sortEvents(ev)
	if !ev[3].In {
		t.Fatalf("a receipt in the same frame must come before the send")
	}
	f, err := replay(ev, key)
	if err != nil {
		t.Fatal(err)
	}
	if f.out["D"][keyMint].Int64() != 50 || f.out["D"]["S"].Int64() != 150 {
		t.Errorf("the send carries the mix: %v", f.out["D"])
	}
	// Balance before the burn: 200 + 50 = 250; the burn of 500 takes it all.
	if f.incompleteSends != 1 || f.incomplete.Int64() != 250 || f.out[keyBurn][keyIncomplete].Int64() != 250 {
		t.Errorf("history_incomplete: %d sends, %v", f.incompleteSends, f.incomplete)
	}
	if f.balance.Sign() != 0 || len(f.held) != 0 {
		t.Errorf("balance %v, held %v", f.balance, f.held)
	}
	if _, err := replay([]histEvent{ev[2], ev[0]}, key); err == nil {
		t.Errorf("events out of chain order were accepted")
	}
}

// AT2, the attribution alone: Bybit's 4,860.27 contributes 91.22 to the test
// wallet, all of it leaves the address, nothing is over-attributed, and the
// test wallet gets exactly what was sent to it.
func TestReplayAT2(t *testing.T) {
	var ev []histEvent
	for i, e := range loadAT2(t) {
		h := histEvent{In: e.in, Raw: e.amount, Block: e.block, Time: at2Time(e.block), Tx: txOf(e.block)}
		switch {
		case e.in && e.source == 1:
			h.Peer = "bybit"
		case e.in && e.source == 2:
			h.Peer = "well"
		case e.in:
			h.Peer = fmt.Sprintf("src%d", i)
		case e.toTarget:
			h.Peer = "target"
		default:
			h.Peer = "elsewhere"
		}
		ev = append(ev, h)
	}
	f, err := replay(ev, func(e histEvent) string { return e.Peer })
	if err != nil {
		t.Fatal(err)
	}
	if f.incompleteSends != 0 {
		t.Errorf("sends without balance: %d", f.incompleteSends)
	}
	near := func(name string, got *big.Int, want float64) {
		g, _ := new(big.Float).Quo(new(big.Float).SetInt(got), big.NewFloat(1e6)).Float64()
		if math.Abs(g-want) > 0.01 {
			t.Errorf("%s = %.6f, want %.2f", name, g, want)
		}
	}
	near("Bybit to the test wallet", f.out["target"]["bybit"], 91.22)
	near("Well Wallet to the test wallet", f.out["target"]["well"], 268.01)
	if got := f.sentTo["target"]; got.Cmp(usdtRaw(t, "8319100")) != 0 {
		t.Errorf("sent to the test wallet = %s", formatUnits(got, 6))
	}
	for _, src := range []string{"bybit", "well"} {
		down := new(big.Int)
		for _, by := range f.out {
			if v, ok := by[src]; ok {
				down.Add(down, v)
			}
		}
		total := new(big.Int).Add(down, valueOr(f.held[src]))
		if total.Cmp(f.recvFrom[src]) != 0 {
			t.Errorf("%s: downstream %s + held %s != received %s", src, formatUnits(down, 6), formatUnits(valueOr(f.held[src]), 6), formatUnits(f.recvFrom[src], 6))
		}
	}
	near("Bybit downstream", new(big.Int).Sub(f.recvFrom["bybit"], valueOr(f.held["bybit"])), 4860.27)
	near("Well Wallet downstream", new(big.Int).Sub(f.recvFrom["well"], valueOr(f.held["well"])), 14280.00)
}

func valueOr(v *big.Int) *big.Int {
	if v == nil {
		return new(big.Int)
	}
	return v
}

// AT2 end to end, view all: the test wallet received 8,319,100 USDT from
// TUrdave, whose history holds one receipt from Bybit's hot wallet and one from
// Well Wallet's. Both are found at 2 hops with a 2-transfer evidence path, and
// every unit of the analysed total is accounted for.
func TestAT2MultiHop(t *testing.T) {
	target := synth(t, "Target", 1)
	events := loadAT2(t)
	chain := &fakeChain{}
	toTarget := new(big.Int)
	var firstTx string
	var last time.Time
	var bybitTx string
	n := 0
	for i, e := range events {
		tr := fakeTransfer{raw: e.amount.String(), block: e.block, time: at2Time(e.block).Format(time.DateTime), tx: txOf(e.block)}
		if e.in {
			tr.to = plainSender
			switch e.source {
			case 1:
				tr.from = bybitHot
				bybitTx = tr.tx
			case 2:
				tr.from = wellHot
			default:
				tr.from = synth(t, "Src", i)
			}
		} else {
			tr.from = plainSender
			tr.to = synth(t, "Dst", 1)
			if e.toTarget {
				tr.to = target
				toTarget.Add(toTarget, e.amount)
				if firstTx == "" {
					firstTx = tr.tx
				}
				last = at2Time(e.block)
				n++
			}
		}
		chain.transfers = append(chain.transfers, tr)
	}
	raw := toTarget.String()
	chain.assets = []map[string]any{usdtAssetRow(raw, "0", n, 0)}
	edge := edgeRow("in", plainSender, raw, raw, "900000000000", n, [4]string{"1", fmt.Sprint(n), raw, raw}, firstTx)
	edge["Last"] = last.Format(time.DateTime)
	chain.edges = []map[string]any{edge}
	labels := &fakeLabels{rows: map[string][]map[string]any{
		bybitHot: {{"label_type": "cex-hot-wallet", "label_value": "bybit-hot", "source": "vendor-x", "recorded_at": "2026-01-01 00:00:00"}},
		wellHot:  {{"label_type": "cex-hot-wallet", "label_value": "well-wallet-hot", "source": "vendor-x", "recorded_at": "2026-01-01 00:00:00"}},
	}}
	tool := newTestTool(t, chain, labels)
	res, err := tool.Invoke(context.Background(), chain, mustParams(t, tool, map[string]any{"chain": "tron", "address": target,
		"view": "all", "direction": "received", "max_hops": 2, "min_amount_usd": 0.0}), "")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := decode(t, res)

	conns := map[string]map[string]any{}
	for _, c := range out["connections"].([]any) {
		cm := c.(map[string]any)
		conns[cm["entity"].(string)] = cm
	}
	for entity, want := range map[string]float64{"bybit": 91.22, "well-wallet": 268.01} {
		c, ok := conns[entity]
		if !ok {
			t.Fatalf("%s not among the connections: %v", entity, out["connections"])
		}
		if got := asFloat(t, c["received"]); math.Abs(got-want) > 0.01 {
			t.Errorf("%s received %v, want %.2f", entity, got, want)
		}
		if h := c["received_shortest_hops"]; h != float64(2) {
			t.Errorf("%s received_shortest_hops = %v, want 2", entity, h)
		}
		if p, _ := c["evidence_path"].([]any); len(p) != 2 || p[0] != firstTx {
			t.Errorf("%s evidence_path = %v, want 2 transfers starting at %s", entity, c["evidence_path"], firstTx)
		}
		if c["entity_type"] != "Exchange (licence unknown)" {
			t.Errorf("%s type %v", entity, c["entity_type"])
		}
	}
	if p := conns["bybit"]["evidence_path"].([]any); p[1] != bybitTx {
		t.Errorf("bybit's second hop is %v, want its receipt %s", p[1], bybitTx)
	}

	// Conservation, exactly: the entities and every cut add up to 8,319,100.
	sum := new(big.Int)
	for _, c := range conns {
		sum.Add(sum, rawOf(t, c["received"]))
	}
	by := out["unattributed_by_cut"].(map[string]any)["received"].(map[string]any)
	cutSum := new(big.Int)
	for _, v := range by {
		cutSum.Add(cutSum, rawOf(t, v))
	}
	sum.Add(sum, cutSum)
	if sum.Cmp(usdtRaw(t, "8319100")) != 0 {
		t.Errorf("attributed + unattributed = %s, want 8319100.000000", formatUnits(sum, 6))
	}
	if un := rawOf(t, out["unattributed"].(map[string]any)["received"]); un.Cmp(cutSum) != 0 {
		t.Errorf("unattributed %s != the sum of its cuts %s", formatUnits(un, 6), formatUnits(cutSum, 6))
	}
	if rawOf(t, by["max_hops"]).Sign() <= 0 {
		t.Errorf("no max_hops cut: %v", by)
	}
	params := out["params"].(map[string]any)
	if params["max_hops_reached"] != float64(2) || params["max_hops"] != float64(2) {
		t.Errorf("max_hops_reached %v, max_hops %v", params["max_hops_reached"], params["max_hops"])
	}
	for k, v := range params["invariants"].(map[string]any) {
		if v != "ok" {
			t.Errorf("invariant %s: %v", k, v)
		}
	}
	if params["complete"] != true {
		t.Errorf("complete = %v", params["complete"])
	}
	// Unlabelled hop-2 senders are listed with their cut.
	foundMaxHops := false
	for _, u := range out["unnamed_connections"].([]any) {
		um := u.(map[string]any)
		if um["cut"] == "max_hops" && um["received_shortest_hops"] == float64(2) && len(um["evidence_path"].([]any)) == 2 {
			foundMaxHops = true
		}
	}
	if !foundMaxHops {
		t.Errorf("no unnamed max_hops row at hop 2: %v", out["unnamed_connections"])
	}
	// assets, counterparties, labels, degree x2, history x2, labels of hop 2.
	if got := out["cost"].(map[string]any)["statements"].(float64); got != 8 {
		t.Errorf("statements = %v, want 8", got)
	}
	checkStatements(t, chain.statements)
	// The history of TUrdave is read up to its last transfer to the test wallet.
	bound := fmt.Sprint(last.Unix())
	for _, s := range chain.statements {
		if strings.Contains(s, "AS Peer") && !strings.Contains(s, "bounds_f") {
			t.Errorf("history statement without bounds:\n%s", s)
		}
		if strings.Contains(s, "AS Peer") && !strings.Contains(s, bound) {
			t.Errorf("history statement not bounded at %s:\n%s", bound, s)
		}
	}
}

var subqueryRe = regexp.MustCompile(`IN \(SELECT [^)]*FROM`)

// checkStatements holds every statement to the rules: bounded, FORMAT JSON,
// address sets as literal FixedString arrays, never a set read from a table,
// an EXPLAIN only of a single literal address.
func checkStatements(t *testing.T, stmts []string) {
	t.Helper()
	for _, s := range stmts {
		if strings.HasPrefix(s, "EXPLAIN") {
			// An EXPLAIN names one address as a literal: no set, no subquery,
			// and no row limit (the index analysis would apply it).
			if strings.Count(s, "SELECT") != 1 || strings.Contains(s, "IN (") || strings.Contains(s, "arrayJoin") ||
				strings.Count(s, "base58Decode('") != 1 || !strings.HasPrefix(s, "EXPLAIN indexes = 1\n") ||
				!strings.Contains(s, "max_execution_time") || strings.Contains(s, "max_rows_to_read") ||
				!strings.HasSuffix(strings.TrimSpace(s), "FORMAT JSON") {
				t.Errorf("EXPLAIN not on a single literal address:\n%s", s)
			}
			continue
		}
		if !strings.Contains(s, "max_rows_to_read") || !strings.Contains(s, "read_overflow_mode = 'throw'") || !strings.Contains(s, "max_execution_time") {
			t.Errorf("unbounded statement:\n%s", s)
		}
		if !strings.Contains(s, "max_result_rows") || !strings.Contains(s, "result_overflow_mode = 'throw'") || !strings.HasSuffix(strings.TrimSpace(s), "FORMAT JSON") {
			t.Errorf("statement without a result cap or FORMAT JSON:\n%s", s)
		}
		if subqueryRe.MatchString(s) || strings.Contains(s, "EXPLAIN") {
			t.Errorf("statement reads a set from a table, or explains:\n%s", s)
		}
		if strings.Contains(s, "AS Peer") || strings.Contains(s, "AS Degree") {
			if !strings.Contains(s, "IN (SELECT arrayJoin(set_f))") || !strings.Contains(s, "toFixedString(x, 20)") {
				t.Errorf("address set not in the literal FixedString shape:\n%s", s)
			}
		}
	}
}

// graph is a small search on both sides, max_hops 3, with every kind of end:
//
//	received: A (hop 1; it also sent 5 to itself, which changes nothing) got funds from H (a hub over the row budget), H2 (a hub
//	by its count), L (sanctioned), the target itself (a cycle), N (unlabelled,
//	whose own history lacks 100 of what it sent: history_incomplete) and D
//	(dust: small); N got funds from E (an exchange, hop 3) and F (unlabelled at
//	the last hop: max_hops).
//	sent: the target sent 100 to A (which sent part of it back: a cycle, the
//	rest unspent) and 500 to R, which sent to X (an exchange, hop 2), burned some
//	and kept the rest (unspent).
type graph struct {
	target, a, h, h2, l, n, d, e, f, r, q, x string
	chain                                    *fakeChain
	labels                                   *fakeLabels
}

func newGraph(t *testing.T) graph {
	g := graph{target: synth(t, "Target", 2), a: synth(t, "Ahop", 1), h: synth(t, "Hub", 1), h2: synth(t, "Hub", 2),
		l: synth(t, "Lab", 1), n: synth(t, "Mid", 1), d: synth(t, "Dust", 1), e: synth(t, "Exch", 1), f: synth(t, "Far", 1),
		r: synth(t, "Recv", 1), q: synth(t, "Qsrc", 1), x: synth(t, "Exch", 2)}
	tr := func(from, to, amount string, block uint64) fakeTransfer {
		return fakeTransfer{from: from, to: to, raw: usdtRaw(t, amount).String(), block: block,
			time: at2Time(at2FirstBlock + block).Format(time.DateTime), tx: txOf(block)}
	}
	g.chain = &fakeChain{
		transfers: []fakeTransfer{
			tr(g.e, g.n, "100", 10), tr(g.f, g.n, "50", 11),
			tr(g.h, g.a, "400", 20), tr(g.h2, g.a, "50", 21), tr(g.l, g.a, "300", 22), tr(g.a, g.a, "5", 22), tr(g.target, g.a, "100", 23),
			tr(g.n, g.a, "250", 24), tr(g.d, g.a, "0.5", 25), tr(g.a, g.target, "1000", 26),
			tr(g.q, g.r, "100", 30), tr(g.target, g.r, "500", 31), tr(g.r, g.x, "300", 32), tr(g.r, tronZero, "50", 33),
		},
		degree:   map[string]int{g.h2: 6000},
		overflow: map[string]bool{g.h: true},
		// H's primary key: ~114M transfers, the size of an exchange hot wallet.
		granules: map[string]int{g.h: 14000},
	}
	g.chain.assets = []map[string]any{usdtAssetRow("1000000000", "600000000", 1, 2)}
	in := edgeRow("in", g.a, "1000000000", "1000000000", "1000000000", 1, [4]string{"1", "1", "1000000000", "1000000000"}, txOf(26))
	in["Last"] = at2Time(at2FirstBlock + 26).Format(time.DateTime)
	g.chain.edges = []map[string]any{in,
		edgeRow("out", g.r, "500000000", "500000000", "500000000", 1, [4]string{"2", "2", "600000000", "600000000"}, txOf(31)),
		edgeRow("out", g.a, "100000000", "100000000", "100000000", 1, [4]string{"2", "2", "600000000", "600000000"}, txOf(23)),
	}
	g.labels = &fakeLabels{rows: map[string][]map[string]any{
		g.l: {{"label_type": "sanctioned-entity", "label_value": "sanctioned-garantex", "source": "ofac", "recorded_at": "2026-01-01 00:00:00"}},
		g.e: {{"label_type": "cex-hot-wallet", "label_value": "kraken-hot", "source": "vendor-x", "recorded_at": "2026-01-01 00:00:00"}},
		g.x: {{"label_type": "cex-deposit-address", "label_value": "binance-deposit", "source": "vendor-x", "recorded_at": "2026-01-01 00:00:00"}},
	}}
	return g
}

func TestMultiHopCuts(t *testing.T) {
	g := newGraph(t)
	tool := newTestTool(t, g.chain, g.labels)
	res, err := tool.Invoke(context.Background(), g.chain, mustParams(t, tool, map[string]any{"chain": "tron", "address": g.target,
		"view": "all", "max_hops": 3, "min_amount_usd": 10.0}), "")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := decode(t, res)
	conns := map[string]map[string]any{}
	for _, c := range out["connections"].([]any) {
		cm := c.(map[string]any)
		conns[cm["entity"].(string)] = cm
	}
	// Received: A's send of 1,000 from a balance of 1,100.5.
	share := func(part float64) float64 { return 1000 * part / 1100.5 }
	checkConn := func(entity, dir string, want float64, hops int, pathLen int) {
		t.Helper()
		c, ok := conns[entity]
		if !ok {
			t.Fatalf("%s not among the connections: %v", entity, out["connections"])
		}
		if got := asFloat(t, c[dir]); math.Abs(got-want) > 0.000002 {
			t.Errorf("%s %s = %v, want %.6f", entity, dir, got, want)
		}
		if h := c[dir+"_shortest_hops"]; h != float64(hops) {
			t.Errorf("%s %s hops = %v, want %d", entity, dir, h, hops)
		}
		if p, _ := c["evidence_path"].([]any); len(p) != pathLen {
			t.Errorf("%s evidence_path = %v, want %d transfers", entity, c["evidence_path"], pathLen)
		}
	}
	checkConn("garantex", "received", share(300), 2, 2)
	// N's 250 to A carried E 100, F 50 and 100 with no known source.
	checkConn("kraken", "received", share(250)*100/250, 3, 3)
	if p := conns["kraken"]["evidence_path"].([]any); p[0] != txOf(26) || p[1] != txOf(24) || p[2] != txOf(10) {
		t.Errorf("kraken's path = %v, want the transfers A->target, N->A, E->N", p)
	}
	// Sent: R's 300 to X carried 250 of the target's 500 (R held 100 of Q's).
	checkConn("binance", "sent", 250, 2, 2)

	by := out["unattributed_by_cut"].(map[string]any)
	near := func(dir, cut string, want float64) {
		t.Helper()
		if got := asFloat(t, by[dir].(map[string]any)[cut]); math.Abs(got-want) > 0.000003 {
			t.Errorf("%s %s = %v, want %.6f", dir, cut, got, want)
		}
	}
	near("received", "hub", share(450))
	near("received", "cycle", share(100))
	near("received", "small", share(0.5))
	near("received", "history_incomplete", share(250)*100/250)
	near("received", "max_hops", share(250)*50/250)
	// A sent its whole receipt of the target's 100 back in its send of 1,000.
	near("sent", "cycle", share(100))
	near("sent", "burn", 50*250.0/300)
	near("sent", "unspent", 100-share(100)+250-50*250.0/300)

	for _, dir := range []string{"received", "sent"} {
		total := map[string]string{"received": "1000", "sent": "600"}[dir]
		sum := new(big.Int)
		for _, c := range conns {
			sum.Add(sum, rawOf(t, c[dir]))
		}
		for _, v := range by[dir].(map[string]any) {
			sum.Add(sum, rawOf(t, v))
		}
		if sum.Cmp(usdtRaw(t, total)) != 0 {
			t.Errorf("%s: attributed + unattributed = %s, want %s exactly", dir, formatUnits(sum, 6), total)
		}
	}
	params := out["params"].(map[string]any)
	if params["max_hops_reached"] != float64(3) {
		t.Errorf("max_hops_reached = %v", params["max_hops_reached"])
	}
	inv := params["invariants"].(map[string]any)
	if !strings.HasPrefix(inv["history_integrity"].(string), "ok; 1 addresses") {
		t.Errorf("history_integrity: %v", inv["history_integrity"])
	}
	// Every cut is listed per direction, with its count and amount.
	cuts := map[string]map[string]any{}
	for _, c := range params["cuts"].([]any) {
		cm := c.(map[string]any)
		cuts[cm["direction"].(string)+" "+cm["reason"].(string)] = cm
	}
	for _, r := range cutReasons {
		for _, d := range []string{"received", "sent"} {
			if _, ok := cuts[d+" "+r]; !ok {
				t.Errorf("cut %s %s not listed", d, r)
			}
		}
	}
	if cuts["received hub"]["counterparties"] != float64(2) {
		t.Errorf("hubs: %v", cuts["received hub"])
	}
	rows := map[string]bool{}
	for _, u := range out["unnamed_connections"].([]any) {
		um := u.(map[string]any)
		rows[um["address"].(string)+" "+um["cut"].(string)] = true
	}
	for _, want := range []string{g.h + " hub", g.h2 + " hub", g.n + " history_incomplete", g.f + " max_hops", g.r + " unspent", g.a + " unspent"} {
		if !rows[want] {
			t.Errorf("unnamed row %q missing: %v", want, out["unnamed_connections"])
		}
	}
	// The hub over its row budget was found by an EXPLAIN of each address of
	// the batch that overflowed (H, H2, N on the receipts table), then the rest
	// was counted; its history was never read.
	explained := map[string]bool{}
	for _, s := range g.chain.statements {
		if strings.Contains(s, "AS Peer") && mentioned(s, g.h) {
			t.Errorf("a hub's history was read")
		}
		if strings.HasPrefix(s, "EXPLAIN") {
			for _, a := range []string{g.h, g.h2, g.n} {
				if mentioned(s, a) {
					explained[a] = true
				}
			}
		}
		if strings.Contains(s, "AS Degree") && mentioned(s, g.h) && !mentioned(s, g.n) {
			t.Errorf("the hub was counted on its own:\n%s", s)
		}
	}
	if len(explained) != 3 {
		t.Errorf("explained %v, want H, H2 and N", explained)
	}
	if n := countWith(g.chain.statements, "EXPLAIN"); n != 3 {
		t.Errorf("EXPLAIN statements = %d, want 3", n)
	}
	cost := out["cost"].(map[string]any)
	for _, d := range cost["detail"].([]any) {
		if dm := d.(map[string]any); dm["name"] == "explain_receipts" && dm["rows_read"] == nil {
			t.Errorf("an EXPLAIN without its cost: %v", dm)
		}
	}
	checkStatements(t, g.chain.statements)
	// The direct floor stays hop 1's: no 1-hop counterparty is labelled.
	if f := out["score"].(map[string]any)["direct_floor"]; f != float64(0) {
		t.Errorf("direct_floor = %v", f)
	}
}

// A report that runs out of its row budget mid-search still returns, with the
// branches it could not follow as cut budget and complete false.
func TestMultiHopBudget(t *testing.T) {
	g := newGraph(t)
	tool := newTestToolWith(t, g.chain, g.labels, func(mc *MethodConfig) { mc.Thresholds.ReportRowsBudget = 4500 })
	res, err := tool.Invoke(context.Background(), g.chain, mustParams(t, tool, map[string]any{"chain": "tron", "address": g.target,
		"view": "all", "max_hops": 3, "min_amount_usd": 10.0}), "")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := decode(t, res)
	params := out["params"].(map[string]any)
	if params["complete"] != false {
		t.Errorf("complete = %v, want false", params["complete"])
	}
	by := out["unattributed_by_cut"].(map[string]any)
	if b := by["received"].(map[string]any)["budget"]; b != "1000.000000" {
		t.Errorf("received budget = %v", b)
	}
	if b := by["sent"].(map[string]any)["budget"]; b != "600.000000" {
		t.Errorf("sent budget = %v", b)
	}
	if len(out["connections"].([]any)) != 0 {
		t.Errorf("connections: %v", out["connections"])
	}
	var budgetRows []string
	for _, u := range out["unnamed_connections"].([]any) {
		if um := u.(map[string]any); um["cut"] == "budget" {
			budgetRows = append(budgetRows, um["address"].(string))
		}
	}
	slices.Sort(budgetRows)
	if want := []string{g.a, g.a, g.r}; !slices.Equal(budgetRows, sortedCopy(want)) {
		t.Errorf("budget rows %v, want %v", budgetRows, want)
	}
}

// Labels come before the small cut: a labelled address reached with less
// than branch_min_usd is still a connection (D, 0.454 USDT at hop 2).
func TestLabelledSmallBranchIsAttributed(t *testing.T) {
	g := newGraph(t)
	g.labels.rows[g.d] = []map[string]any{{"label_type": "gambling", "label_value": "gambling-site", "source": "vendor-x", "recorded_at": "2026-01-01 00:00:00"}}
	tool := newTestTool(t, g.chain, g.labels)
	res, err := tool.Invoke(context.Background(), g.chain, mustParams(t, tool, map[string]any{"chain": "tron", "address": g.target,
		"view": "all", "direction": "received", "max_hops": 3, "min_amount_usd": 10.0}), "")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := decode(t, res)
	var conn map[string]any
	for _, c := range out["connections"].([]any) {
		if cm := c.(map[string]any); cm["entity_type"] == "Gambling" {
			conn = cm
		}
	}
	if conn == nil {
		t.Fatalf("the labelled small branch is not a connection: %v", out["connections"])
	}
	if got := asFloat(t, conn["received"]); math.Abs(got-1000*0.5/1100.5) > 0.000002 || conn["received_shortest_hops"] != float64(2) {
		t.Errorf("connection: %v", conn)
	}
	if got := out["unattributed_by_cut"].(map[string]any)["received"].(map[string]any)["small"]; got != "0.000000" {
		t.Errorf("small = %v, want 0", got)
	}
}

// A mint is attributed to the entity "mint" of type Other from the source
// issuance; the ledger still closes exactly.
func TestMintIsAttributedToIssuance(t *testing.T) {
	g := newGraph(t)
	// N's 100 from E becomes a mint: N got 100 issued, 50 from F, sent 250.
	for i, tr := range g.chain.transfers {
		if tr.from == g.e {
			g.chain.transfers[i].from = tronZero
		}
	}
	tool := newTestTool(t, g.chain, g.labels)
	res, err := tool.Invoke(context.Background(), g.chain, mustParams(t, tool, map[string]any{"chain": "tron", "address": g.target,
		"view": "all", "direction": "received", "max_hops": 3, "min_amount_usd": 10.0}), "")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := decode(t, res)
	var mint map[string]any
	sum := new(big.Int)
	for _, c := range out["connections"].([]any) {
		cm := c.(map[string]any)
		sum.Add(sum, rawOf(t, cm["received"]))
		if cm["entity"] == "mint" {
			mint = cm
		}
	}
	if mint == nil {
		t.Fatalf("no mint connection: %v", out["connections"])
	}
	if mint["entity_type"] != "Other" || mint["label_source"] != "issuance" || mint["label_source_class"] != "issuance" ||
		mint["received_shortest_hops"] != float64(3) || len(mint["evidence_path"].([]any)) != 3 {
		t.Errorf("mint connection: %v", mint)
	}
	if got := asFloat(t, mint["received"]); math.Abs(got-1000*250/1100.5*100/250) > 0.000002 {
		t.Errorf("mint received = %v", got)
	}
	by := out["unattributed_by_cut"].(map[string]any)["received"].(map[string]any)
	if _, ok := by["mint"]; ok {
		t.Errorf("mint is still a cut: %v", by)
	}
	for _, v := range by {
		sum.Add(sum, rawOf(t, v))
	}
	if sum.Cmp(usdtRaw(t, "1000")) != 0 {
		t.Errorf("attributed + unattributed = %s, want 1000 exactly", formatUnits(sum, 6))
	}
}

func countWith(stmts []string, prefix string) int {
	n := 0
	for _, s := range stmts {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}

// An address of an overflowed batch whose EXPLAIN gives no granule count is a
// hub, and the report says it was not measured.
func TestMultiHopHubWithoutEstimate(t *testing.T) {
	g := newGraph(t)
	g.chain.noGranule = map[string]bool{g.h: true}
	tool := newTestTool(t, g.chain, g.labels)
	res, err := tool.Invoke(context.Background(), g.chain, mustParams(t, tool, map[string]any{"chain": "tron", "address": g.target,
		"view": "all", "direction": "received", "max_hops": 3, "min_amount_usd": 10.0}), "")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := decode(t, res)
	found := false
	for _, u := range out["unnamed_connections"].([]any) {
		um := u.(map[string]any)
		if um["address"] == g.h {
			found = true
			if um["cut"] != "hub" || um["note"] != hubWithoutEstimate {
				t.Errorf("H's row: %v", um)
			}
		}
		if um["address"] == g.h2 && um["note"] != nil {
			t.Errorf("a hub by its count carries the no-estimate note: %v", um)
		}
	}
	if !found {
		t.Errorf("H not listed: %v", out["unnamed_connections"])
	}
	for _, c := range out["params"].(map[string]any)["cuts"].([]any) {
		cm := c.(map[string]any)
		if cm["reason"] == "hub" && cm["direction"] == "received" && !strings.Contains(cm["note"].(string), "1 of them without a size estimate") {
			t.Errorf("hub cut note: %v", cm["note"])
		}
	}
}

func TestPrimaryKeyGranules(t *testing.T) {
	text := []string{"ReadFromMergeTree", "Indexes:", "  MinMax", "    Granules: 900/900", "  PrimaryKey", "    Keys:", "      Transfer_Sender",
		"    Condition: x", "    Parts: 3/120", "    Granules: 131/3000000", "  Skip", "    Granules: 5/131"}
	if g, ok := primaryKeyGranules(text); !ok || g != 131 {
		t.Errorf("granules = %d, %v; want 131", g, ok)
	}
	if _, ok := primaryKeyGranules([]string{"PrimaryKey", "  Condition: true"}); ok {
		t.Errorf("a text without a granule count was read as one")
	}
	if _, ok := primaryKeyGranules([]string{"PrimaryKey", "  Granules: many"}); ok {
		t.Errorf("an unreadable granule count was accepted")
	}
}

func sortedCopy(s []string) []string {
	c := slices.Clone(s)
	slices.Sort(c)
	return c
}

// A report whose ledger does not close is refused: here the summary's total
// disagrees with the counterparties'.
func TestMultiHopRefusesUnreconciledSummary(t *testing.T) {
	g := newGraph(t)
	g.chain.assets = []map[string]any{usdtAssetRow("1000000001", "600000000", 1, 2)}
	tool := newTestTool(t, g.chain, g.labels)
	_, err := tool.Invoke(context.Background(), g.chain, mustParams(t, tool, map[string]any{"chain": "tron", "address": g.target,
		"view": "all", "max_hops": 3, "min_amount_usd": 10.0}), "")
	if err == nil || !strings.Contains(err.Error(), "summary reconciles") {
		t.Fatalf("err = %v, want the summary invariant", err)
	}
}

// The direct view is untouched by the search: no search statement, no cut
// buckets, no invariants block.
func TestDirectViewRunsNoSearch(t *testing.T) {
	g := newGraph(t)
	tool := newTestTool(t, g.chain, g.labels)
	res, err := tool.Invoke(context.Background(), g.chain, mustParams(t, tool, map[string]any{"chain": "tron", "address": g.target,
		"min_amount_usd": 10.0}), "")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := decode(t, res)
	if _, ok := out["unattributed_by_cut"]; ok {
		t.Errorf("unattributed_by_cut in the direct view")
	}
	if _, ok := out["params"].(map[string]any)["invariants"]; ok {
		t.Errorf("invariants in the direct view")
	}
	if len(g.chain.statements) != 2 {
		t.Errorf("statements = %d, want 2", len(g.chain.statements))
	}
}

// The two tables disagree: the target recorded 1,000 from A, A's own history
// shows a send of 990. The report says so, splits what A's history carries and
// puts the rest in history_incomplete, and the ledger still closes.
func TestMultiHopTwoTablesDisagree(t *testing.T) {
	g := newGraph(t)
	for i, tr := range g.chain.transfers {
		if tr.from == g.a && tr.to == g.target {
			g.chain.transfers[i].raw = usdtRaw(t, "990").String()
		}
	}
	tool := newTestTool(t, g.chain, g.labels)
	res, err := tool.Invoke(context.Background(), g.chain, mustParams(t, tool, map[string]any{"chain": "tron", "address": g.target,
		"view": "all", "direction": "received", "max_hops": 2, "min_amount_usd": 10.0}), "")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := decode(t, res)
	inv := out["params"].(map[string]any)["invariants"].(map[string]any)
	if !strings.HasPrefix(inv["two_tables_agree"].(string), "1 links differ") {
		t.Errorf("two_tables_agree: %v", inv["two_tables_agree"])
	}
	by := out["unattributed_by_cut"].(map[string]any)["received"].(map[string]any)
	if got := by["history_incomplete"]; got != "10.000000" {
		t.Errorf("history_incomplete = %v, want the 10 A's history does not carry", got)
	}
}

// The ledger checks fail a report whose numbers do not close.
func TestInvariantsRefuseABrokenLedger(t *testing.T) {
	s := newSearch()
	s.cuts[received]["hub"].SetInt64(5)
	edges := edgesResult{total: map[direction]dirTotal{
		received: {raw: big.NewInt(100), rawAboveMin: big.NewInt(100)},
		sent:     {raw: new(big.Int), rawAboveMin: new(big.Int)},
	}}
	row := assetRow{recv: big.NewInt(100), sent: new(big.Int)}
	listed := map[direction]*big.Int{received: big.NewInt(100), sent: new(big.Int)}
	attributed := map[direction]*big.Int{received: big.NewInt(96), sent: new(big.Int)}
	err := reconcile(&reportOut{}, s, []direction{received}, edges, row, listed, attributed, 0)
	if err == nil || !strings.Contains(err.Error(), "conservation") {
		t.Errorf("err = %v, want conservation", err)
	}
	attributed[received] = big.NewInt(95)
	if err := reconcile(&reportOut{}, newSearchWith(s), []direction{received}, edges, row, listed, attributed, 0); err != nil {
		t.Errorf("a closed ledger was refused: %v", err)
	}
	s.caps["x"] = &capState{initial: map[string]*big.Int{"a": big.NewInt(3)}, used: map[string]*big.Int{"a": big.NewInt(4)}}
	if err := s.checkLinks(); err == nil || !strings.Contains(err.Error(), "over-attribution") {
		t.Errorf("err = %v, want over-attribution", err)
	}
}

// newSearchWith copies a search's cut amounts into a fresh one.
func newSearchWith(s *search) *search {
	n := newSearch()
	for d, by := range s.cuts {
		for r, v := range by {
			n.cuts[d][r].Set(v)
		}
	}
	return n
}
