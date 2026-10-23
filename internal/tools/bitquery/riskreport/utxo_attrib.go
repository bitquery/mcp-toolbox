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

// The UTXO attribution of the direct view, hop 1: an output's sources are its
// transaction's inputs, pro rata through each input's share of the total input
// value. All arithmetic is in exact satoshi (big.Int). The per-transaction
// split reuses split() (floor + largest-remainder), so every split's parts add
// up to the amount exactly.
//
// Received side (who funded the target A). For each receipt of v in a
// transaction T, T's inputs {Xi} with values {wi} (Sum wi = T's total input
// value) carry v in proportion: Xi gets v*wi/Sum, in whole satoshi. A as its
// own input (a self-send or change) takes a part too; that part is dropped from
// the counterparties (it is the target's own), but it is in the split, so the
// parts still add up to v.
//
// Sent side (where A's coins went). A is an input of value a to a transaction
// T'; A's share of T' is a / T'.sum_inputs. T'’s outputs {Yj} with values {oj}
// each receive oj * a / sum_inputs. The fee (sum_inputs - Sum oj) is a weight
// too, so A's share of the fee is split off into a fee bucket that no
// counterparty receives — the total reaching real outputs is therefore at most
// a. The output back to A (change) takes a part that is dropped, like the fee.
// So for each spend, Sum of the parts over the outputs and the fee equals a
// exactly, and the amount reaching real counterparties is a minus A's change
// and A's fee share.

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"
)

// btcFeeKey and btcSkipKey are split keys that are not a counterparty: the fee
// a spend paid (A's share of it), and a part carried by an input or output
// without a standard Bitcoin address. Both are dropped from the counterparties
// but are in the split, so the conservation sums still hold.
const (
	btcFeeKey  = "\x00fee"
	btcSkipKey = "\x00skip"
)

// btcPart is one input or output of a transaction: an address and its value in
// satoshi.
type btcPart struct {
	addr string
	val  *big.Int
}

// attributeReceipt splits a receipt of v over the inputs of its transaction, in
// proportion to each input's value. The parts (keyed by input address, with
// btcSkipKey for a non-standard address) add up to v exactly. An empty input
// set (a coinbase transaction) returns nothing: the receipt has no source.
func attributeReceipt(v *big.Int, inputs []btcPart) map[string]*big.Int {
	if v == nil || v.Sign() <= 0 || len(inputs) == 0 {
		return map[string]*big.Int{}
	}
	w := map[string]*big.Int{}
	total := new(big.Int)
	for _, in := range inputs {
		if in.val == nil || in.val.Sign() <= 0 {
			continue
		}
		k := in.addr
		if !bitcoinAddressRe.MatchString(k) {
			k = btcSkipKey
		}
		addTo(w, k, in.val)
		total.Add(total, in.val)
	}
	if total.Sign() <= 0 {
		return map[string]*big.Int{}
	}
	return split(v, w, total)
}

// attributeSpend splits A's input value a over the outputs of its transaction,
// in proportion to each output's value, with A's share of the fee (sum_inputs
// minus the outputs' total) split into a fee bucket. The parts (keyed by output
// address, btcFeeKey for the fee, btcSkipKey for a non-standard address) add up
// to a exactly. When sum_inputs is missing or not above the outputs' total, the
// split is over the outputs alone (the fee cannot be separated); the sums still
// hold.
func attributeSpend(a, sumInputs *big.Int, outputs []btcPart) map[string]*big.Int {
	if a == nil || a.Sign() <= 0 {
		return map[string]*big.Int{}
	}
	w := map[string]*big.Int{}
	outSum := new(big.Int)
	for _, o := range outputs {
		if o.val == nil || o.val.Sign() <= 0 {
			continue
		}
		k := o.addr
		if !bitcoinAddressRe.MatchString(k) {
			k = btcSkipKey
		}
		addTo(w, k, o.val)
		outSum.Add(outSum, o.val)
	}
	total := new(big.Int).Set(outSum)
	if sumInputs != nil && sumInputs.Cmp(outSum) > 0 {
		addTo(w, btcFeeKey, new(big.Int).Sub(sumInputs, outSum))
		total.Set(sumInputs)
	}
	if total.Sign() <= 0 {
		return map[string]*big.Int{}
	}
	return split(a, w, total)
}

