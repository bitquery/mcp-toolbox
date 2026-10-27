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

// The Ethereum statements and exec of the direct view. Ethereum is schema-close
// to Tron — the same transfers_sender / transfers_receiver tables with
// Transfer_Currency_Native, Transfer_Amount and a per-row success flag — so the
// statements mirror tron.go with three deltas: the address codec is 20-byte hex
// (unhex / concat('0x', hex) rather than base58), there is no TRC-10 asset id
// (an asset is native ETH or an ERC-20 contract), and the price store is the ETH
// range-keyed dictionary with the native key cityHash64(''). The exec twins
// (assetsEth / priceEth / counterpartiesEth) parse the same row shapes the Tron
// exec does, so report.go treats an Ethereum report exactly as a Tron one — only
// the dispatch picks these. Every value written into a statement is a validated
// 0x address, a decimal integer or a formatted time: the kind builds SQL by
// hand, so nothing the caller or the data supplies reaches a statement
// unchecked. The gateway is the modern fleet (ClickHouse 26.8), so the Tron
// settings() apply unchanged.
//
// Multi-hop (view all) is not built here yet: a request for it is coerced to the
// direct view in parse, as on Bitcoin. A5 is the direct view only.

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ethAddressRe accepts a 0x-prefixed 20-byte hex address. The hex is matched
// case-insensitively; parse lower-cases the target and every counterparty comes
// back lower-cased from the statement, so a comparison or a labels lookup never
// turns on case. It REJECTS a Tron 'T…' and a Bitcoin address (neither is 0x
// hex), so a wrong-chain address is refused before any statement runs.
var ethAddressRe = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

const (
	ethSender   = "eth_api.transfers_sender"
	ethReceiver = "eth_api.transfers_receiver"
)

// ethDecimals is native ETH's precision: 18 decimals, so one unit is a wei.
const ethDecimals = 18

// ethPrefixSkip is the fixed network prefix of every Ethereum address: the '0x'
// that each one starts with is not a resemblance, so the look-alike rule counts
// matching leading characters only after it.
const ethPrefixSkip = 2

// ethRaw is the FixedString(20) form of a validated 0x address: the 40 hex
// characters after 0x, lower-cased, through unhex. unhex is case-insensitive, so
// the lower-casing is only for a stable statement text.
func ethRaw(addr string) string {
	return fmt.Sprintf("unhex('%s')", strings.ToLower(strings.TrimPrefix(strings.ToLower(addr), "0x")))
}

// ethHex encodes a 20-byte address column as a 0x hex string.
func ethHex(col string) string {
	return fmt.Sprintf("concat('0x', lower(hex(%s)))", col)
}

// ethNativeAsset is the native ETH asset carrier.
func ethNativeAsset() tronAsset {
	return tronAsset{Key: "ETH", Native: true, Symbol: "ETH", Decimals: ethDecimals}
}

// ethAssetCond is the asset filter of a transfer row on Ethereum. Native ETH
// keys off Transfer_Currency_Native (as on Tron); an ERC-20 keys off its
// contract, which is checked here as well as where it is read, so an asset that
// skipped a check matches no row instead of reaching the statement. There is no
// TRC-10 form on Ethereum.
func ethAssetCond(a tronAsset) string {
	switch {
	case a.Native:
		return "Transfer_Currency_Native = 1"
	case a.Contract != "" && ethAddressRe.MatchString(a.Contract):
		return "Transfer_Currency_SmartContract = " + ethRaw(a.Contract)
	default:
		return "0"
	}
}

// parseEthAsset reads the asset parameter: "" (choose), ETH, or an ERC-20
// contract address (0x…). It lower-cases a contract, as every address is.
func parseEthAsset(s string) (tronAsset, bool, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return tronAsset{}, false, nil
	case strings.EqualFold(s, "ETH"):
		return ethNativeAsset(), true, nil
	}
	low := strings.ToLower(s)
	if ethAddressRe.MatchString(low) {
		return tronAsset{Key: low, Contract: low}, true, nil
	}
	return tronAsset{}, false, fmt.Errorf("asset must be ETH or a token contract address (0x followed by 40 hex characters)")
}

