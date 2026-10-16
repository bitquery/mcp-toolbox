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

// Running the search's statements: the degree gate and the history reads, each
// through executor.query, so each is budgeted, costed and has its own query_id.

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/googleapis/mcp-toolbox/internal/util"
)

// historyBatch caps the addresses of one statement: a statement is parsed
// whole, and the server's query size limit (256 KiB by default) must hold the
// set twice (the set and the bound keys).
const historyBatch = 1000

// limitKind says how a statement failed.
type limitKind int

const (
	notLimit  limitKind = iota
	rowsLimit           // a row or byte limit: the set holds more than the budget
	stopLimit           // the deadline, a time or memory limit, or the report's row budget
)

// classifyLimit tells a statement stopped by a limit from any other failure.
func classifyLimit(ctx context.Context, err error) limitKind {
	if err == nil {
		return notLimit
	}
	if errors.Is(err, errBudget) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return stopLimit
	}
	var db *util.BitqueryDatabaseError
	if errors.As(err, &db) {
		switch db.Code() {
		case 158, 307, 396: // TOO_MANY_ROWS, TOO_MANY_BYTES, TOO_MANY_ROWS_OR_BYTES
			return rowsLimit
		case 159, 160, 241: // TIMEOUT_EXCEEDED, TOO_SLOW, MEMORY_LIMIT_EXCEEDED
			return stopLimit
		}
		switch db.Name() {
		case "TOO_MANY_ROWS", "TOO_MANY_BYTES", "TOO_MANY_ROWS_OR_BYTES":
			return rowsLimit
		case "TIMEOUT_EXCEEDED", "TOO_SLOW", "MEMORY_LIMIT_EXCEEDED":
			return stopLimit
		}
	}
	return notLimit
}

// degreeResult is the gate's answer for a set of addresses.
type degreeResult struct {
	in, out map[string]int64 // transfers per table
	over    map[string]bool  // a hub found without counting it
	// over without an estimate: its EXPLAIN gave no granule count, or the
	// rest of its set overflowed again.
	unknown map[string]bool
}

// degrees counts the transfers of each address on both tables. The row budget
// of a statement is (rows an address costs at least + hub_transfers) per
// address. A set that overflows it is resolved without reading: an EXPLAIN of
// each address alone (a literal, no set) gives the primary-key granules it
// occupies, about transfers / 8,192; the largest are hubs until the rest fits
// the budget, and the rest is counted.
func (e *executor) degrees(ctx context.Context, addrs []string) (degreeResult, error) {
	res := degreeResult{in: map[string]int64{}, out: map[string]int64{}, over: map[string]bool{}, unknown: map[string]bool{}}
	for _, side := range []hopSide{receiptsSide, sendsSide} {
		into := res.in
		if !side.In {
			into = res.out
		}
		for i := 0; i < len(addrs); i += historyBatch {
			if err := e.degreeSet(ctx, side, addrs[i:min(i+historyBatch, len(addrs))], into, res); err != nil {
				return res, err
			}
		}
	}
	return res, nil
}

func (e *executor) degreeSet(ctx context.Context, side hopSide, addrs []string, into map[string]int64, res degreeResult) error {
	var todo []string
	for _, a := range addrs {
		if !res.over[a] {
			todo = append(todo, a)
		}
	}
	if len(todo) == 0 {
		return nil
	}
	rows, overflow, err := e.countSet(ctx, side, todo)
	if err != nil {
		return err
	}
	if overflow {
		if todo, err = e.resolveOverflow(ctx, side, todo, res); err != nil || len(todo) == 0 {
			return err
		}
		if rows, overflow, err = e.countSet(ctx, side, todo); err != nil {
			return err
		}
		if overflow {
			// The estimates did not hold: nothing left is counted.
			for _, a := range todo {
				res.over[a] = true
				res.unknown[a] = true
			}
			return nil
		}
	}
	asked := map[string]bool{}
	for _, a := range todo {
		asked[a] = true
	}
	for _, r := range rows {
		a := asStr(r["Address"])
		if !asked[a] {
			return fmt.Errorf("the degree statement returned an address it was not asked for")
		}
		into[a] += asInt(r["Degree"])
	}
	return nil
}