// btcState is the per-report Bitcoin scratch: the receipt and spend
// transactions read by assetsBitcoin (window, largest first, capped), consumed
// by counterpartiesBitcoin, with the first receipt in the window and whether a
// coinbase receipt (no sources) was seen. recvTrunc / sentTrunc mean that side's
// counterparties are incomplete (the transaction list was capped, or a read was
// refused for its size and turned into a cut); degraded means the report cannot
// be marked complete.
type btcState struct {
	receipts  []btcTx
	spends    []btcTx
	recvTrunc bool
	sentTrunc bool
	degraded  bool
	firstIn   time.Time
	coinbase  bool
}

// btcSummary is an address's totals, from the lifetime-totals table (whole
// history) or from a summed windowed read.
type btcSummary struct {
	recv, sent        *big.Int
	firstIn, lastIn   string
	firstOut, lastOut string
}

// btcTx is one transaction of the target: its key, the target's amount in it
// (satoshi), the transaction time and, on the received side, its hash.
type btcTx struct {
	key  btcTxKey
	amt  *big.Int
	time string
	tx   string
}

// btcAggResult is the aggregate row of an address's receipts or spends.
type btcAggResult struct {
	amtW, amtAll      *big.Int
	n                 int64
	firstW, lastW     string
	firstAll, lastAll string
}

// btcHeader is the bare table name an http statement names in its
// X-Bitquery-Table header.
func btcHeader(table string) string {
	if i := strings.LastIndexByte(table, '.'); i >= 0 {
		return table[i+1:]
	}
	return table
}

// btcAgg runs one address-keyed read (receipts or spends) and parses its
// optional aggregate row and its per-transaction rows. truncated is whether the
// per-transaction list reached its cap. degraded is true when the read was
// refused for its size, time or the report's budget (so the caller degrades to a
// cut or, for a windowed summary, refuses) rather than failing the report; a
// non-limit failure is returned as an error.
func (e *executor) btcAgg(ctx context.Context, name, table, addr string, w window, withTx, withAgg bool) (btcAggResult, []btcTx, bool, bool, error) {
	agg := btcAggResult{amtW: new(big.Int), amtAll: new(big.Int)}
	maxRows, err := e.maxRows()
	if err != nil {
		return agg, nil, false, true, nil // the budget is already spent: degrade
	}
	stmt := btcAggStatement(table, addr, w, withTx, withAgg, maxRows, btcExpandTxCap+resultSlack, e.secs(ctx), btcExpandTxCap)
	rows, err := e.query(ctx, name, btcHeader(table), stmt)
	if err != nil {
		if classifyLimit(ctx, err) == notLimit {
			return agg, nil, false, false, err
		}
		return agg, nil, false, true, nil
	}
	var txs []btcTx
	nTx := 0
	for _, r := range rows {
		if asStr(r["kind"]) == "agg" {
			if agg.amtW, err = btcSatoshi(asStr(r["amt_w"])); err != nil {
				return agg, nil, false, false, err
			}
			if agg.amtAll, err = btcSatoshi(asStr(r["amt_all"])); err != nil {
				return agg, nil, false, false, err
			}
			agg.n = asInt(r["n_w"])
			agg.firstW, agg.lastW = emptyTime(asStr(r["first_w"])), emptyTime(asStr(r["last_w"]))
			agg.firstAll, agg.lastAll = emptyTime(asStr(r["first_all"])), emptyTime(asStr(r["last_all"]))
			continue
		}
		nTx++
		amt, perr := btcSatoshi(asStr(r["amt_w"]))
		if perr != nil {
			return agg, nil, false, false, perr
		}
		txs = append(txs, btcTx{key: btcTxKey{Block: asUint(r["blk"]), TxIndex: asUint(r["txi"])},
			amt: amt, time: emptyTime(asStr(r["first_w"])), tx: asStr(r["tx"])})
	}
	return agg, txs, nTx >= btcExpandTxCap, false, nil
}

