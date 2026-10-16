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

// The Tron statements of the direct view. Every value written into a statement
// is a validated base58 address, a decimal integer or a formatted time: the
// kind builds SQL by hand, so nothing the caller or the data supplies reaches a
// statement unchecked. Every statement is bounded by SETTINGS (rows read,
// result rows, time) and runs alone.

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
)

var (
	tronAddressRe = regexp.MustCompile(`^T[1-9A-HJ-NP-Za-km-z]{33}$`)
	trc10IDRe     = regexp.MustCompile(`^[0-9]{1,12}$`)
	plainSymbolRe = regexp.MustCompile(`^[A-Za-z0-9]{1,16}$`)
)

const (
	tronSender   = "tron_api.transfers_sender"
	tronReceiver = "tron_api.transfers_receiver"
)

// tronRaw is the FixedString(20) form of a validated base58 address.
func tronRaw(addr string) string {
	return fmt.Sprintf("substring(base58Decode('%s'), 2, 20)", addr)
}

// tronB58 encodes a FixedString(20) column as a base58 address.
func tronB58(col string) string {
	return fmt.Sprintf("base58Encode(concat(concat(unhex('41'), %[1]s), substring(SHA256(SHA256(concat(unhex('41'), %[1]s))), 1, 4)))", col)
}

// tronAsset is the report asset on Tron.
type tronAsset struct {
	Key      string // "TRX", a contract, or "trc10:<id>"
	Native   bool
	Contract string
	TRC10    string
	Symbol   string
	Decimals int
}

// cond is the asset filter of a transfer row.
func (a tronAsset) cond() string {
	switch {
	case a.Native:
		return "Transfer_Currency_Native = 1"
	case a.Contract != "":
		return "Transfer_Currency_SmartContract = " + tronRaw(a.Contract)
	default:
		return fmt.Sprintf("(Transfer_Currency_Native = 0 AND length(Transfer_Currency_SmartContract) = 0 AND Transfer_Currency_AssetId = '%s')", a.TRC10)
	}
}

// parseTronAsset reads the asset parameter: "" (choose), TRX, a contract, or a
// TRC-10 asset id.
func parseTronAsset(s string) (tronAsset, bool, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return tronAsset{}, false, nil
	case strings.EqualFold(s, "TRX"):
		return tronAsset{Key: "TRX", Native: true, Symbol: "TRX", Decimals: 6}, true, nil
	case tronAddressRe.MatchString(s):
		return tronAsset{Key: s, Contract: s}, true, nil
	case trc10IDRe.MatchString(s):
		return tronAsset{Key: "trc10:" + s, TRC10: s}, true, nil
	}
	return tronAsset{}, false, fmt.Errorf("asset must be TRX, a token contract address (T...) or a TRC-10 asset id")
}

// window is the analysis window; zero times are open ends.
type window struct {
	After, Before time.Time
}

func (w window) cond() string {
	var b strings.Builder
	if !w.After.IsZero() {
		fmt.Fprintf(&b, " AND Block_Time >= toDateTime('%s', 'UTC')", w.After.UTC().Format(time.DateTime))
	}
	if !w.Before.IsZero() {
		fmt.Fprintf(&b, " AND Block_Time < toDateTime('%s', 'UTC')", w.Before.UTC().Format(time.DateTime))
	}
	return b.String()
}

func (w window) open() bool { return w.After.IsZero() && w.Before.IsZero() }

func settings(maxRows int64, maxResult int, maxSeconds int) string {
	return fmt.Sprintf("SETTINGS max_rows_to_read = %d, read_overflow_mode = 'throw', max_result_rows = %d, result_overflow_mode = 'throw', max_execution_time = %d",
		maxRows, maxResult, maxSeconds)
}