// countSet runs the key-only count of a set. overflow is true when the set
// holds more than its own row budget (not the report's).
func (e *executor) countSet(ctx context.Context, side hopSide, todo []string) ([]map[string]any, bool, error) {
	left, err := e.maxRows()
	if err != nil {
		return nil, false, err
	}
	budget := e.perAddress() * int64(len(todo))
	stmt, err := degreeStatement(side, todo, min(left, budget), e.th.StatementMaxSeconds)
	if err != nil {
		return nil, false, err
	}
	rows, err := e.query(ctx, "degree_"+side.Name, side.Table, stmt)
	if err != nil {
		if classifyLimit(ctx, err) != rowsLimit || budget > left {
			return nil, false, err
		}
		e.cost.Detail = append(e.cost.Detail, statementCost{Name: "degree_" + side.Name + " (over its row budget)"})
		e.cost.Statements++
		return nil, true, nil
	}
	return rows, false, nil
}

// perAddress is a gate or history statement's row budget per address.
func (e *executor) perAddress() int64 {
	return e.th.GateRowsPerAddress + int64(e.th.HubTransfers)
}

// granuleRows is the rows of one primary-key granule.
const granuleRows = 8192

// resolveOverflow finds the hubs of a set that overflowed its count: it
// estimates each address's transfers on the table from an EXPLAIN of that
// address alone, marks the largest as hubs until the rest fits the set's
// budget, and returns the rest. An address whose EXPLAIN gives no granule count
// is a hub without an estimate.
func (e *executor) resolveOverflow(ctx context.Context, side hopSide, todo []string, res degreeResult) ([]string, error) {
	type est struct {
		addr string
		rows int64
	}
	var ests []est
	for _, a := range todo {
		g, ok, err := e.granules(ctx, side, a)
		if err != nil {
			return nil, err
		}
		if !ok {
			res.over[a] = true
			res.unknown[a] = true
			continue
		}
		ests = append(ests, est{a, g * granuleRows})
	}
	sort.Slice(ests, func(i, j int) bool {
		if ests[i].rows != ests[j].rows {
			return ests[i].rows > ests[j].rows
		}
		return ests[i].addr < ests[j].addr
	})
	sum := int64(0)
	for _, x := range ests {
		sum += x.rows
	}
	// While the rest does not fit, its largest address is over the budget of
	// one address, so over hub_transfers: a hub.
	for len(ests) > 0 && sum > e.perAddress()*int64(len(ests)) && ests[0].rows > int64(e.th.HubTransfers) {
		res.over[ests[0].addr] = true
		sum -= ests[0].rows
		ests = ests[1:]
	}
	rest := make([]string, len(ests))
	for i, x := range ests {
		rest[i] = x.addr
	}
	sort.Strings(rest)
	return rest, nil
}

// granules reads the primary-key granules one address occupies on a table
// from EXPLAIN indexes = 1, which reads no data. ok is false when the text has
// no primary-key granule count.
func (e *executor) granules(ctx context.Context, side hopSide, addr string) (int64, bool, error) {
	if _, err := e.maxRows(); err != nil {
		return 0, false, err
	}
	stmt, err := explainStatement(side, addr, e.th.StatementMaxSeconds)
	if err != nil {
		return 0, false, err
	}
	rows, err := e.query(ctx, "explain_"+side.Name, side.Table, stmt)
	if err != nil {
		return 0, false, err
	}
	var lines []string
	for _, r := range rows {
		lines = append(lines, asStr(r["explain"]))
	}
	g, ok := primaryKeyGranules(lines)
	return g, ok, nil
}