// summaryAddressTransfers reads an address's lifetime totals from the
// aggregating-merge table (one read, keyed by address, cheap at any size). An
// unknown address returns zeros; a non-limit failure is an error.
func (e *executor) summaryAddressTransfers(ctx context.Context, addr string) (btcSummary, error) {
	s := btcSummary{recv: new(big.Int), sent: new(big.Int)}
	maxRows, err := e.maxRows()
	if err != nil {
		return s, err
	}
	rows, err := e.query(ctx, "address_totals", "", btcAddressTransfersStatement(addr, maxRows, e.secs(ctx)))
	if err != nil {
		return s, err
	}
	if len(rows) == 0 {
		return s, nil
	}
	r := rows[0]
	if s.recv, err = btcSatoshi(asStr(r["received"])); err != nil {
		return s, err
	}
	if s.sent, err = btcSatoshi(asStr(r["sent"])); err != nil {
		return s, err
	}
	s.firstIn, s.lastIn = emptyTime(asStr(r["first_in"])), emptyTime(asStr(r["last_in"]))
	s.firstOut, s.lastOut = emptyTime(asStr(r["first_out"])), emptyTime(asStr(r["last_out"]))
	return s, nil
}

// assetsBitcoin builds the one asset of the report (BTC) and stashes the
// per-transaction lists for counterpartiesBitcoin so the chain is read once. The
// summary (received, sent, balance, activity) comes from the cheap lifetime
// table on a whole-history report and from a summed windowed read otherwise; the
// per-transaction lists, which must be read from the flow tables either way,
// degrade to a cut when the address is too large to list. A windowed report
// whose summary read is refused for its size refuses honestly rather than
// returning a wrong or empty summary.
func (e *executor) assetsBitcoin(ctx context.Context, addr string, w window, windowed bool) (assetsResult, error) {
	var recvW, recvAll, sentW, sentAll *big.Int
	var firstIn, first, last string
	var recvTx, sentTx []btcTx
	var recvTrunc, sentTrunc bool

	if windowed {
		// Both totals and the list come from the bounded reads; a refusal for size
		// means the window is too wide to total honestly.
		rAgg, rTx, rTrunc, rDeg, err := e.btcAgg(ctx, "receipts", btcOutputsToAddress, addr, w, true, true)
		if err != nil {
			return assetsResult{}, err
		}
		sAgg, sTx, sTrunc, sDeg, err := e.btcAgg(ctx, "spends", btcInputsByAddress, addr, w, false, true)
		if err != nil {
			return assetsResult{}, err
		}
		if rDeg || sDeg {
			return assetsResult{}, agentErr("this address has too much history for a windowed direct report; narrow or omit the time window")
		}
		recvW, recvAll, sentW, sentAll = rAgg.amtW, rAgg.amtAll, sAgg.amtW, sAgg.amtAll
		firstIn = rAgg.firstW
		first, last = minTimeStr(rAgg.firstW, sAgg.firstW), maxTimeStr(rAgg.lastW, sAgg.lastW)
		recvTx, sentTx, recvTrunc, sentTrunc = rTx, sTx, rTrunc, sTrunc
	} else {
		s, err := e.summaryAddressTransfers(ctx, addr)
		if err != nil {
			return assetsResult{}, err
		}
		recvW, recvAll, sentW, sentAll = s.recv, s.recv, s.sent, s.sent
		firstIn = s.firstIn
		first, last = minTimeStr(s.firstIn, s.firstOut), maxTimeStr(s.lastIn, s.lastOut)
		// The lists still come from the flow tables (list only, no aggregate); a
		// refusal for size is a cut, not a failure — the summary is already correct.
		var rDeg, sDeg bool
		if _, recvTx, recvTrunc, rDeg, err = e.btcAgg(ctx, "receipts", btcOutputsToAddress, addr, w, true, false); err != nil {
			return assetsResult{}, err
		}
		if _, sentTx, sentTrunc, sDeg, err = e.btcAgg(ctx, "spends", btcInputsByAddress, addr, w, false, false); err != nil {
			return assetsResult{}, err
		}
		recvTrunc, sentTrunc = recvTrunc || rDeg, sentTrunc || sDeg
	}

	row := assetRow{
		asset: bitcoinAsset(),
		recv:  recvW, sent: sentW, recvAll: recvAll, sentAll: sentAll,
		nIn: int64(len(recvTx)), nOut: int64(len(sentTx)),
		first: first, last: last, firstAll: first, lastAll: last,
	}
	txset := map[btcTxKey]bool{}
	for _, t := range recvTx {
		txset[t.key] = true
	}
	for _, t := range sentTx {
		txset[t.key] = true
	}
	row.txs = int64(len(txset))
	if firstIn != "" {
		if t, perr := time.Parse(time.DateTime, firstIn); perr == nil {
			row.firstIn = t.UTC()
		}
	}
	e.btc = &btcState{receipts: recvTx, spends: sentTx, recvTrunc: recvTrunc, sentTrunc: sentTrunc,
		degraded: recvTrunc || sentTrunc, firstIn: row.firstIn}
	return assetsResult{rows: []assetRow{row}}, nil
}

