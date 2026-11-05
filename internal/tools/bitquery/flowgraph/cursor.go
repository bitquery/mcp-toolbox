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

package flowgraph

// The keyset cursor for group_by=address paging. A page past the top-K does not
// re-rank a cached list; it re-runs the same statement over the same fixed
// window with a HAVING predicate that keeps only the counterparties strictly
// after the last returned one in the total order
//
//	row_null ASC, Usd_Rank DESC, (Transfers_In + Transfers_Out) DESC, Counterparty ASC
//
// (Usd_Rank = round(coalesce(Usd_Total,0),2), the quantized ranking key — a raw
// Float64 sum is not bit-reproducible between runs, so a cent-rounded key is what
// keeps a boundary counterparty from being dropped or repeated across pages, and
// is also what makes the order deterministic across the two prod nodes).
//
// next_cursor carries that position AND a hash binding the window and every
// filter, so a cursor cannot be replayed against a different query (a changed
// window, direction, token or filter) — which would silently skip or repeat
// counterparties. A mismatch is rejected and the caller restarts from page one.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// cursorVersion is bumped if the encoded shape or the total order changes, so a
// cursor minted by an older build is rejected rather than resumed incorrectly.
// v2: the address is validated against the chain's codec (base58 on Tron), not a
// hardcoded 0x-hex form.
const cursorVersion = 2

// cursorKey is the last returned counterparty's position in the total order.
// RowNull is 0 (priced) or 1 (unpriced); UsdRank is the cent-quantized ranking
// key exactly as the statement emitted it (Usd_Rank); Tsum is Transfers_In +
// Transfers_Out; Addr is the Counterparty output string (0x + 40 lower-hex).
type cursorKey struct {
	RowNull int     `json:"r"`
	UsdRank float64 `json:"u"`
	Tsum    int64   `json:"t"`
	Addr    string  `json:"a"`
}

// cursorEnvelope is what is base64url-encoded into next_cursor: the version, the
// bind hash, and the position.
type cursorEnvelope struct {
	V    int       `json:"v"`
	Hash string    `json:"h"`
	Key  cursorKey `json:"k"`
}

// encodeCursor mints next_cursor for a page's last row, bound to the query.
func encodeCursor(k cursorKey, bind string) string {
	b, _ := json.Marshal(cursorEnvelope{V: cursorVersion, Hash: bind, Key: k})
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeCursor parses and validates a caller-supplied cursor against the current
// query's bind hash. Every failure is a client error with a plain message; the
// decoded key's fields are validated (RowNull 0|1, a finite non-negative
// ranking key and transfer count, the address in the exact Counterparty output
// form) so the render builds the predicate from typed, checked literals — never
// from caller text.
func decodeCursor(s, bind string, codec chainCodec) (cursorKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return cursorKey{}, fmt.Errorf("it is malformed")
	}
	var env cursorEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return cursorKey{}, fmt.Errorf("it is malformed")
	}
	if env.V != cursorVersion {
		return cursorKey{}, fmt.Errorf("it is from an older version; restart from the first page")
	}
	if env.Hash != bind {
		return cursorKey{}, fmt.Errorf("it does not match this query (the addresses, window or a filter changed); restart from the first page")
	}
	k := env.Key
	if k.RowNull != 0 && k.RowNull != 1 {
		return cursorKey{}, fmt.Errorf("it is malformed")
	}
	if math.IsNaN(k.UsdRank) || math.IsInf(k.UsdRank, 0) || k.UsdRank < 0 {
		return cursorKey{}, fmt.Errorf("it is malformed")
	}
	if k.Tsum < 0 {
		return cursorKey{}, fmt.Errorf("it is malformed")
	}
	if !codec.outputAddr(k.Addr) {
		return cursorKey{}, fmt.Errorf("it is malformed")
	}
	return k, nil
}

// bindHash binds a cursor to the query it was issued for. Everything that
// changes which counterparties the statement returns, or the order it ranks
// them in, goes into the hash: the chain, the (sorted) address set, the fixed
// window, the direction, the token, the two include flags, the USD floor, the
// group_by, and the two label filters (applied in Go, but they change which
// rows survive a page, so a walk must not mix them). limit and format are left
// out — they change page size and rendering, not the ranking, so a caller may
// vary them between pages.
func bindHash(req request) string {
	addrs := append([]string(nil), req.addrs...)
	sort.Strings(addrs)
	lt := append([]string(nil), req.labelTypes...)
	sort.Strings(lt)
	xlt := append([]string(nil), req.excludeLabelTypes...)
	sort.Strings(xlt)
	canonical := strings.Join([]string{
		"chain=" + req.chain,
		"addrs=" + strings.Join(addrs, ","),
		"ws=" + req.wStart.UTC().Format("2006-01-02T15:04:05"),
		"we=" + req.wEnd.UTC().Format("2006-01-02T15:04:05"),
		"dir=" + req.direction,
		"tok=" + req.token,
		"ii=" + strconv.Itoa(req.includeInternal),
		"iz=" + strconv.Itoa(req.includeZero),
		"min=" + formatFloat(req.minUsd),
		"gb=" + req.groupBy,
		"lt=" + strings.Join(lt, ","),
		"xlt=" + strings.Join(xlt, ","),
	}, "\n")
	sum := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(sum[:12])
}
