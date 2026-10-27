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

// The Bitcoin statements and codec of the direct view. Bitcoin is a UTXO chain
// on an older ClickHouse: a value transfer is not a row between two addresses
// but a transaction's inputs funding its outputs, so the direct view reads an
// address's receipts and spends and the transactions behind them, then
// attributes each one pro rata (utxo_attrib.go). Every value written into a
// statement is a validated Bitcoin address, a decimal integer (a block or a
// transaction index) or a formatted time: the kind builds SQL by hand, so
// nothing the caller or the data supplies reaches a statement unchecked.
//
// The gateway does NOT cancel a query when the client disconnects, so a
// statement carries max_execution_time and timeout_overflow_mode='throw' but
// NOT cancel_http_readonly_queries_on_client_close, and no statement is started
// that cannot finish in the time left (executor.mayStart). The gateway is an
// older engine that rejects the modern JSON/date output settings, so every date
// is turned into a string in SQL (toString) and every amount is returned as a
// decimal string (parsed to exact satoshi in Go).

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
)

// bitcoinAddressRe accepts the base58 (P2PKH '1…', P2SH '3…') and bech32
// (segwit 'bc1…', testnet 'tb1…') forms. It is deliberately a little loose on
// length; what matters is that it admits no character that is not base58 or
// bech32 (so no quote can reach a statement) and that it REJECTS a Tron 'T…'
// address, which neither form begins with.
var bitcoinAddressRe = regexp.MustCompile(`^(?:[13][1-9A-HJ-NP-Za-km-z]{25,39}|(?:bc1|tb1)[02-9ac-hj-np-z]{6,87})$`)

// Bitcoin tables. inputs_by_address is the complete record of what an address
// spent; outputs_to_address is a flow copy of what an address received (it
// misses a few whole blocks a month, so the two are never cross-checked for an
// equality invariant). tx_inputs and tx_outputs are a transaction's inputs and
// outputs, keyed by (block, tx_index).
const (
	btcOutputsToAddress = "bitcoin_flow.outputs_to_address" // receipts of an address
	btcInputsByAddress  = "bitcoin.inputs_by_address"       // spends of an address (complete)
	btcTxInputs         = "bitcoin_flow.tx_inputs"          // a transaction's inputs
	btcTxOutputs        = "bitcoin_flow.tx_outputs"         // a transaction's outputs
)

// btcCurrencyID is BTC's currency id in the price store's range-keyed dictionary.
const btcCurrencyID = 375376

// chainPrefixSkip is the chain's fixed address network-prefix length, skipped
// before the poisoning rule counts matching leading characters. Every Tron
// base58 address begins with 'T' (not a resemblance); Bitcoin has several
// address forms with no single fixed prefix, so nothing is skipped there.
func chainPrefixSkip(chain string) int {
	switch chain {
	case "bitcoin":
		return 0
	case "ethereum":
		// Every Ethereum address starts with the same "0x"; it is not a resemblance.
		return ethPrefixSkip
	}
	return tronPrefixSkip
}

// btcDecimals is BTC's precision: 8 decimals, so one unit is a satoshi.
const btcDecimals = 8

// bitcoinAsset is the report asset on Bitcoin: there is only one, BTC, and it
// is native (no contract). It is carried in the same struct the Tron path uses
// purely as a data carrier for the output; its SQL methods are never called on
// the Bitcoin path (the statements here are written by hand).
func bitcoinAsset() tronAsset {
	return tronAsset{Key: "BTC", Native: true, Symbol: "BTC", Decimals: btcDecimals}
}

// btcSettings bounds a statement on the Bitcoin gateway. It carries the row,
// result and time limits with 'throw' overflow modes, but NOT
// cancel_http_readonly_queries_on_client_close (the gateway ignores it, and a
// statement that cannot finish in the time left is never started) and none of
// the modern JSON/date settings the engine rejects. wait_end_of_query is set by
// the executor as a URL parameter, which the gateway accepts.
func btcSettings(maxRows int64, maxResult int, maxSeconds int) string {
	return fmt.Sprintf("SETTINGS max_rows_to_read = %d, read_overflow_mode = 'throw', max_result_rows = %d, result_overflow_mode = 'throw', max_execution_time = %d, timeout_overflow_mode = 'throw'",
		maxRows, maxResult, maxSeconds)
}

// btcTimeLit formats a time as a ClickHouse DateTime literal in UTC.
func btcTimeLit(t time.Time) string {
	return fmt.Sprintf("toDateTime('%s', 'UTC')", t.UTC().Format(time.DateTime))
}

// btcWindow is the analysis window as two expressions: pinned (always, strictly
// before the report's pin) and the in-window test (1 when the window has no
// start). Every statement reads strictly before the pin, so all of them see the
// same chain.
func btcWindow(w window) (pin string, inWin string) {
	pin = ""
	if !w.Before.IsZero() {
		pin = " AND tx_time < " + btcTimeLit(w.Before)
	}
	inWin = "1"
	if !w.After.IsZero() {
		inWin = "tx_time >= " + btcTimeLit(w.After)
	}
	return pin, inWin
}

