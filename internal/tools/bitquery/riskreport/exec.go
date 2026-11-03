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

// Running the statements: one at a time, each billed to the caller through its
// own query_id, each counted in the report's cost, and none started once the
// report's row budget is spent, or when the report's deadline is too close for
// it. The deadline is the server's too: a statement's max_execution_time is the
// time left, and the server cancels a read-only HTTP query whose client went
// away, so a statement the report gave up on does not run on, billed.

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/googleapis/mcp-toolbox/internal/bitquerylabels"
	"github.com/googleapis/mcp-toolbox/internal/tools/bitquery/chainexec"
)

type executor struct {
	*chainexec.Exec
	th     Thresholds
	labels sqlSource
	// refreshLeft is how many more addresses this report may send to the
	// labels-query-service; refreshSet says it was read from the hook.
	refreshLeft int
	refreshSet  bool
	// btc is the Bitcoin scratch: the receipt and spend transactions
	// assetsBitcoin read, which counterpartiesBitcoin consumes (nil off Bitcoin).
	btc *btcState
}

// newExecutor builds an executor: the shared runner (package chainexec) carries the
// chain source and the per-statement / per-report budget knobs from the thresholds;
// the labels source and the chain-specific state stay on the executor.
func newExecutor(src httpSource, labels sqlSource, th Thresholds) *executor {
	return &executor{
		Exec: &chainexec.Exec{
			Src:                 src,
			StatementMaxSeconds: th.StatementMaxSeconds,
			StatementMaxRows:    th.StatementMaxRows,
			ReportRowsBudget:    th.ReportRowsBudget,
		},
		th:     th,
		labels: labels,
	}
}

// The statement runner lives in package chainexec; executor embeds *chainexec.Exec,
// which promotes Query / Secs / MayStart / Took / MaxRows and the Cost ledger. These
// aliases and lowercase forwarders keep the risk-report code and its tests calling
// the same names they did before the lift, so the report stays byte-identical.
var (
	errNoTime = chainexec.ErrNoTime
	errBudget = chainexec.ErrBudget
	asInt     = chainexec.AsInt
	asStr     = chainexec.AsStr
	asBig     = chainexec.AsBig
	asStrings = chainexec.AsStrings
)

const minStartSeconds = chainexec.MinStartSeconds

func (e *executor) secs(ctx context.Context) int                    { return e.Secs(ctx) }
func (e *executor) mayStart(ctx context.Context, kind string) error { return e.MayStart(ctx, kind) }
func (e *executor) took(kind string, d time.Duration)               { e.Took(kind, d) }
func (e *executor) maxRows() (int64, error)                         { return e.MaxRows() }
func (e *executor) query(ctx context.Context, name, header, statement string) ([]map[string]any, error) {
	return e.Query(ctx, name, header, statement)
}

func dirOf(v any) direction {
	if asStr(v) == "out" {
		return sent
	}
	return received
}

// emptyTime turns ClickHouse's zero DateTime into "".
func emptyTime(s string) string {
	if s == "" || strings.HasPrefix(s, "1970-01-01") {
		return ""
	}
	return s
}

// assetRow is one asset of the address.
type assetRow struct {
	asset             tronAsset
	recv, sent        *big.Int
	recvAll, sentAll  *big.Int
	nIn, nOut, txs    int64
	first, last       string
	firstAll, lastAll string
	// firstIn is the first receipt in the window (zero: none).
	firstIn time.Time
}

func (a assetRow) out() assetOut {
	return assetOut{Asset: a.asset.Key, Symbol: a.asset.Symbol, Contract: a.asset.Contract, Decimals: a.asset.Decimals,
		Received: formatUnits(a.recv, a.asset.Decimals), Sent: formatUnits(a.sent, a.asset.Decimals), Transfer: a.nIn + a.nOut, TxCount: a.txs}
}

type assetsResult struct {
	rows      []assetRow
	truncated bool
}

func (e *executor) assets(ctx context.Context, addr string, w window) (assetsResult, error) {
	maxRows, err := e.maxRows()
	if err != nil {
		return assetsResult{}, err
	}
	rows, err := e.query(ctx, "assets", headerSender, assetsStatement(addr, w, maxRows, e.secs(ctx)))
	if err != nil {
		return assetsResult{}, err
	}
	var out assetsResult
	out.truncated = len(rows) >= 200
	for _, r := range rows {
		a := tronAsset{Symbol: asStr(r["Symbol"]), Decimals: int(asInt(r["Decimals"]))}
		switch {
		case asInt(r["Native"]) == 1:
			a.Native, a.Key = true, "TRX"
			if a.Symbol == "" {
				a.Symbol = "TRX"
			}
		case asStr(r["Contract"]) != "":
			// The contract and the TRC-10 id may be written into a later
			// statement: an asset whose value fails the check is skipped.
			if !tronAddressRe.MatchString(asStr(r["Contract"])) {
				continue
			}
			a.Contract = asStr(r["Contract"])
			a.Key = a.Contract
		default:
			if !trc10IDRe.MatchString(asStr(r["AssetId"])) {
				continue
			}
			a.TRC10 = asStr(r["AssetId"])
			a.Key = "trc10:" + a.TRC10
		}
		row := assetRow{
			asset: a, recv: asBig(r["Received"]), sent: asBig(r["Sent"]), recvAll: asBig(r["Received_All"]), sentAll: asBig(r["Sent_All"]),
			nIn: asInt(r["n_in"]), nOut: asInt(r["n_out"]), txs: asInt(r["txs"]),
			first: emptyTime(asStr(r["First"])), last: emptyTime(asStr(r["Last"])),
			firstAll: emptyTime(asStr(r["First_All"])), lastAll: emptyTime(asStr(r["Last_All"])),
		}
		// The first receipt goes into the counterparty statement as a literal:
		// only a time that parses is kept, and it is formatted again there.
		if s := emptyTime(asStr(r["First_In"])); s != "" {
			t, err := time.Parse(time.DateTime, s)
			if err != nil {
				return assetsResult{}, fmt.Errorf("the assets statement returned a first receipt that is not a time")
			}
			row.firstIn = t.UTC()
		}
		out.rows = append(out.rows, row)
	}
	return out, nil
}