var granulesRe = regexp.MustCompile(`^Granules:\s*([0-9]+)\s*/\s*[0-9]+`)

// primaryKeyGranules sums the selected granules of every PrimaryKey index
// section of an EXPLAIN indexes = 1 text ("Granules: selected/total").
func primaryKeyGranules(lines []string) (int64, bool) {
	var total int64
	found, inPK := false, false
	for _, l := range lines {
		t := strings.TrimSpace(l)
		switch {
		case t == "PrimaryKey":
			inPK = true
		case inPK && strings.HasPrefix(t, "Granules:"):
			m := granulesRe.FindStringSubmatch(t)
			if m == nil {
				return 0, false
			}
			n, err := strconv.ParseInt(m[1], 10, 64)
			if err != nil {
				return 0, false
			}
			total += n
			found, inPK = true, false
		case inPK && (t == "MinMax" || t == "Partition" || t == "Skip" || strings.HasPrefix(t, "Name:")):
			inPK = false
		}
	}
	return total, found
}

// histRow is one history row with the address it belongs to.
type histRow struct {
	addr string
	ev   histEvent
}

// history reads the report asset's transfers of the addresses on one table,
// each up to its bound. maxResult is the statement's result cap.
func (e *executor) history(ctx context.Context, side hopSide, addrs []string, bounds []uint32, asset tronAsset, maxResult int) ([]histRow, error) {
	left, err := e.maxRows()
	if err != nil {
		return nil, err
	}
	per := e.th.GateRowsPerAddress + int64(e.th.HubTransfers)
	stmt, err := historyStatement(side, addrs, bounds, asset, min(left, per*int64(len(addrs))), maxResult, e.th.StatementMaxSeconds)
	if err != nil {
		return nil, err
	}
	rows, err := e.query(ctx, "history_"+side.Name, side.Table, stmt)
	if err != nil {
		return nil, err
	}
	asked := map[string]uint32{}
	for i, a := range addrs {
		asked[a] = bounds[i]
	}
	out := make([]histRow, 0, len(rows))
	for _, r := range rows {
		a := asStr(r["Address"])
		bound, ok := asked[a]
		if !ok {
			return nil, fmt.Errorf("history integrity: the history statement returned an address it was not asked for")
		}
		peer := asStr(r["Peer"])
		if !tronAddressRe.MatchString(peer) {
			return nil, fmt.Errorf("history integrity: the history statement returned a counterparty that is not a Tron address")
		}
		t, err := time.Parse(time.DateTime, asStr(r["Time"]))
		if err != nil {
			return nil, fmt.Errorf("history integrity: a transfer without a time")
		}
		if bound != unbounded && t.Unix() > int64(bound) {
			// The statement bounds every address; a row past it is not used.
			continue
		}
		raw, ok := new(big.Int).SetString(strings.TrimSpace(asStr(r["Raw"])), 10)
		if !ok || raw.Sign() < 0 {
			return nil, fmt.Errorf("history integrity: a transfer amount that is not an integer")
		}
		out = append(out, histRow{addr: a, ev: histEvent{
			In: side.In, Peer: peer, Raw: raw, Time: t.UTC(), Tx: asStr(r["Tx"]),
			Block: asUint(r["Block_Number"]), TxIndex: asUint(r["Transaction_Index"]), Call: asUint(r["Call_Index"]),
			Log: asUint(r["Log_Index"]), Transfer: asUint(r["Transfer_Index"]),
		}})
	}
	return out, nil
}

// asUint reads an unsigned integer that ClickHouse may print quoted (64-bit).
func asUint(v any) uint64 {
	switch n := v.(type) {
	case string:
		u, _ := strconv.ParseUint(strings.TrimSpace(n), 10, 64)
		return u
	case float64:
		if n < 0 {
			return 0
		}
		return uint64(n)
	}
	if i := asInt(v); i > 0 {
		return uint64(i)
	}
	return 0
}