// priceBitcoin reads BTC's USD price from the chain's price store at the pin, as
// one billed statement counted in the report's cost (named "price"). It returns
// 0 and no error when the store has no price for BTC then, so a miss is never
// read as 0 USD by the caller.
func (e *executor) priceBitcoin(ctx context.Context, pin time.Time) (float64, error) {
	maxRows, err := e.maxRows()
	if err != nil {
		return 0, err
	}
	// The price read touches no transfer table, only the price dictionary, so it
	// names no table in the X-Bitquery-Table header (unlike the Tron price read,
	// which rides a known transfer route): there is no evidence the Bitcoin
	// gateway routes or bills a table header, so an empty one cannot misroute it.
	rows, err := e.query(ctx, "price", "", btcPriceStatement(pin, maxRows, e.secs(ctx)))
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	s := strings.TrimSpace(asStr(rows[0]["Price"]))
	if s == "" {
		return 0, nil
	}
	f, perr := strconv.ParseFloat(s, 64)
	if perr != nil || f <= 0 || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, nil
	}
	return f, nil
}

// counterpartiesBitcoin attributes the target's receipts and spends to the
// counterparties they funded, reading each transaction's inputs (received) and
// outputs (sent) and splitting pro rata. A read refused for its size, time or
// the report's budget stops that side and becomes a cut (the expanded
// counterparties are kept, the rest are reported incomplete); a non-limit
// failure fails the report. Bitcoin has one asset, so there are no other-asset
// contacts: the other result is empty.
func (e *executor) counterpartiesBitcoin(ctx context.Context, in cpInput) (edgesResult, otherResult, error) {
	st := e.btc
	if st == nil {
		return edgesResult{}, otherResult{}, fmt.Errorf("the report read counterparties before assets")
	}
	recvCps, coinbase, recvCut, err := e.btcSide(ctx, received, st.receipts, btcTxInputs, "receipt_sources", false, in)
	if err != nil {
		return edgesResult{}, otherResult{}, err
	}
	sentCps, _, sentCut, err := e.btcSide(ctx, sent, st.spends, btcTxOutputs, "spend_destinations", true, in)
	if err != nil {
		return edgesResult{}, otherResult{}, err
	}
	st.coinbase = coinbase
	recvTrunc := st.recvTrunc || recvCut
	sentTrunc := st.sentTrunc || sentCut
	st.recvTrunc, st.sentTrunc = recvTrunc, sentTrunc
	st.degraded = st.degraded || recvTrunc || sentTrunc

	edges := edgesResult{total: map[direction]dirTotal{
		received: {raw: new(big.Int), rawAboveMin: new(big.Int)},
		sent:     {raw: new(big.Int), rawAboveMin: new(big.Int)},
	}}
	if !in.FirstIn.IsZero() {
		edges.firstReceipt = in.FirstIn.UTC().Format(time.DateTime)
	}
	for _, s := range []struct {
		dir   direction
		cps   []counterparty
		trunc bool
	}{{received, recvCps, recvTrunc}, {sent, sentCps, sentTrunc}} {
		rows, tot := btcDirTotals(s.cps, in.Limit, s.trunc)
		edges.rows = append(edges.rows, rows...)
		edges.total[s.dir] = tot
	}
	return edges, otherResult{total: map[direction]int64{}}, nil
}