// btcSatoshi parses a non-negative decimal amount string (as ClickHouse prints a
// Decimal(18,8)) into exact satoshi. It never uses a float: the fractional part
// is padded to 8 digits and concatenated, so 1.5 -> 150000000 exactly. "" is 0.
func btcSatoshi(s string) (*big.Int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return new(big.Int), nil
	}
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	if intPart == "" {
		intPart = "0"
	}
	if len(fracPart) > btcDecimals {
		fracPart = fracPart[:btcDecimals] // Decimal(18,8) never has more, but never read more
	}
	for len(fracPart) < btcDecimals {
		fracPart += "0"
	}
	digits := intPart + fracPart
	b, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return nil, fmt.Errorf("the chain returned an amount that is not a decimal: %q", truncate(s, 32))
	}
	if neg {
		b.Neg(b)
	}
	return b, nil
}

// btcTxKey is one transaction, identified by (block, tx_index): the sort key of
// tx_inputs and tx_outputs, so a point lookup of it prunes.
type btcTxKey struct {
	Block   uint64
	TxIndex uint64
}

// btcExpandTxCap bounds the per-transaction list a side returns: the largest
// this many transactions by value are listed and expanded into counterparties,
// the rest become a cut. It bounds how much a report reads and bills on a busy
// address, and keeps the list read's own result well under the gateway cap.
const btcExpandTxCap = 1000

// btcPartsChunk is the transactions a single inputs/outputs read covers. The
// gateway enforces a hard result cap of about 25,000 rows; at a few tens of
// parts per transaction, a few hundred transactions stay safely under it.
const btcPartsChunk = 300

// btcPartsResultCap is the result-row limit an inputs/outputs read sets, below
// the gateway's own hard cap, so an unusually wide read overflows predictably
// (and is caught and turned into a cut) rather than erroring the report.
const btcPartsResultCap = 20000

// btcAggStatement reads an address's receipts or spends: the per-transaction
// rows ('tx') of the window, largest first, capped at limit; when withAgg is
// set it also returns one aggregate row ('agg') over the whole history up to the
// pin and over the window. (No window functions on this engine, so the two are
// folded with UNION ALL and a kind marker.) The aggregate is used only for the
// windowed summary; the whole-history summary comes from the lifetime-totals
// table instead, so withAgg is false there and the statement never sums every
// row. addr is a validated Bitcoin address; amounts and times come back as
// strings. withTx carries the transaction hash in the 'tx' rows (receipts have
// it; spends do not — the hash is read later with the outputs).
func btcAggStatement(table, addr string, w window, withTx, withAgg bool, maxRows int64, maxResult, maxSeconds, limit int) string {
	pin, inWin := btcWindow(w)
	txHash := "''"
	if withTx {
		txHash = "lower(hex(any(tx_id_bin)))"
	}
	list := fmt.Sprintf(`SELECT 'tx' AS kind, toString(block) AS blk, toString(tx_index) AS txi,
    toString(sum(value)) AS amt_w, '' AS amt_all, toString(count()) AS n_w,
    toString(min(tx_time)) AS first_w, '' AS last_w, '' AS first_all, '' AS last_all, %[5]s AS tx
  FROM %[1]s WHERE address = '%[2]s'%[4]s AND (%[3]s)
  GROUP BY block, tx_index
  ORDER BY sum(value) DESC, block, tx_index
  LIMIT %[6]d`, table, addr, inWin, pin, txHash, limit)
	body := list
	if withAgg {
		// The aggregate totals the window and the whole history up to the pin; it
		// scans every row of the address, so it is used only for a windowed report.
		body = fmt.Sprintf(`SELECT 'agg' AS kind, '' AS blk, '' AS txi,
    toString(sumIf(value, %[3]s)) AS amt_w, toString(sum(value)) AS amt_all, toString(countIf(%[3]s)) AS n_w,
    toString(minIf(tx_time, %[3]s)) AS first_w, toString(maxIf(tx_time, %[3]s)) AS last_w,
    toString(min(tx_time)) AS first_all, toString(max(tx_time)) AS last_all, '' AS tx
  FROM %[1]s WHERE address = '%[2]s'%[4]s
  UNION ALL
  SELECT * FROM
  (
  %[5]s
  )`, table, addr, inWin, pin, list)
	}
	return fmt.Sprintf(`SELECT kind, blk, txi, amt_w, amt_all, n_w, first_w, last_w, first_all, last_all, tx FROM
(
  %[1]s
)
%[2]s
FORMAT JSON`, body, btcSettings(maxRows, maxResult, maxSeconds))
}