// ethPriceKey is the report asset's key text in the ETH price store: native ETH
// is ” (verified: cityHash64(”) resolves to the same price as the WETH
// contract key), an ERC-20 its lower-case hex contract without 0x. Both forms
// are values already validated on the asset, so each is written as a literal.
func ethPriceKey(a tronAsset) string {
	if a.Native {
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(a.Contract, "0x"))
}

// ethAssetsStatement totals every asset of the address: in the window and over
// the whole history up to the pin (the pin bounds both table reads, so the
// balance and the window totals end at the same moment). It groups by
// (native, contract) — Ethereum has no TRC-10 asset id — and First_In is the
// first receipt of each asset in the window, which the counterparty statement
// takes as a literal instead of reading the receipts again.
func ethAssetsStatement(addr string, w window, maxRows int64, maxSeconds int) string {
	inWin := "1"
	if c := w.lower(); c != "" {
		inWin = strings.TrimPrefix(c, " AND ")
	}
	return fmt.Sprintf(`SELECT Native, %[3]s AS Contract, Symbol, Decimals,
  toString(recv_w) AS Received, toString(sent_w) AS Sent, toString(recv_all) AS Received_All, toString(sent_all) AS Sent_All,
  n_in, n_out, txs, First, Last, First_All, Last_All, First_In
FROM (
  SELECT Native, SC,
    any(Symbol0) AS Symbol, any(Decimals0) AS Decimals,
    sumIf(Amount, Dir = 'in' AND W) AS recv_w, sumIf(Amount, Dir = 'out' AND W) AS sent_w,
    sumIf(Amount, Dir = 'in') AS recv_all, sumIf(Amount, Dir = 'out') AS sent_all,
    countIf(Dir = 'in' AND W) AS n_in, countIf(Dir = 'out' AND W) AS n_out,
    uniqExactIf(Tx, W) AS txs,
    toString(minIf(T, W)) AS First, toString(maxIf(T, W)) AS Last,
    toString(min(T)) AS First_All, toString(max(T)) AS Last_All,
    toString(minIf(T, Dir = 'in' AND W)) AS First_In
  FROM (
    SELECT 'in' AS Dir, Transfer_Currency_Native AS Native, Transfer_Currency_SmartContract AS SC,
      Transfer_Currency_Symbol AS Symbol0, Transfer_Currency_Decimals AS Decimals0, Transfer_Amount AS Amount, Transaction_Hash AS Tx, Block_Time AS T, (%[4]s) AS W
    FROM %[5]s WHERE Transfer_Receiver = %[1]s AND Transfer_Success = 1%[7]s
    UNION ALL
    SELECT 'out', Transfer_Currency_Native, Transfer_Currency_SmartContract,
      Transfer_Currency_Symbol, Transfer_Currency_Decimals, Transfer_Amount, Transaction_Hash, Block_Time, (%[4]s)
    FROM %[6]s WHERE Transfer_Sender = %[1]s AND Transfer_Success = 1%[7]s
  )
  GROUP BY Native, SC
)
ORDER BY n_in + n_out DESC, Contract
LIMIT 200
%[2]s
FORMAT JSON`, ethRaw(addr), settings(maxRows, 1000, maxSeconds), "if(SC != '', "+ethHex("SC")+", '')", inWin, ethReceiver, ethSender,
		pinCond(w.Before))
}

// ethPriceStatement reads the report asset's USD price from the ETH price store
// at the report's pin, the way price_at reads it: the range-keyed dictionary is
// looked up by cityHash64 of the asset's key text with the pin as the range
// value. The ranges are open-ended, so the lookup resolves at the pin (no
// top-of-hour gap and no Bitcoin-style hour fallback). dictGetOrNull returns
// NULL when the asset has no price, which the report reads as "no price" (never
// as 0). The statement reads only the dictionary.
func ethPriceStatement(asset tronAsset, pin time.Time, maxRows int64, maxSeconds int) string {
	return fmt.Sprintf(`SELECT toString(dictGetOrNull('marketdata.prices_eth_dict', 'Price_Amount', cityHash64('%s'), toDateTime('%s', 'UTC'))) AS Price
%s
FORMAT JSON`, ethPriceKey(asset), pin.UTC().Format(time.DateTime), settings(maxRows, 1, maxSeconds))
}