// btcDirTotals sorts a direction's counterparties by amount, keeps the largest
// limit, and totals ALL of them (the totals cover every counterparty, as the
// Tron counterparty statement's window totals do). When more were found than
// are kept, or the per-transaction list was truncated upstream, the direction's
// counterparty count is set above the kept count so the report reports a cut.
func btcDirTotals(cps []counterparty, limit int, trunc bool) ([]counterparty, dirTotal) {
	tot := dirTotal{raw: new(big.Int), rawAboveMin: new(big.Int)}
	for _, c := range cps {
		tot.transfers += c.Transfers
		tot.raw.Add(tot.raw, c.Raw)
		tot.rawAboveMin.Add(tot.rawAboveMin, c.RawAboveMin)
	}
	sort.SliceStable(cps, func(i, j int) bool {
		if c := cps[i].Raw.Cmp(cps[j].Raw); c != 0 {
			return c > 0
		}
		return cps[i].Address < cps[j].Address
	})
	found := len(cps)
	if limit > 0 && found > limit {
		cps = cps[:limit]
	}
	tot.counterparties = int64(found)
	if trunc && tot.counterparties <= int64(len(cps)) {
		tot.counterparties = int64(len(cps)) + 1
	}
	return cps, tot
}

// btcSide reads the parts (inputs or outputs) of a set of transactions, in
// small chunks that stay under the gateway's result cap, attributes each
// transaction and accumulates per counterparty. hadCoinbase is whether a
// received transaction had no inputs. truncated is whether a chunk was refused
// for its size, time or the report's budget: the chunks read so far are kept and
// attributed, the rest become a cut, so a large address degrades to an
// incomplete report instead of a hard error.
func (e *executor) btcSide(ctx context.Context, dir direction, txs []btcTx, partsTable, name string, withTx bool, in cpInput) ([]counterparty, bool, bool, error) {
	partsByTx := map[btcTxKey][]btcPart{}
	siByTx := map[btcTxKey]*big.Int{}
	hashByTx := map[btcTxKey]string{}
	keys := make([]btcTxKey, len(txs))
	for i, t := range txs {
		keys[i] = t.key
	}
	truncated := false
	processed := len(keys)
	for i := 0; i < len(keys); i += btcPartsChunk {
		chunk := keys[i:min(i+btcPartsChunk, len(keys))]
		maxRows, err := e.maxRows()
		if err != nil {
			// The report's row budget is spent: the rest of this side is a cut.
			truncated, processed = true, i
			break
		}
		stmt := btcTxPartsStatement(partsTable, chunk, withTx, maxRows, btcPartsResultCap, e.secs(ctx))
		rows, err := e.query(ctx, name, btcHeader(partsTable), stmt)
		if err != nil {
			if classifyLimit(ctx, err) == notLimit {
				return nil, false, false, err
			}
			// A result/row/time/budget limit: stop here, the rest is a cut.
			truncated, processed = true, i
			break
		}
		for _, r := range rows {
			k := btcTxKey{Block: asUint(r["blk"]), TxIndex: asUint(r["txi"])}
			val, perr := btcSatoshi(asStr(r["val"]))
			if perr != nil {
				return nil, false, false, perr
			}
			partsByTx[k] = append(partsByTx[k], btcPart{addr: asStr(r["addr"]), val: val})
			if _, ok := siByTx[k]; !ok {
				si, _ := btcSatoshi(asStr(r["si"]))
				siByTx[k] = si
			}
			if withTx {
				if h := asStr(r["tx"]); h != "" {
					if _, ok := hashByTx[k]; !ok {
						hashByTx[k] = h
					}
				}
			}
		}
	}
	accs := map[string]*cpAccum{}
	hadCoinbase := false
	// Only the transactions whose chunk was read are attributed; the rest were
	// cut (truncated) and their flow is unanalysed, not coinbase.
	for _, t := range txs[:processed] {
		parts := partsByTx[t.key]
		var alloc map[string]*big.Int
		if dir == received {
			if len(parts) == 0 {
				hadCoinbase = true
				continue
			}
			alloc = attributeReceipt(t.amt, parts)
		} else {
			if len(parts) == 0 {
				continue
			}
			alloc = attributeSpend(t.amt, siByTx[t.key], parts)
		}
		hash := t.tx
		if withTx {
			hash = hashByTx[t.key]
		}
		for addr, raw := range alloc {
			if raw.Sign() <= 0 || addr == in.Addr || strings.HasPrefix(addr, "\x00") {
				continue
			}
			acc := accs[addr]
			if acc == nil {
				acc = &cpAccum{}
				accs[addr] = acc
			}
			acc.add(raw, t.time, hash)
		}
	}
	out := make([]counterparty, 0, len(accs))
	for addr, acc := range accs {
		out = append(out, acc.counterparty(dir, addr, in))
	}
	return out, hadCoinbase, truncated, nil
}

