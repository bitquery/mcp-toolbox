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

// Running the search's statements: the history reads and, when a read is
// refused for its size, the EXPLAINs that find the hubs in it. Each goes
// through executor.query, so each is budgeted, costed and has its own query_id.

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
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
	if errors.Is(err, errBudget) || errors.Is(err, errNoTime) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
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

// perAddress is a history statement's row budget per address: what one
// address costs to read at least, plus hub_transfers.
func (e *executor) perAddress() int64 {
	return e.th.GateRowsPerAddress + int64(e.th.HubTransfers)
}

// resultSlack is added to a history statement's result cap.
const resultSlack = 100

// readSet reads one table's history of a set of addresses. Its row budget is
// perAddress per address and its result cap hub_transfers per address, so a
// set holding a hub is refused (before reading, for the row budget); overflow
// reports that refusal, which the caller resolves with EXPLAINs.
func (e *executor) readSet(ctx context.Context, side hopSide, addrs []string, bounds []uint32, asset tronAsset) ([]histRow, bool, error) {
	left, err := e.maxRows()
	if err != nil {
		return nil, false, err
	}
	budget := e.perAddress() * int64(len(addrs))
	maxResult := e.th.HubTransfers*len(addrs) + resultSlack
	rows, err := e.history(ctx, side, addrs, bounds, asset, min(left, budget), maxResult)
	if err != nil {
		// A refusal for size within the set's own budget is the set's; within
		// the report's remaining budget it is the report's.
		if classifyLimit(ctx, err) == rowsLimit && budget <= left {
			return nil, true, nil
		}
		return nil, false, err
	}
	return rows, false, nil
}

// granuleRows is the rows of one primary-key granule.
const granuleRows = 8192

// granules reads the primary-key granules one address occupies on a table
// from EXPLAIN indexes = 1, which reads no data. ok is false when the text has
// no primary-key granule count.
func (e *executor) granules(ctx context.Context, side hopSide, addr string) (int64, bool, error) {
	if _, err := e.maxRows(); err != nil {
		return 0, false, err
	}
	stmt, err := explainStatement(side, addr, e.secs(ctx))
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
// each up to its bound.
func (e *executor) history(ctx context.Context, side hopSide, addrs []string, bounds []uint32, asset tronAsset, maxRows int64, maxResult int) ([]histRow, error) {
	stmt, err := historyStatement(side, addrs, bounds, asset, maxRows, maxResult, e.secs(ctx))
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