// ethCounterpartiesStatement lists the 1-hop counterparties, per direction, in
// one read of each table, in the same shape the Tron statement produces: R = 1
// groups hold the report asset, R = 0 groups every other asset. The one delta
// from Tron is the native-dust bar for the other-asset groups: ETH's 18-decimal
// native amount makes a fixed unit count meaningless, so the bar is a USD bar in
// wei (in.NativeDustRaw, from the ETH native price), falling back to the unit
// count only when no native price was found.
func ethCounterpartiesStatement(in cpInput) string {
	asset := ethAssetCond(in.Asset)
	win := in.W.cond()
	nativeDust := fmt.Sprint(in.NativeDust)
	if in.NativeDustRaw != nil && in.NativeDustRaw.Sign() > 0 {
		nativeDust = in.NativeDustRaw.String()
	}
	imitation := "0"
	if plainSymbolRe.MatchString(in.Asset.Symbol) {
		imitation = fmt.Sprintf("(Transfer_Currency_Native = 0 AND NOT (%s) AND translateUTF8(lowerUTF8(Transfer_Currency_Symbol), '%s', '%s') = '%s')",
			asset, confusableFrom, confusableTo, strings.ToLower(in.Asset.Symbol))
	}
	side := func(dir, table, self, other, early string) string {
		return fmt.Sprintf(`  SELECT '%[1]s' AS Dir, %[4]s AS C, toUInt8(%[5]s) AS R, count() AS n, sum(Transfer_Amount) AS raw,
    countIf(Transfer_Amount > toUInt256(%[7]s)) AS n_min, sumIf(Transfer_Amount, Transfer_Amount > toUInt256(%[7]s)) AS raw_min,
    countIf(Transfer_Amount <= toUInt256(%[8]s)) AS n_dust, max(Transfer_Amount) AS raw_max, %[9]s AS raw_early,
    countIf(Transfer_Currency_Native = 1 AND Transfer_Amount <= toUInt256(%[11]s)) AS n_native_dust,
    countIf(Transfer_Currency_Native = 1 AND Transfer_Amount > toUInt256(%[11]s)) AS n_native_above,
    groupUniqArrayIf(5)(if(Transfer_Currency_SmartContract != '', %[12]s, ''), %[13]s) AS Imitations,
    toString(min(Block_Time)) AS First, toString(max(Block_Time)) AS Last,
    arrayMap(x -> x.2, groupArraySortedIf(10)((toUInt32(Block_Time), lower(hex(Transaction_Hash))), Transfer_Amount > toUInt256(%[8]s))) AS Tx_Above_Dust,
    arrayMap(x -> x.2, groupArraySorted(10)((toUInt32(Block_Time), lower(hex(Transaction_Hash))))) AS Tx
  FROM %[2]s WHERE %[3]s = %[10]s AND Transfer_Success = 1%[6]s
  GROUP BY C, R`, dir, table, self, other, asset, win, in.MinRaw.String(), in.DustRaw.String(), early, ethRaw(in.Addr),
			nativeDust, ethHex("Transfer_Currency_SmartContract"), imitation)
	}
	early := "toUInt256(0)"
	if !in.FirstIn.IsZero() {
		early = fmt.Sprintf("sumIf(Transfer_Amount, Block_Time <= toDateTime('%s', 'UTC') + INTERVAL %d DAY)",
			in.FirstIn.UTC().Format(time.DateTime), in.EarlyDays)
	}
	return fmt.Sprintf(`SELECT Dir, %[1]s AS Counterparty, R, n, toString(raw) AS Raw, n_min, toString(raw_min) AS Raw_Above_Min, n_dust,
  toString(raw_max) AS Raw_Max, toString(raw_early) AS Raw_Early, n_native_dust, n_native_above, Imitations,
  First, Last, Tx_Above_Dust, Tx,
  count() OVER (PARTITION BY Dir, R) AS Dir_Counterparties,
  sum(n) OVER (PARTITION BY Dir, R) AS Dir_Transfers,
  toString(sum(raw) OVER (PARTITION BY Dir, R)) AS Dir_Raw,
  toString(sum(raw_min) OVER (PARTITION BY Dir, R)) AS Dir_Raw_Above_Min
FROM (
%[2]s
  UNION ALL
%[3]s
)
ORDER BY Dir, R DESC, if(R = 1, raw, toUInt256(n)) DESC, C
LIMIT %[4]d BY Dir, R
%[5]s
FORMAT JSON`, ethHex("C"),
		side("in", ethReceiver, "Transfer_Receiver", "Transfer_Sender", "toUInt256(0)"),
		side("out", ethSender, "Transfer_Sender", "Transfer_Receiver", early),
		in.Limit, settings(in.MaxRows, 4*in.Limit+10, in.MaxSecs))
}