// btcContrib is one transaction's attributed amount to a counterparty.
type btcContrib struct {
	raw *big.Int
	t   string
	tx  string
}

type cpAccum struct {
	items []btcContrib
}

func (a *cpAccum) add(raw *big.Int, t, tx string) {
	a.items = append(a.items, btcContrib{raw: new(big.Int).Set(raw), t: t, tx: tx})
}

// btcEvidenceCap bounds the transaction hashes kept per counterparty (as the
// Tron counterparty statement keeps ten).
const btcEvidenceCap = 10

// counterparty aggregates one counterparty's per-transaction contributions into
// the same shape the Tron path produces: the thresholds (min, dust) are applied
// per contribution, as the Tron statement applies them per transfer.
func (a *cpAccum) counterparty(dir direction, addr string, in cpInput) counterparty {
	c := counterparty{Dir: dir, Address: addr, Raw: new(big.Int), RawAboveMin: new(big.Int), RawMax: new(big.Int), RawEarly: new(big.Int)}
	var earlyCut time.Time
	early := dir == sent && !in.FirstIn.IsZero()
	if early {
		earlyCut = in.FirstIn.Add(time.Duration(in.EarlyDays) * 24 * time.Hour)
	}
	for _, it := range a.items {
		c.Transfers++
		c.Raw.Add(c.Raw, it.raw)
		if it.raw.Cmp(c.RawMax) > 0 {
			c.RawMax.Set(it.raw)
		}
		if in.MinRaw != nil && it.raw.Cmp(in.MinRaw) > 0 {
			c.RawAboveMin.Add(c.RawAboveMin, it.raw)
			c.TransfersAboveMin++
		}
		if in.DustRaw != nil {
			if it.raw.Cmp(in.DustRaw) <= 0 {
				c.DustTransfers++
			} else if it.tx != "" && len(c.TxAboveDust) < btcEvidenceCap {
				c.TxAboveDust = append(c.TxAboveDust, it.tx)
			}
		}
		if early && it.t != "" {
			if tt, err := time.Parse(time.DateTime, it.t); err == nil && !tt.After(earlyCut) {
				c.RawEarly.Add(c.RawEarly, it.raw)
			}
		}
		if it.tx != "" && len(c.Tx) < btcEvidenceCap {
			c.Tx = append(c.Tx, it.tx)
		}
		if it.t != "" {
			if c.First == "" || it.t < c.First {
				c.First = it.t
			}
			if it.t > c.Last {
				c.Last = it.t
			}
		}
	}
	return c
}

// minTimeStr / maxTimeStr choose the earlier non-empty / later of two formatted
// times (lexical order works on the fixed "YYYY-MM-DD HH:MM:SS" shape).
func minTimeStr(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	case a < b:
		return a
	default:
		return b
	}
}

func maxTimeStr(a, b string) string {
	if a > b {
		return a
	}
	return b
}