// btcAddressTransfersStatement reads an address's lifetime totals from the
// aggregating-merge table, keyed by address, so it is cheap for an address of
// any size — unlike summing every receipt or spend row. The amounts and times
// are aggregate states merged per address. It has no time bound, so it serves
// the whole-history (default) report only.
func btcAddressTransfersStatement(addr string, maxRows int64, maxSeconds int) string {
	return fmt.Sprintf(`SELECT toString(sumMerge(transfers_to_amount)) AS received,
  toString(sumMerge(transfers_from_amount)) AS sent,
  toString(minMerge(transfers_to_first_at)) AS first_in, toString(maxMerge(transfers_to_last_at)) AS last_in,
  toString(minMerge(transfers_from_first_at)) AS first_out, toString(maxMerge(transfers_from_last_at)) AS last_out
FROM bitcoin_flow.address_transfers WHERE address = '%[1]s' GROUP BY address
%[2]s
FORMAT JSON`, addr, btcSettings(maxRows, 10, maxSeconds))
}

// btcDistinctKeys removes duplicate transaction keys, keeping order.
func btcDistinctKeys(keys []btcTxKey) []btcTxKey {
	seen := map[btcTxKey]bool{}
	out := make([]btcTxKey, 0, len(keys))
	for _, k := range keys {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// btcTxWhere is the key predicate selecting a set of transactions on a table
// sorted by (block, tx_index): block IN (…) prunes on the leading key column and
// the exact (block, tx_index) pairs narrow to the transactions. A ONE-element
// tuple IN is ambiguous on the older engine — (block, tx_index) IN ((N, M)) is
// read as the bare tuple (N, M), not a set of one — so a single transaction is
// written as an equality instead (a scalar one-element IN, as block IN (N), is
// unambiguous and kept).
func btcTxWhere(keys []btcTxKey) string {
	uniq := btcDistinctKeys(keys)
	if len(uniq) == 1 {
		return fmt.Sprintf("block = %d AND tx_index = %d", uniq[0].Block, uniq[0].TxIndex)
	}
	blocks := map[uint64]bool{}
	var bl, tl strings.Builder
	for i, k := range uniq {
		if i > 0 {
			tl.WriteString(", ")
		}
		fmt.Fprintf(&tl, "(%d, %d)", k.Block, k.TxIndex)
		if !blocks[k.Block] {
			blocks[k.Block] = true
			if bl.Len() > 0 {
				bl.WriteString(", ")
			}
			fmt.Fprintf(&bl, "%d", k.Block)
		}
	}
	return fmt.Sprintf("block IN (%s) AND (block, tx_index) IN (%s)", bl.String(), tl.String())
}

// btcTxPartsStatement reads the inputs (table btcTxInputs) or the outputs
// (table btcTxOutputs) of a bounded set of transactions. Each transaction is a
// literal (block, tx_index) on the sort key, so the read prunes. It returns, per
// part, the address, the amount and the transaction's total input value
// (sum_inputs, constant per transaction); the outputs also carry the
// transaction hash. withTx adds the hash (outputs only).
func btcTxPartsStatement(table string, keys []btcTxKey, withTx bool, maxRows int64, maxResult, maxSeconds int) string {
	txHash := ""
	if withTx {
		txHash = ", lower(hex(tx_id_bin)) AS tx"
	}
	return fmt.Sprintf(`SELECT toString(block) AS blk, toString(tx_index) AS txi, address AS addr, toString(value) AS val, toString(sum_inputs) AS si%[4]s
FROM %[1]s
WHERE %[2]s
ORDER BY block, tx_index
%[3]s
FORMAT JSON`, table, btcTxWhere(keys), btcSettings(maxRows, maxResult, maxSeconds), txHash)
}

// btcPriceStatement reads BTC's USD price from the chain's price dictionary at
// or before the report's pin, the way price_at reads it: the key is a composite
// (currency id in the high 32 bits, the hour start in the low 32). The
// dictionary is keyed by the exact hour slot and has no range semantics, so the
// slot for the pin's own hour can be briefly absent at the top of an hour while
// the feed catches up. To keep a live report from losing every USD threshold in
// that window, the read walks back from the pin's hour up to 24 hours and
// returns the newest hour that has a price, with that hour as asof (the caller
// prints it). No priced hour in the day returns no row (read as no price, never
// 0). The engine has no dictGetOrNull, so dictHas gates the read.
func btcPriceStatement(pin time.Time, maxRows int64, maxSeconds int) string {
	key := fmt.Sprintf("toUInt64(toUInt32(h)) + bitShiftLeft(toUInt64(%d), 32)", btcCurrencyID)
	return fmt.Sprintf(`SELECT formatDateTime(h, '%%FT%%TZ') AS asof, toString(dictGetFloat32('market_price', 'usd', %[1]s)) AS price
FROM (SELECT toStartOfHour(%[2]s) - (arrayJoin(range(25)) * 3600) AS h)
WHERE dictHas('market_price', %[1]s)
ORDER BY h DESC
LIMIT 1
%[3]s
FORMAT JSON`, key, btcTimeLit(pin), btcSettings(maxRows, 1, maxSeconds))
}