// assetsEth totals every asset of the address on Ethereum. It mirrors
// executor.assets, parsing the same row shape without a TRC-10 asset id: an
// asset is native ETH or an ERC-20 identified by its contract.
func (e *executor) assetsEth(ctx context.Context, addr string, w window) (assetsResult, error) {
	maxRows, err := e.maxRows()
	if err != nil {
		return assetsResult{}, err
	}
	rows, err := e.query(ctx, "assets", headerSender, ethAssetsStatement(addr, w, maxRows, e.secs(ctx)))
	if err != nil {
		return assetsResult{}, err
	}
	var out assetsResult
	out.truncated = len(rows) >= 200
	for _, r := range rows {
		a := tronAsset{Symbol: asStr(r["Symbol"]), Decimals: int(asInt(r["Decimals"]))}
		if asInt(r["Native"]) == 1 {
			a.Native, a.Key = true, "ETH"
			if a.Symbol == "" {
				a.Symbol = "ETH"
			}
		} else {
			// The contract may be written into a later statement: an asset whose
			// value fails the check is skipped.
			c := strings.ToLower(asStr(r["Contract"]))
			if !ethAddressRe.MatchString(c) {
				continue
			}
			a.Contract, a.Key = c, c
		}
		row := assetRow{
			asset: a, recv: asBig(r["Received"]), sent: asBig(r["Sent"]), recvAll: asBig(r["Received_All"]), sentAll: asBig(r["Sent_All"]),
			nIn: asInt(r["n_in"]), nOut: asInt(r["n_out"]), txs: asInt(r["txs"]),
			first: emptyTime(asStr(r["First"])), last: emptyTime(asStr(r["Last"])),
			firstAll: emptyTime(asStr(r["First_All"])), lastAll: emptyTime(asStr(r["Last_All"])),
		}
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

// priceEth reads the report asset's USD price from the ETH price store at the
// pin, as one billed statement counted in the report's cost (named "price"). It
// returns 0 and no error when the store has no price for the asset, so a miss is
// never read as 0 USD by the caller.
func (e *executor) priceEth(ctx context.Context, asset tronAsset, pin time.Time) (float64, error) {
	maxRows, err := e.maxRows()
	if err != nil {
		return 0, err
	}
	rows, err := e.query(ctx, "price", headerSender, ethPriceStatement(asset, pin, maxRows, e.secs(ctx)))
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

// counterpartiesEth runs the Ethereum counterparty statement and splits its
// rows exactly as executor.counterparties does for Tron: the report asset's
// counterparties (R = 1) and the other assets' contacts (R = 0). Only the
// address form differs (0x hex, validated with ethAddressRe).
func (e *executor) counterpartiesEth(ctx context.Context, in cpInput) (edgesResult, otherResult, error) {
	maxRows, err := e.maxRows()
	if err != nil {
		return edgesResult{}, otherResult{}, err
	}
	in.MaxRows, in.MaxSecs = maxRows, e.secs(ctx)
	rows, err := e.query(ctx, "counterparties", headerSender, ethCounterpartiesStatement(in))
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
		addr := strings.ToLower(asStr(r["Counterparty"]))
		if !ethAddressRe.MatchString(addr) {
			return edgesResult{}, otherResult{}, fmt.Errorf("the counterparties statement returned an address that is not an Ethereum address")
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
