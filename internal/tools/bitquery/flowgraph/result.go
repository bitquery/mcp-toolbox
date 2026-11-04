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

// The result of a group_by=address call and its renderings. The default is a
// text document (a summary plus a counterparty CSV of at most 100 rows), small
// enough for a model-facing client's response cap; format=json returns the
// struct so an app gets every returned row. USD is null (never 0) for a side
// with no priced transfer. Usd_Total (in + out) is the ranking key, not a net
// flow: a pass-through counterparty counts both legs.

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/googleapis/mcp-toolbox/internal/tools/bitquery/chainexec"
)

type windowOut struct {
	AfterTime  string `json:"after_time"`
	BeforeTime string `json:"before_time"`
}

type cpRow struct {
	Counterparty         string   `json:"counterparty"`
	UsdIn                *float64 `json:"usd_in"`
	UsdOut               *float64 `json:"usd_out"`
	UsdTotal             float64  `json:"usd_total"`
	TransfersIn          int64    `json:"transfers_in"`
	TransfersOut         int64    `json:"transfers_out"`
	UnpricedTransfersIn  int64    `json:"unpriced_transfers_in"`
	UnpricedTransfersOut int64    `json:"unpriced_transfers_out"`
	FirstSeen            string   `json:"first_seen"`
	LastSeen             string   `json:"last_seen"`
}

// remainder is every counterparty not in the returned top-K: its USD and its
// transfers, derived as the WITH TOTALS grand total minus the returned rows. It
// carries the all-unpriced counterparties too (none of them reach the top-K
// while priced ones remain), so its transfers include unpriced ones; that is
// why the share base is transfers, not only USD.
type remainder struct {
	Usd       float64 `json:"usd"`
	Transfers int64   `json:"transfers"`
}

type totalsOut struct {
	UsdIn        *float64 `json:"usd_in"`
	UsdOut       *float64 `json:"usd_out"`
	UsdTotal     float64  `json:"usd_total"`
	TransfersIn  int64    `json:"transfers_in"`
	TransfersOut int64    `json:"transfers_out"`
	NoPrice      int64    `json:"no_price_transfers"`
	Suspect      int64    `json:"suspect_transfers"`
	Stale        int64    `json:"stale_transfers"`
}

type result struct {
	Chain              string            `json:"chain"`
	GroupBy            string            `json:"group_by"`
	Window             windowOut         `json:"window"`
	AddressesRequested int               `json:"addresses_requested"`
	AddressesUsed      int               `json:"addresses_used"`
	AddressesInvalid   []string          `json:"addresses_invalid,omitempty"`
	IncludeInternal    int               `json:"include_internal"`
	MinUsd             float64           `json:"min_usd"`
	Counterparties     []cpRow           `json:"counterparties"`
	BelowCutoff        remainder         `json:"below_cutoff"`
	Totals             totalsOut         `json:"totals"`
	Partial            bool              `json:"partial"`
	Cost               chainexec.CostOut `json:"cost"`
	TimingMS           int64             `json:"timing_ms"`
}

