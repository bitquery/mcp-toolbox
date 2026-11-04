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
	"sort"
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
	// Label is the primary label type picked for the counterparty ("" = none);
	// LabelValue its value; Labels every distinct label type it carries.
	Label      string   `json:"label,omitempty"`
	LabelValue string   `json:"label_value,omitempty"`
	Labels     []string `json:"labels,omitempty"`
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
	b.WriteString("\ncounterparty,label,usd_in,usd_out,transfers_in,transfers_out,first_seen,last_seen\n")
	n := len(r.Counterparties)
	if n > 100 {
		n = 100
	}
	for _, c := range r.Counterparties[:n] {
		fmt.Fprintf(&b, "%s,%s,%s,%s,%d,%d,%s,%s\n",
			c.Counterparty, c.Label, usdP(c.UsdIn), usdP(c.UsdOut), c.TransfersIn, c.TransfersOut, c.FirstSeen, c.LastSeen)
	}
	return b.String()
}

// attachLabels picks a primary label type for each returned counterparty and
// records every type it carries. Fetched once for the returned page.
func (t Tool) attachLabels(r *result, byAddr map[string][]labelRow) {
	for i := range r.Counterparties {
		c := &r.Counterparties[i]
		lt, lv, types := t.lm.pick(byAddr[strings.ToLower(c.Counterparty)])
		c.Label, c.LabelValue, c.Labels = lt, lv, types
	}
}

// typeRow is one label type's share of the flow.
type typeRow struct {
	LabelType      string  `json:"label_type"`
	Counterparties int     `json:"counterparties"`
	Usd            float64 `json:"usd"`
	Share          float64 `json:"share"`
}

// typeResult is the group_by=label_type answer: the flow totalled by the
// primary label type of each returned counterparty, plus a (none) row for the
// unlabeled and a (below_cutoff) row for the counterparties past the top-K, so
// the shares sum to 100% of the base.
type typeResult struct {
	Chain              string            `json:"chain"`
	GroupBy            string            `json:"group_by"`
	Window             windowOut         `json:"window"`
	AddressesRequested int               `json:"addresses_requested"`
	AddressesUsed      int               `json:"addresses_used"`
	AddressesInvalid   []string          `json:"addresses_invalid,omitempty"`
	IncludeInternal    int               `json:"include_internal"`
	ShareBaseUsd       float64           `json:"share_base_usd"`
	Types              []typeRow         `json:"types"`
	Partial            bool              `json:"partial"`
	Cost               chainexec.CostOut `json:"cost"`
	TimingMS           int64             `json:"timing_ms"`
}

// shapeLabelType totals the returned counterparties' USD by their primary label
// type (unlabeled -> (none)), adds the below_cutoff remainder as its own row,
// and divides by the grand total so the shares sum to 100%.
func (t Tool) shapeLabelType(req request, doc map[string]any, byAddr map[string][]labelRow) typeResult {
	res := typeResult{
		Chain:   req.chain,
		GroupBy: "label_type",
		Window: windowOut{
			AfterTime:  sqlTimePlain(req.wStart),
			BeforeTime: sqlTimePlain(req.wEnd),
		},
		AddressesRequested: req.requested,
		AddressesUsed:      len(req.addrs),
		AddressesInvalid:   req.invalid,
		IncludeInternal:    req.includeInternal,
	}
	usdByType := map[string]float64{}
	cntByType := map[string]int{}
	for _, r := range chainexec.RowsOf(doc) {
		addr := strings.ToLower(chainexec.AsStr(r["Counterparty"]))
		lt, _, _ := t.lm.pick(byAddr[addr])
		if lt == "" {
			lt = noneLabel
		}
		usdByType[lt] += asFloat(r["Usd_Total"])
		cntByType[lt]++
	}
	var base float64
	var sumReturned float64
	if tot := chainexec.TotalsOf(doc); tot != nil {
		base = asFloat(tot["Usd_Total"])
	}
	for _, v := range usdByType {
		sumReturned += v
	}
	// The remainder: every counterparty past the top-K, as one bucket.
	below := nonNeg(base - sumReturned)
	res.ShareBaseUsd = base
	for lt, usd := range usdByType {
		res.Types = append(res.Types, typeRow{LabelType: lt, Counterparties: cntByType[lt], Usd: usd, Share: shareOf(usd, base)})
	}
	if below > 0 {
		res.Types = append(res.Types, typeRow{LabelType: belowCutoffLabel, Usd: below, Share: shareOf(below, base)})
	}
	sort.Slice(res.Types, func(i, j int) bool { return res.Types[i].Usd > res.Types[j].Usd })
	return res
}

func shareOf(a, base float64) float64 {
	if base <= 0 || math.IsNaN(base) {
		return 0
	}
	return a / base
}

func (r typeResult) markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Counterparties by label type — %s\n\n", r.Chain)
	fmt.Fprintf(&b, "Window %s → %s UTC. Addresses: %d requested, %d used.\n", r.Window.AfterTime, r.Window.BeforeTime, r.AddressesRequested, r.AddressesUsed)
	fmt.Fprintf(&b, "Share base (total flow in + out): $%s.\n", usd(r.ShareBaseUsd))
	if r.Partial {
		b.WriteString("Partial: the labels were not all read before the budget; shares are over what was.\n")
	}
	b.WriteString("\nlabel_type,counterparties,usd,share_pct\n")
	for _, ty := range r.Types {
		fmt.Fprintf(&b, "%s,%d,%s,%.2f\n", ty.LabelType, ty.Counterparties, usd(ty.Usd), ty.Share*100)
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