// price reads the report asset's USD price from the chain's price store at the
// pin, as one billed statement counted in the report's cost (named "price"). It
// returns 0 and no error when the store has no price for the asset, so a miss
// is never read as 0 USD by the caller. The header names the sender table, as a
// route the gateway knows; the statement reads only the dictionary.
func (e *executor) price(ctx context.Context, asset tronAsset, pin time.Time) (float64, error) {
	maxRows, err := e.maxRows()
	if err != nil {
		return 0, err
	}
	rows, err := e.query(ctx, "price", headerSender, priceStatement(asset, pin, maxRows, e.secs(ctx)))
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

// pickAsset returns the report asset: the one asked for, or else the one with
// the largest USD volume in the window among the assets with a price, or else
// the one with the most transfers.
func pickAsset(a assetsResult, req request, prices map[string]float64) (tronAsset, assetRow, bool) {
	if req.assetSet {
		for _, r := range a.rows {
			if r.asset.Key == req.asset.Key {
				return r.asset, r, r.nIn+r.nOut > 0
			}
		}
		return req.asset, assetRow{}, false
	}
	var best *assetRow
	bestUSD := -1.0
	for i := range a.rows {
		r := &a.rows[i]
		if r.nIn+r.nOut == 0 {
			continue
		}
		p := prices[r.asset.Key]
		if p <= 0 {
			continue
		}
		vol, _ := new(big.Float).SetInt(new(big.Int).Add(r.recv, r.sent)).Float64()
		usd := vol * p
		for range r.asset.Decimals {
			usd /= 10
		}
		if usd > bestUSD {
			best, bestUSD = r, usd
		}
	}
	if best == nil {
		for i := range a.rows {
			r := &a.rows[i]
			if r.nIn+r.nOut > 0 && (best == nil || r.nIn+r.nOut > best.nIn+best.nOut) {
				best = r
			}
		}
	}
	if best == nil {
		return tronAsset{}, assetRow{}, false
	}
	return best.asset, *best, true
}

type dirTotal struct {
	counterparties int64
	transfers      int64
	raw            *big.Int
	rawAboveMin    *big.Int
}

type edgesResult struct {
	rows         []counterparty
	total        map[direction]dirTotal
	firstReceipt string
}

func (r edgesResult) complete() bool {
	for _, d := range []direction{received, sent} {
		if r.total[d].counterparties > int64(countDir(r.rows, d)) {
			return false
		}
	}
	return true
}

type otherResult struct {
	rows  []contact
	total map[direction]int64
}

// counterparties runs the counterparty statement and splits its rows: the
// report asset's counterparties and the other assets' contacts.
func (e *executor) counterparties(ctx context.Context, in cpInput) (edgesResult, otherResult, error) {
	maxRows, err := e.maxRows()
	if err != nil {
		return edgesResult{}, otherResult{}, err
	}
	in.MaxRows, in.MaxSecs = maxRows, e.secs(ctx)
	rows, err := e.query(ctx, "counterparties", headerSender, counterpartiesStatement(in))
	if err != nil {
		return edgesResult{}, otherResult{}, err
	}
	edges := edgesResult{total: map[direction]dirTotal{
		received: {raw: new(big.Int), rawAboveMin: new(big.Int)},
		sent:     {raw: new(big.Int), rawAboveMin: new(big.Int)},
	}}
	if !in.FirstIn.IsZero() {
		edges.firstReceipt = in.FirstIn.UTC().Format(time.DateTime)
	}
	other := otherResult{total: map[direction]int64{}}
	for _, r := range rows {
		d := dirOf(r["Dir"])
		addr := asStr(r["Counterparty"])
		if !tronAddressRe.MatchString(addr) {
			return edgesResult{}, otherResult{}, fmt.Errorf("the counterparties statement returned an address that is not a Tron address")
		}
		if asInt(r["R"]) == 1 {
			edges.rows = append(edges.rows, counterparty{
				Dir: d, Address: addr, Transfers: asInt(r["n"]), Raw: asBig(r["Raw"]),
				TransfersAboveMin: asInt(r["n_min"]), RawAboveMin: asBig(r["Raw_Above_Min"]), DustTransfers: asInt(r["n_dust"]),
				RawMax: asBig(r["Raw_Max"]), RawEarly: asBig(r["Raw_Early"]), First: asStr(r["First"]), Last: asStr(r["Last"]),
				TxAboveDust: asStrings(r["Tx_Above_Dust"]), Tx: asStrings(r["Tx"]),
			})
			edges.total[d] = dirTotal{counterparties: asInt(r["Dir_Counterparties"]), transfers: asInt(r["Dir_Transfers"]),
				raw: asBig(r["Dir_Raw"]), rawAboveMin: asBig(r["Dir_Raw_Above_Min"])}
			continue
		}
		other.rows = append(other.rows, contact{Dir: d, Address: addr, Transfers: asInt(r["n"]), NativeDust: asInt(r["n_native_dust"]),
			NativeAboveDust: asInt(r["n_native_above"]), Imitations: asStrings(r["Imitations"]), First: asStr(r["First"]), Tx: asStrings(r["Tx"])})
		other.total[d] = asInt(r["Dir_Counterparties"])
	}
	return edges, other, nil
}

// refreshLabels asks the labels-query-service for the addresses before their
// labels are read, when the labels source carries the hook, so a label the
// service finds is in the table the read then scans. One report sends at most
// the hook's cap over every read it makes, the earlier reads first, so the
// address and its direct counterparties come before the deeper hops. The
// refresh ends before the label read's own start floor, so it never takes the
// time the read needs; with under a second to give it, it is skipped. A
// failure is the hook's to log and count: the read runs either way.
func (e *executor) refreshLabels(ctx context.Context, chain string, addrs []string) {
	hook, ok := e.labels.(bitquerylabels.HookSource)
	if !ok || len(addrs) == 0 {
		return
	}
	pre := hook.BitqueryLabelsPreprocessor()
	if pre == nil {
		return
	}
	if !e.refreshSet {
		e.refreshLeft = pre.MaxAddresses()
		e.refreshSet = true
	}
	if pre.MaxAddresses() > 0 && e.refreshLeft <= 0 {
		return
	}
	if dl, ok := ctx.Deadline(); ok {
		end := dl.Add(-minStartSeconds * time.Second)
		if time.Until(end) < time.Second {
			return
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, end)
		defer cancel()
	}
	start := time.Now()
	sent := pre.Refresh(ctx, addrs, []string{chain}, e.refreshLeft)
	e.took("labels_refresh", time.Since(start))
	e.Cost.Detail = append(e.Cost.Detail, statementCost{Name: "labels_refresh", ElapsedMS: time.Since(start).Milliseconds()})
	e.refreshLeft -= sent
}

// labelRows reads the labels of a set of addresses, in batches. The addresses
// are validated against the chain's own address form so a batch carries only
// well-formed literals (the Tron form drops every Bitcoin address, and the
// reverse): on Bitcoin that guard would return no labels at all.
func (e *executor) labelRows(ctx context.Context, chain string, addrs []string) (map[string][]labelRow, error) {
	valid := tronAddressRe.MatchString
	switch chain {
	case "bitcoin":
		valid = bitcoinAddressRe.MatchString
	case "ethereum":
		valid = ethAddressRe.MatchString
	}
	seen := map[string]bool{}
	var uniq []string
	for _, a := range addrs {
		if !seen[a] && valid(a) {
			seen[a] = true
			uniq = append(uniq, a)
		}
	}
	e.refreshLabels(ctx, chain, uniq)
	out := map[string][]labelRow{}
	for i := 0; i < len(uniq); i += e.th.LabelBatch {
		batch := uniq[i:min(i+e.th.LabelBatch, len(uniq))]
		if err := e.mayStart(ctx, "labels"); err != nil {
			return nil, err
		}
		start := time.Now()
		res, err := e.labels.RunSQL(ctx, labelsStatement(chain, batch, e.th.StatementMaxRows, e.secs(ctx)), nil)
		e.took("labels", time.Since(start))
		if err != nil {
			return nil, err
		}
		e.Cost.Statements++
		// A real read of the labels table underlies this, but its rows_read /
		// bytes_read stay 0: the labels source returns result rows only (through
		// database/sql), not the statistics block the http chain source exposes,
		// so the scan stats are dropped at the source boundary and cannot be
		// captured here without the source surfacing them.
		e.Cost.Detail = append(e.Cost.Detail, statementCost{Name: "labels", ElapsedMS: time.Since(start).Milliseconds()})
		rows, _ := res.([]any)
		for _, x := range rows {
			r, ok := x.(map[string]any)
			if !ok {
				continue
			}
			lr := labelRow{Address: asStr(r["address"]), LabelType: asStr(r["label_type"]), Value: asStr(r["label_value"]),
				Source: asStr(r["source"]), RecordedAt: asStr(r["recorded_at"])}
			out[lr.Address] = append(out[lr.Address], lr)
		}
	}
	return out, nil
}