// shapeAddress turns one statement's document into the result: the returned
// rows, and the below_cutoff remainder from its WITH TOTALS row.
func shapeAddress(req request, doc map[string]any) result {
	res := result{
		Chain:   req.chain,
		GroupBy: "address",
		Window: windowOut{
			AfterTime:  sqlTimePlain(req.wStart),
			BeforeTime: sqlTimePlain(req.wEnd),
		},
		AddressesRequested: req.requested,
		AddressesUsed:      len(req.addrs),
		AddressesInvalid:   req.invalid,
		IncludeInternal:    req.includeInternal,
		MinUsd:             req.minUsd,
	}
	var sumUsd float64
	var sumTransfers int64
	for _, r := range chainexec.RowsOf(doc) {
		row := cpRow{
			Counterparty:         chainexec.AsStr(r["Counterparty"]),
			UsdIn:                asFloatPtr(r["Usd_In"]),
			UsdOut:               asFloatPtr(r["Usd_Out"]),
			UsdTotal:             asFloat(r["Usd_Total"]),
			TransfersIn:          chainexec.AsInt(r["Transfers_In"]),
			TransfersOut:         chainexec.AsInt(r["Transfers_Out"]),
			UnpricedTransfersIn:  chainexec.AsInt(r["Unpriced_Transfers_In"]),
			UnpricedTransfersOut: chainexec.AsInt(r["Unpriced_Transfers_Out"]),
			FirstSeen:            emptyTime(chainexec.AsStr(r["First_Seen"])),
			LastSeen:             emptyTime(chainexec.AsStr(r["Last_Seen"])),
		}
		res.Counterparties = append(res.Counterparties, row)
		sumUsd += row.UsdTotal
		sumTransfers += row.TransfersIn + row.TransfersOut
	}
	if tot := chainexec.TotalsOf(doc); tot != nil {
		res.Totals = totalsOut{
			UsdIn:        asFloatPtr(tot["Usd_In"]),
			UsdOut:       asFloatPtr(tot["Usd_Out"]),
			UsdTotal:     asFloat(tot["Usd_Total"]),
			TransfersIn:  chainexec.AsInt(tot["Transfers_In"]),
			TransfersOut: chainexec.AsInt(tot["Transfers_Out"]),
			NoPrice:      chainexec.AsInt(tot["T_No_Price"]),
			Suspect:      chainexec.AsInt(tot["T_Suspect"]),
			Stale:        chainexec.AsInt(tot["T_Stale"]),
		}
		res.BelowCutoff = remainder{
			Usd:       nonNeg(res.Totals.UsdTotal - sumUsd),
			Transfers: nonNegInt(res.Totals.TransfersIn + res.Totals.TransfersOut - sumTransfers),
		}
	}
	return res
}

func nonNeg(f float64) float64 {
	if f < 0 || math.IsNaN(f) {
		return 0
	}
	return f
}

func nonNegInt(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}

// markdown is the default rendering: a summary plus a counterparty CSV of at
// most 100 rows (format=json carries every returned row).
func (r result) markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Counterparties — %s\n\n", r.Chain)
	fmt.Fprintf(&b, "Window %s → %s UTC. Addresses: %d requested, %d used",
		r.Window.AfterTime, r.Window.BeforeTime, r.AddressesRequested, r.AddressesUsed)
	if len(r.AddressesInvalid) > 0 {
		fmt.Fprintf(&b, ", %d invalid (%s)", len(r.AddressesInvalid), strings.Join(r.AddressesInvalid, ", "))
	}
	b.WriteString(".\n\n")
	fmt.Fprintf(&b, "Returned the top %d counterparties by USD (in + out). ", len(r.Counterparties))
	fmt.Fprintf(&b, "Total flow $%s (in $%s, out $%s); below the cutoff $%s across %d transfers.\n",
		usd(r.Totals.UsdTotal), usdP(r.Totals.UsdIn), usdP(r.Totals.UsdOut), usd(r.BelowCutoff.Usd), r.BelowCutoff.Transfers)
	fmt.Fprintf(&b, "Unpriced transfers: %d no-price, %d suspect, %d stale.\n",
		r.Totals.NoPrice, r.Totals.Suspect, r.Totals.Stale)
	if r.Partial {
		b.WriteString("Partial: the call stopped on its budget before it was complete.\n")
	}
	b.WriteString("\ncounterparty,usd_in,usd_out,transfers_in,transfers_out,first_seen,last_seen\n")
	n := len(r.Counterparties)
	if n > 100 {
		n = 100
	}
	for _, c := range r.Counterparties[:n] {
		fmt.Fprintf(&b, "%s,%s,%s,%d,%d,%s,%s\n",
			c.Counterparty, usdP(c.UsdIn), usdP(c.UsdOut), c.TransfersIn, c.TransfersOut, c.FirstSeen, c.LastSeen)
	}
	return b.String()
}

func usd(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "n/a"
	}
	return strconv.FormatFloat(f, 'f', 2, 64)
}

func usdP(p *float64) string {
	if p == nil {
		return ""
	}
	return usd(*p)
}