// assetsStatement totals every asset of the address: in the window and over the
// whole history.
func assetsStatement(addr string, w window, maxRows int64, maxSeconds int) string {
	inWin := "1"
	if c := w.cond(); c != "" {
		inWin = strings.TrimPrefix(c, " AND ")
	}
	return fmt.Sprintf(`SELECT Native, Contract, AssetId, Symbol, Decimals,
  toString(recv_w) AS Received, toString(sent_w) AS Sent, toString(recv_all) AS Received_All, toString(sent_all) AS Sent_All,
  n_in, n_out, txs, First, Last, First_All, Last_All
FROM (
  SELECT Native, %[3]s AS Contract, AssetId,
    any(Symbol0) AS Symbol, any(Decimals0) AS Decimals,
    sumIf(Amount, Dir = 'in' AND W) AS recv_w, sumIf(Amount, Dir = 'out' AND W) AS sent_w,
    sumIf(Amount, Dir = 'in') AS recv_all, sumIf(Amount, Dir = 'out') AS sent_all,
    countIf(Dir = 'in' AND W) AS n_in, countIf(Dir = 'out' AND W) AS n_out,
    uniqExactIf(Tx, W) AS txs,
    toString(minIf(T, W)) AS First, toString(maxIf(T, W)) AS Last,
    toString(min(T)) AS First_All, toString(max(T)) AS Last_All
  FROM (
    SELECT 'in' AS Dir, Transfer_Currency_Native AS Native, Transfer_Currency_SmartContract AS SC, Transfer_Currency_AssetId AS AssetId,
      Transfer_Currency_Symbol AS Symbol0, Transfer_Currency_Decimals AS Decimals0, Transfer_Amount AS Amount, Transaction_Hash AS Tx, Block_Time AS T, (%[4]s) AS W
    FROM %[5]s WHERE Transfer_Receiver = %[1]s AND Transfer_Success = 1
    UNION ALL
    SELECT 'out', Transfer_Currency_Native, Transfer_Currency_SmartContract, Transfer_Currency_AssetId,
      Transfer_Currency_Symbol, Transfer_Currency_Decimals, Transfer_Amount, Transaction_Hash, Block_Time, (%[4]s)
    FROM %[6]s WHERE Transfer_Sender = %[1]s AND Transfer_Success = 1
  )
  GROUP BY Native, SC, AssetId
)
ORDER BY n_in + n_out DESC, Contract, AssetId
LIMIT 200
%[2]s
FORMAT JSON`, tronRaw(addr), settings(maxRows, 1000, maxSeconds), "if(length(SC) = 20, "+tronB58("SC")+", '')", inWin, tronReceiver, tronSender)
}

// cpInput holds the numbers written into the counterparty statement.
type cpInput struct {
	Addr       string
	Asset      tronAsset
	W          window
	MinRaw     *big.Int
	DustRaw    *big.Int
	NativeDust int64
	EarlyDays  int
	Limit      int
	MaxRows    int64
	MaxSecs    int
}

// confusables maps Cyrillic and Greek letters that look like Latin ones.
const (
	confusableFrom = "аеорсухкмтвнѕіјԁԛԝαοτυνκρειϲ"
	confusableTo   = "aeopcyxkmtbhsijdqwaotuvkpeic"
)

