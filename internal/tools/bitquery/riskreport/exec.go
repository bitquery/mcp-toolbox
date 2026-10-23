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
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/googleapis/mcp-toolbox/internal/util"
)

type executor struct {
	src    httpSource
	labels sqlSource
	th     Thresholds
	cost   costOut
	// last is how long the last statement of each kind took.
	last map[string]time.Duration
	// btc is the Bitcoin scratch: the receipt and spend transactions
	// assetsBitcoin read, which counterpartiesBitcoin consumes (nil off Bitcoin).
	btc *btcState
}

// errNoTime stops a statement the report's deadline leaves no time for.
var errNoTime = fmt.Errorf("the report's deadline is too close to start another statement")

// minStartSeconds is the least time left in which a statement is started.
const minStartSeconds = 5

// secs is a statement's max_execution_time: statement_max_seconds, or the
// whole seconds left before the report's deadline when fewer, at least 1.
func (e *executor) secs(ctx context.Context) int {
	s := e.th.StatementMaxSeconds
	if dl, ok := ctx.Deadline(); ok {
		s = min(s, int(time.Until(dl)/time.Second))
	}
	return max(1, s)
}

// mayStart refuses a statement when the time left before the report's deadline
// is under the larger of 5 seconds and the last statement of the same kind.
func (e *executor) mayStart(ctx context.Context, kind string) error {
	dl, ok := ctx.Deadline()
	if !ok {
		return nil
	}
	need := max(minStartSeconds*time.Second, e.last[kind])
	if time.Until(dl) < need {
		return errNoTime
	}
	return nil
}

func (e *executor) took(kind string, d time.Duration) {
	if e.last == nil {
		e.last = map[string]time.Duration{}
	}
	e.last[kind] = d
}

// errBudget stops a report whose statements have read its row budget.
var errBudget = fmt.Errorf("the report reached its row budget before it was complete; narrow it with after_time / before_time or a single asset")

func (e *executor) maxRows() (int64, error) {
	left := e.th.ReportRowsBudget - e.cost.RowsRead
	if left <= 0 {
		return 0, errBudget
	}
	return min(left, e.th.StatementMaxRows), nil
}

// query posts one statement to the chain source and returns its rows. header
// is the table the statement reads, as the X-Bitquery-Table header names it.
func (e *executor) query(ctx context.Context, name, header, statement string) ([]map[string]any, error) {
	if err := e.mayStart(ctx, name); err != nil {
		return nil, err
	}
	start := time.Now()
	u, err := url.Parse(strings.TrimRight(e.src.HttpBaseURL(), "/") + "/")
	if err != nil {
		return nil, fmt.Errorf("error building the request URL: %w", err)
	}
	q := u.Query()
	for k, v := range e.src.HttpQueryParams() {
		q.Set(k, v)
	}
	// Bitquery: each statement carries its own billing query_id, built from the
	// caller identity, as the http tool does for its one statement.
	var qid string
	var stamp bool
	if paths := e.src.BitqueryLegacyBillingPaths(); paths != "" {
		qid, stamp = util.BitqueryLegacyClickhouseQueryID(ctx, paths)
	} else {
		qid, stamp = util.BitqueryClickhouseQueryID(ctx)
	}
	if stamp {
		q.Set("query_id", qid)
	}
	// A statement answers FORMAT JSON, one document: with the answer held to
	// the end of the query, a limit or a timeout that fires after the first
	// block is an HTTP error (classified as a limit), not a body cut short.
	// Set here, whatever the source's own parameters say.
	q.Set("wait_end_of_query", "1")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(statement))
	if err != nil {
		return nil, fmt.Errorf("error creating the request: %w", err)
	}
	for k, v := range e.src.HttpDefaultHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set("X-Bitquery-Table", header)
	resp, err := e.src.RunRequest(ctx, req)
	e.took(name, time.Since(start))
	if err != nil {
		// A refused or failed statement is listed too: it was sent, and may
		// have read before it stopped.
		e.cost.Statements++
		e.cost.Detail = append(e.cost.Detail, statementCost{Name: name + " (failed)", ElapsedMS: time.Since(start).Milliseconds()})
		return nil, err
	}
	doc, ok := resp.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("unexpected answer to the %s statement", name)
	}
	sc := statementCost{Name: name, ElapsedMS: time.Since(start).Milliseconds()}
	if st, ok := doc["statistics"].(map[string]any); ok {
		sc.RowsRead = asInt(st["rows_read"])
		sc.BytesRead = asInt(st["bytes_read"])
	}
	e.cost.Statements++
	e.cost.RowsRead += sc.RowsRead
	e.cost.BytesRead += sc.BytesRead
	e.cost.Detail = append(e.cost.Detail, sc)
	data, _ := doc["data"].([]any)
	rows := make([]map[string]any, 0, len(data))
	for _, d := range data {
		if r, ok := d.(map[string]any); ok {
			rows = append(rows, r)
		}
	}
	return rows, nil
}

func asInt(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case uint64:
		return int64(n)
	case int:
		return int64(n)
	case uint32:
		return int64(n)
	case int32:
		return int64(n)
	case uint8:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	}
	return 0
}

func asStr(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case nil:
		return ""
	case []byte:
		return string(s)
	}
	return fmt.Sprint(v)
}

func asBig(v any) *big.Int {
	s := strings.TrimSpace(asStr(v))
	if s == "" {
		return new(big.Int)
	}
	b, ok := new(big.Int).SetString(s, 10)
	if !ok {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			b, _ = big.NewFloat(f).Int(nil)
			return b
		}
		return new(big.Int)
	}
	return b
}

func asStrings(v any) []string {
	arr, _ := v.([]any)
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		if s := asStr(x); s != "" {
			out = append(out, s)
		}
	}
	return out
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

// labelRows reads the labels of a set of addresses, in batches. The addresses
// are validated against the chain's own address form so a batch carries only
// well-formed literals (the Tron form drops every Bitcoin address, and the
// reverse): on Bitcoin that guard would return no labels at all.
func (e *executor) labelRows(ctx context.Context, chain string, addrs []string) (map[string][]labelRow, error) {
	valid := tronAddressRe.MatchString
	if chain == "bitcoin" {
		valid = bitcoinAddressRe.MatchString
	}
	seen := map[string]bool{}
	var uniq []string
	for _, a := range addrs {
		if !seen[a] && valid(a) {
			seen[a] = true
			uniq = append(uniq, a)
		}
	}
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
		e.cost.Statements++
		// A real read of the labels table underlies this, but its rows_read /
		// bytes_read stay 0: the labels source returns result rows only (through
		// database/sql), not the statistics block the http chain source exposes,
		// so the scan stats are dropped at the source boundary and cannot be
		// captured here without the source surfacing them.
		e.cost.Detail = append(e.cost.Detail, statementCost{Name: "labels", ElapsedMS: time.Since(start).Milliseconds()})
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