// counterpartiesStatement lists the 1-hop counterparties, per direction, in one
// read of each table: R = 1 groups hold the report asset (amounts against the
// thresholds, sends early after the first receipt), R = 0 groups every other
// asset (native dust, native transfers above it, tokens that carry the report
// asset's symbol, or a look-alike of it, on another contract). Totals per
// (direction, R) cover ALL counterparties: window functions run before LIMIT BY.
func counterpartiesStatement(in cpInput) string {
	asset := in.Asset.cond()
	win := in.W.cond()
	imitation := "0"
	if plainSymbolRe.MatchString(in.Asset.Symbol) {
		imitation = fmt.Sprintf("(Transfer_Currency_Native = 0 AND NOT (%s) AND translateUTF8(lowerUTF8(Transfer_Currency_Symbol), '%s', '%s') = '%s')",
			asset, confusableFrom, confusableTo, strings.ToLower(in.Asset.Symbol))
	}
	side := func(dir, table, self, other, early string) string {
		return fmt.Sprintf(`  SELECT '%[1]s' AS Dir, %[4]s AS C, toUInt8(%[5]s) AS R, count() AS n, sum(Transfer_Amount) AS raw,
    countIf(Transfer_Amount > toUInt256(%[7]s)) AS n_min, sumIf(Transfer_Amount, Transfer_Amount > toUInt256(%[7]s)) AS raw_min,
    countIf(Transfer_Amount <= toUInt256(%[8]s)) AS n_dust, max(Transfer_Amount) AS raw_max, %[9]s AS raw_early,
    countIf(Transfer_Currency_Native = 1 AND Transfer_Amount <= toUInt256(%[11]d)) AS n_native_dust,
    countIf(Transfer_Currency_Native = 1 AND Transfer_Amount > toUInt256(%[11]d)) AS n_native_above,
    groupUniqArrayIf(5)(if(length(Transfer_Currency_SmartContract) = 20, %[12]s, Transfer_Currency_AssetId), %[13]s) AS Imitations,
    toString(min(Block_Time)) AS First, toString(max(Block_Time)) AS Last,
    arrayMap(x -> x.2, groupArraySortedIf(10)((toUInt32(Block_Time), lower(hex(Transaction_Hash))), Transfer_Amount > toUInt256(%[8]s))) AS Tx_Above_Dust,
    arrayMap(x -> x.2, groupArraySorted(10)((toUInt32(Block_Time), lower(hex(Transaction_Hash))))) AS Tx
  FROM %[2]s WHERE %[3]s = %[10]s AND Transfer_Success = 1%[6]s
  GROUP BY C, R`, dir, table, self, other, asset, win, in.MinRaw.String(), in.DustRaw.String(), early, tronRaw(in.Addr),
			in.NativeDust, tronB58("Transfer_Currency_SmartContract"), imitation)
	}
	early := fmt.Sprintf("sumIf(Transfer_Amount, Block_Time <= first_in + INTERVAL %d DAY)", in.EarlyDays)
	return fmt.Sprintf(`WITH (SELECT min(Block_Time) FROM %[1]s WHERE Transfer_Receiver = %[2]s AND %[3]s AND Transfer_Success = 1%[4]s) AS first_in
SELECT Dir, %[5]s AS Counterparty, R, n, toString(raw) AS Raw, n_min, toString(raw_min) AS Raw_Above_Min, n_dust,
  toString(raw_max) AS Raw_Max, toString(raw_early) AS Raw_Early, n_native_dust, n_native_above, Imitations,
  First, Last, Tx_Above_Dust, Tx, toString(first_in) AS First_Receipt,
  count() OVER (PARTITION BY Dir, R) AS Dir_Counterparties,
  sum(n) OVER (PARTITION BY Dir, R) AS Dir_Transfers,
  toString(sum(raw) OVER (PARTITION BY Dir, R)) AS Dir_Raw,
  toString(sum(raw_min) OVER (PARTITION BY Dir, R)) AS Dir_Raw_Above_Min
FROM (
%[6]s
  UNION ALL
%[7]s
)
ORDER BY Dir, R DESC, if(R = 1, raw, toUInt256(n)) DESC, C
LIMIT %[8]d BY Dir, R
%[9]s
FORMAT JSON`, tronReceiver, tronRaw(in.Addr), asset, win, tronB58("C"),
		side("in", tronReceiver, "Transfer_Receiver", "Transfer_Sender", "toUInt256(0)"),
		side("out", tronSender, "Transfer_Sender", "Transfer_Receiver", early),
		in.Limit, settings(in.MaxRows, 4*in.Limit+10, in.MaxSecs))
}

// labelsStatement reads the label rows of a batch of validated addresses.
func labelsStatement(chain string, addrs []string, maxRows int64, maxSeconds int) string {
	quoted := make([]string, len(addrs))
	for i, a := range addrs {
		quoted[i] = "'" + a + "'"
	}
	return fmt.Sprintf(`SELECT address, label_type, label_value, source, toString(max(recorded_at)) AS recorded_at
FROM directory.labels
WHERE chain = '%s' AND address IN (%s)
GROUP BY address, label_type, label_value, source
ORDER BY address, label_type, label_value, source
SETTINGS max_rows_to_read = %d, read_overflow_mode = 'throw', max_execution_time = %d`,
		chain, strings.Join(quoted, ", "), maxRows, maxSeconds)
}

// The statements of the multi-hop search. Every address set is a literal
// array of validated addresses in the shape the tron_flows_between tool uses:
// checksum-checked, cast to the column's FixedString(20) (a String set silently
// loses every value ending in 00) and read through IN (SELECT arrayJoin(set)).
// A subquery that reads a transfer table into a set is a full scan there, so
// none is ever written.

// quoteAddrs validates and quotes a list of base58 addresses.
func quoteAddrs(addrs []string) (string, error) {
	if len(addrs) == 0 {
		return "", fmt.Errorf("an empty address set")
	}
	q := make([]string, len(addrs))
	for i, a := range addrs {
		if !tronAddressRe.MatchString(a) {
			return "", fmt.Errorf("an address in a statement is not a Tron address")
		}
		q[i] = "'" + a + "'"
	}
	return "[" + strings.Join(q, ", ") + "]", nil
}

// tronSet is the literal set expression (a scalar of Array(FixedString(20))).
func tronSet(list string) string {
	return "(SELECT arrayDistinct(arrayMap(x -> toFixedString(x, 20), arrayFilter(x -> length(x) = 20, arrayMap(x -> if(length(tryBase58Decode(x)) = 25 AND substring(tryBase58Decode(x), 22, 4) = substring(SHA256(SHA256(substring(tryBase58Decode(x), 1, 21))), 1, 4), substring(tryBase58Decode(x), 2, 20), ''), " + list + ")))))"
}

// hopSide is one transfer table, seen from the addresses of the set.
type hopSide struct {
	Name  string // statement name in the cost detail
	Table string
	Self  string // the set's column
	Peer  string
	In    bool // the set's addresses receive
}

var (
	receiptsSide = hopSide{Name: "receipts", Table: tronReceiver, Self: "Transfer_Receiver", Peer: "Transfer_Sender", In: true}
	sendsSide    = hopSide{Name: "sends", Table: tronSender, Self: "Transfer_Sender", Peer: "Transfer_Receiver"}
)

// degreeStatement counts every transfer (any asset, any status) of each
// address on one table, reading the key column only. A hub over the row
// budget makes it throw before or while reading, which the caller resolves by
// splitting the set.
func degreeStatement(side hopSide, addrs []string, maxRows int64, maxSeconds int) (string, error) {
	list, err := quoteAddrs(addrs)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`WITH %[1]s AS set_f
SELECT %[2]s AS Address, Degree
FROM (
  SELECT %[3]s AS A, count() AS Degree
  FROM %[4]s
  PREWHERE %[3]s IN (SELECT arrayJoin(set_f))
  GROUP BY A
)
%[5]s
FORMAT JSON`, tronSet(list), tronB58("A"), side.Self, side.Table, settings(maxRows, len(addrs)+10, maxSeconds)), nil
}

// explainStatement asks for the primary-key granules one address occupies on
// a table. The address is a single literal: no set, no subquery, so the
// EXPLAIN reads nothing. It carries no row limit: the index analysis it runs
// checks max_rows_to_read against the granules it selects and would throw for
// exactly the hubs it is meant to measure.
func explainStatement(side hopSide, addr string, maxSeconds int) (string, error) {
	if !tronAddressRe.MatchString(addr) {
		return "", fmt.Errorf("an address in a statement is not a Tron address")
	}
	return fmt.Sprintf(`EXPLAIN indexes = 1
SELECT %[1]s FROM %[2]s WHERE %[1]s = %[3]s
SETTINGS max_execution_time = %[4]d
FORMAT JSON`, side.Self, side.Table, tronRaw(addr), maxSeconds), nil
}

// unbounded is the bound of an address whose whole history is read.
const unbounded = uint32(4294967295)

// historyStatement reads the report asset's transfers of a set of addresses on
// one table, each address up to its own bound (Block_Time <= bound, in unix
// seconds), in chain order.
func historyStatement(side hopSide, addrs []string, bounds []uint32, asset tronAsset, maxRows int64, maxResult int, maxSeconds int) (string, error) {
	list, err := quoteAddrs(addrs)
	if err != nil {
		return "", err
	}
	if len(bounds) != len(addrs) {
		return "", fmt.Errorf("a bound per address is required")
	}
	b := make([]string, len(bounds))
	top := uint32(0)
	for i, x := range bounds {
		b[i] = fmt.Sprintf("%d", x)
		top = max(top, x)
	}
	prune := ""
	if top != unbounded {
		prune = fmt.Sprintf(" AND Block_Time <= toDateTime(%d, 'UTC')", top)
	}
	return fmt.Sprintf(`WITH %[1]s AS set_f,
  arrayMap(x -> toFixedString(substring(base58Decode(x), 2, 20), 20), %[2]s) AS keys_f,
  CAST([%[3]s], 'Array(UInt32)') AS bounds_f
SELECT %[4]s AS Address, %[5]s AS Peer, toString(Transfer_Amount) AS Raw,
  Block_Number, Transaction_Index, Call_Index, Log_Index, Transfer_Index,
  toString(Block_Time) AS Time, lower(hex(Transaction_Hash)) AS Tx
FROM %[6]s
PREWHERE %[7]s IN (SELECT arrayJoin(set_f))
WHERE %[8]s AND Transfer_Success = 1%[9]s AND toUInt32(Block_Time) <= bounds_f[indexOf(keys_f, %[7]s)]
ORDER BY %[7]s, Block_Number, Transaction_Index, Call_Index, Log_Index, Transfer_Index, Transaction_Hash
%[10]s
FORMAT JSON`, tronSet(list), list, strings.Join(b, ", "), tronB58(side.Self), tronB58(side.Peer), side.Table, side.Self,
		asset.cond(), prune, settings(maxRows, maxResult, maxSeconds)), nil
}
