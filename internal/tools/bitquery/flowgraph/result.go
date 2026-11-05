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
	Label       string      `json:"label,omitempty"`
	LabelValue  string      `json:"label_value,omitempty"`
	Labels      []string    `json:"labels,omitempty"`
	LabelStatus labelStatus `json:"label_status"`
	// TokenCount is the number of distinct tokens moved with the counterparty;
	// Tokens is the top-10 of them by USD (unpriced last). SetMembers is which of
	// the input addresses dealt with this counterparty — emitted only for a set of
	// more than one address (for a single address it is trivially that address).
	TokenCount int64        `json:"token_count"`
	Tokens     []tokenEntry `json:"tokens,omitempty"`
	SetMembers []string     `json:"set_members,omitempty"`
}

// tokenEntry is one token's flow with a counterparty, as the statement emits it in
// the Tokens array: the token already chain-encoded (0x contract on EVM, base58 on
// Tron, "native" for the chain coin, "trc10:<id>" for a TRC-10), decimal-string
// amounts in token units, and USD null for a side with no priced transfer.
type tokenEntry struct {
	Token        string   `json:"token"`
	AmountIn     string   `json:"amount_in"`
	AmountOut    string   `json:"amount_out"`
	UsdIn        *float64 `json:"usd_in"`
	UsdOut       *float64 `json:"usd_out"`
	TransfersIn  int64    `json:"transfers_in"`
	TransfersOut int64    `json:"transfers_out"`
}

// parseTokens turns the statement's Tokens column (an array of positional tuples)
// into typed entries; a malformed element is skipped.
func parseTokens(v any) []tokenEntry {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]tokenEntry, 0, len(arr))
	for _, e := range arr {
		t, ok := e.([]any)
		if !ok || len(t) < 7 {
			continue
		}
		out = append(out, tokenEntry{
			Token:        chainexec.AsStr(t[0]),
			AmountIn:     chainexec.AsStr(t[1]),
			AmountOut:    chainexec.AsStr(t[2]),
			UsdIn:        asFloatPtr(t[3]),
			UsdOut:       asFloatPtr(t[4]),
			TransfersIn:  chainexec.AsInt(t[5]),
			TransfersOut: chainexec.AsInt(t[6]),
		})
	}
	return out
}

// parseStrings turns a statement array column into a string slice.
func parseStrings(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		out = append(out, chainexec.AsStr(e))
	}
	return out
}

func ptrOr0(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
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
	// SelfTransfers is the count of transfers between two input-set addresses
	// (intra-set), excluded from the counterparty rows and the totals and counted
	// here once; 0 for a single-address set.
	SelfTransfers int64 `json:"self_transfers"`
	// ShareBaseUsd*/LabeledUsdShare* describe the RETURNED page (the rows we hold
	// labels for), not the full flow: ShareBaseUsd_In = sum of priced Usd_In over the
	// returned counterparties; LabeledUsdShare_In = that fraction carried by a labeled
	// counterparty. null when the page has no priced transfer on that side.
	ShareBaseUsdIn     *float64 `json:"share_base_usd_in"`
	ShareBaseUsdOut    *float64 `json:"share_base_usd_out"`
	LabeledUsdShareIn  *float64 `json:"labeled_usd_share_in"`
	LabeledUsdShareOut *float64 `json:"labeled_usd_share_out"`
	// FilteredOut is how many of the page's counterparties label_type /
	// exclude_label_type dropped. Totals and BelowCutoff are NOT recomputed for a
	// filter — they stay the full-flow numbers — so this tells a filtered page from a
	// short one. Walk the cursor to reach every matching counterparty.
	FilteredOut int `json:"filtered_out,omitempty"`
	// PickRule is the label-pick rule, verbatim, on every call (BIT-16068 section 3).
	PickRule string `json:"pick_rule"`
	Partial  bool   `json:"partial"`
	// NextCursor is set when the returned page filled the limit, so more
	// counterparties follow: pass it back as cursor (with the same addresses,
	// window and filters) for the next page. Empty = the last page, or a Partial
	// stop (which carries no cursor).
	NextCursor string `json:"next_cursor,omitempty"`
	// Hint is a narrowing suggestion carried on a Partial stop (shorten the
	// window, pin a token, raise min_usd), so a call that always stops is still
	// actionable.
	Hint     string            `json:"hint,omitempty"`
	Cost     chainexec.CostOut `json:"cost"`
	TimingMS int64             `json:"timing_ms"`
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
			TokenCount:           chainexec.AsInt(r["Token_Count"]),
			Tokens:               parseTokens(r["Tokens"]),
		}
		if len(req.addrs) > 1 {
			row.SetMembers = parseStrings(r["Set_Members"])
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
		// BelowCutoff is the remainder below the top-K, over the UNFILTERED page sums,
		// so a label filter does not change it (Filtered_Out records the filter).
		res.BelowCutoff = remainder{
			Usd:       nonNeg(res.Totals.UsdTotal - sumUsd),
			Transfers: nonNegInt(res.Totals.TransfersIn + res.Totals.TransfersOut - sumTransfers),
		}
		res.SelfTransfers = chainexec.AsInt(tot["Self_Transfers"])
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
	fmt.Fprintf(&b, "Returned %d counterparties by USD (in + out)", len(r.Counterparties))
	if r.FilteredOut > 0 {
		fmt.Fprintf(&b, " (%d more on the page were dropped by the label filter; walk the cursor for further matches)", r.FilteredOut)
	}
	b.WriteString(". ")
	fmt.Fprintf(&b, "Total flow $%s (in $%s, out $%s); ", usd(r.Totals.UsdTotal), usdP(r.Totals.UsdIn), usdP(r.Totals.UsdOut))
	if r.BelowCutoff.Usd == 0 && r.BelowCutoff.Transfers > 0 {
		fmt.Fprintf(&b, "below the cutoff: unpriced ($0 priced) across %d transfers.\n", r.BelowCutoff.Transfers)
	} else {
		fmt.Fprintf(&b, "below the cutoff $%s across %d transfers.\n", usd(r.BelowCutoff.Usd), r.BelowCutoff.Transfers)
	}
	fmt.Fprintf(&b, "Unpriced transfers: %d no-price, %d suspect, %d stale.",
		r.Totals.NoPrice, r.Totals.Suspect, r.Totals.Stale)
	if r.SelfTransfers > 0 {
		fmt.Fprintf(&b, " Self-transfers (intra-set, excluded from the above): %d.", r.SelfTransfers)
	}
	b.WriteString("\n")
	if r.Partial {
		b.WriteString("Partial: the call stopped on its budget before it was complete.\n")
		if r.Hint != "" {
			fmt.Fprintf(&b, "%s\n", r.Hint)
		}
	}
	if r.NextCursor != "" {
		fmt.Fprintf(&b, "More counterparties follow — pass cursor=%s with the same addresses, window and filters for the next page.\n", r.NextCursor)
	}
	if len(r.Counterparties) > 100 {
		fmt.Fprintf(&b, "Showing 100 of %d counterparties on this page; format=json returns all.\n", len(r.Counterparties))
	}
	b.WriteString("\ncounterparty,label,label_status,usd_in,usd_out,transfers_in,transfers_out,unpriced_in,unpriced_out,token_count,first_seen,last_seen\n")
	n := len(r.Counterparties)
	if n > 100 {
		n = 100
	}
	for _, c := range r.Counterparties[:n] {
		fmt.Fprintf(&b, "%s,%s,%s,%s,%s,%d,%d,%d,%d,%d,%s,%s\n",
			c.Counterparty, c.Label, c.LabelStatus, usdP(c.UsdIn), usdP(c.UsdOut),
			c.TransfersIn, c.TransfersOut, c.UnpricedTransfersIn, c.UnpricedTransfersOut, c.TokenCount,
			c.FirstSeen, c.LastSeen)
	}
	if r.PickRule != "" {
		fmt.Fprintf(&b, "\n%s\n", r.PickRule)
	}
	return b.String()
}

// attachLabels picks a primary label type for each returned counterparty, records
// every type it carries and its Label_Status, applies the label_type /
// exclude_label_type filters (dropping the non-matching rows and counting them in
// Filtered_Out — Totals/BelowCutoff are NOT recomputed), and fills the share base
// over the returned page. Pick_Rule is recorded for the call.
func (t Tool) attachLabels(req request, r *result, byAddr map[string][]labelRow) {
	r.PickRule = t.lm.PickRule()
	for i := range r.Counterparties {
		c := &r.Counterparties[i]
		lt, lv, types := t.lm.pick(byAddr[strings.ToLower(c.Counterparty)])
		c.Label, c.LabelValue, c.Labels = lt, lv, types
		if lt != "" {
			c.LabelStatus = statusLabeled
		} else {
			c.LabelStatus = statusUnlabeled
		}
	}
	if len(req.labelTypes) > 0 || len(req.excludeLabelTypes) > 0 {
		keep := r.Counterparties[:0]
		for _, c := range r.Counterparties {
			if matchLabelFilter(c.Label, req.labelTypes, req.excludeLabelTypes) {
				keep = append(keep, c)
			} else {
				r.FilteredOut++
			}
		}
		r.Counterparties = keep
	}
	// Share base + labeled share over the RETURNED (filtered) page — the only rows
	// we hold labels for; not the full flow (below-cutoff counterparties are unlabeled
	// here). null on a side with no priced transfer on the page.
	var baseIn, baseOut, labIn, labOut float64
	var anyIn, anyOut bool
	for i := range r.Counterparties {
		c := &r.Counterparties[i]
		if c.UsdIn != nil {
			anyIn = true
			baseIn += *c.UsdIn
			if c.Label != "" {
				labIn += *c.UsdIn
			}
		}
		if c.UsdOut != nil {
			anyOut = true
			baseOut += *c.UsdOut
			if c.Label != "" {
				labOut += *c.UsdOut
			}
		}
	}
	if anyIn {
		r.ShareBaseUsdIn = &baseIn
		if baseIn > 0 {
			s := labIn / baseIn
			r.LabeledUsdShareIn = &s
		}
	}
	if anyOut {
		r.ShareBaseUsdOut = &baseOut
		if baseOut > 0 {
			s := labOut / baseOut
			r.LabeledUsdShareOut = &s
		}
	}
}

// cpBrief names a counterparty and its USD (in + out), for a label type's top few.
type cpBrief struct {
	Counterparty string  `json:"counterparty"`
	Usd          float64 `json:"usd"`
}

// typeRow is one label type's share of the flow: the combined share plus the per-
// direction shares and the type's largest counterparties.
type typeRow struct {
	LabelType         string    `json:"label_type"`
	Counterparties    int       `json:"counterparties"`
	Usd               float64   `json:"usd"`
	Share             float64   `json:"share"`
	ShareIn           *float64  `json:"share_in"`
	ShareOut          *float64  `json:"share_out"`
	TopCounterparties []cpBrief `json:"top_counterparties,omitempty"`
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
	ShareBaseUsdIn     float64           `json:"share_base_usd_in"`
	ShareBaseUsdOut    float64           `json:"share_base_usd_out"`
	SelfTransfers      int64             `json:"self_transfers"`
	Types              []typeRow         `json:"types"`
	PickRule           string            `json:"pick_rule"`
	Partial            bool              `json:"partial"`
	Hint               string            `json:"hint,omitempty"`
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
	res.PickRule = t.lm.PickRule()
	filtered := len(req.labelTypes) > 0 || len(req.excludeLabelTypes) > 0

	type agg struct {
		usd, usdIn, usdOut float64
		cnt                int
		tops               []cpBrief
	}
	byType := map[string]*agg{}
	var order []string
	for _, r := range chainexec.RowsOf(doc) {
		addr := strings.ToLower(chainexec.AsStr(r["Counterparty"]))
		lt, _, _ := t.lm.pick(byAddr[addr])
		// The filter is on the picked primary type; (none) is unlabeled, so it can
		// never be in an include list and is not in exclude.
		if filtered && !matchLabelFilter(lt, req.labelTypes, req.excludeLabelTypes) {
			continue
		}
		display := lt
		if display == "" {
			display = noneLabel
		}
		a := byType[display]
		if a == nil {
			a = &agg{}
			byType[display] = a
			order = append(order, display)
		}
		cpUsd := asFloat(r["Usd_Total"])
		a.usd += cpUsd
		a.usdIn += ptrOr0(asFloatPtr(r["Usd_In"]))
		a.usdOut += ptrOr0(asFloatPtr(r["Usd_Out"]))
		a.cnt++
		a.tops = append(a.tops, cpBrief{Counterparty: chainexec.AsStr(r["Counterparty"]), Usd: cpUsd})
	}

	tot := chainexec.TotalsOf(doc)
	if tot != nil {
		res.SelfTransfers = chainexec.AsInt(tot["Self_Transfers"])
	}
	// Share base. Unfiltered: the WITH TOTALS grand total (in/out/combined), with a
	// (below_cutoff) row for the counterparties past the top-K so shares sum to 100%.
	// Filtered: the sum over the matching types only (shown types sum to 100% of the
	// filtered population) and NO (below_cutoff) row — the below-cutoff types are unknown.
	var base, baseIn, baseOut float64
	if filtered {
		for _, a := range byType {
			base += a.usd
			baseIn += a.usdIn
			baseOut += a.usdOut
		}
	} else if tot != nil {
		base = asFloat(tot["Usd_Total"])
		baseIn = ptrOr0(asFloatPtr(tot["Usd_In"]))
		baseOut = ptrOr0(asFloatPtr(tot["Usd_Out"]))
	}
	res.ShareBaseUsd, res.ShareBaseUsdIn, res.ShareBaseUsdOut = base, baseIn, baseOut

	var retUsd, retIn, retOut float64
	for _, a := range byType {
		retUsd += a.usd
		retIn += a.usdIn
		retOut += a.usdOut
	}
	for _, lt := range order {
		a := byType[lt]
		sort.Slice(a.tops, func(i, j int) bool { return a.tops[i].Usd > a.tops[j].Usd })
		top := a.tops
		if len(top) > 3 {
			top = top[:3]
		}
		res.Types = append(res.Types, typeRow{
			LabelType:         lt,
			Counterparties:    a.cnt,
			Usd:               a.usd,
			Share:             shareOf(a.usd, base),
			ShareIn:           shareOfP(a.usdIn, baseIn),
			ShareOut:          shareOfP(a.usdOut, baseOut),
			TopCounterparties: append([]cpBrief(nil), top...),
		})
	}
	if !filtered {
		if below := nonNeg(base - retUsd); below > 0 {
			res.Types = append(res.Types, typeRow{
				LabelType: belowCutoffLabel,
				Usd:       below,
				Share:     shareOf(below, base),
				ShareIn:   shareOfP(nonNeg(baseIn-retIn), baseIn),
				ShareOut:  shareOfP(nonNeg(baseOut-retOut), baseOut),
			})
		}
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

// shareOfP is shareOf for a side whose base can be null (no priced transfer on that
// side): null, not 0, so a reader does not read "0% in" where there simply was no
// priced inflow — the null-not-0 rule the rest of the tool follows.
func shareOfP(a, base float64) *float64 {
	if base <= 0 || math.IsNaN(base) {
		return nil
	}
	s := a / base
	return &s
}

// pctP renders a *share as a percentage, "" when null.
func pctP(p *float64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatFloat(*p*100, 'f', 2, 64)
}

func (r typeResult) markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Counterparties by label type — %s\n\n", r.Chain)
	fmt.Fprintf(&b, "Window %s → %s UTC. Addresses: %d requested, %d used.\n", r.Window.AfterTime, r.Window.BeforeTime, r.AddressesRequested, r.AddressesUsed)
	fmt.Fprintf(&b, "Share base (total flow in + out): $%s (in $%s, out $%s).\n", usd(r.ShareBaseUsd), usd(r.ShareBaseUsdIn), usd(r.ShareBaseUsdOut))
	if r.SelfTransfers > 0 {
		fmt.Fprintf(&b, "Self-transfers (intra-set, excluded): %d.\n", r.SelfTransfers)
	}
	if r.Partial {
		b.WriteString("Partial: the call stopped on its budget before it was complete; shares are over what was read.\n")
		if r.Hint != "" {
			fmt.Fprintf(&b, "%s\n", r.Hint)
		}
	}
	b.WriteString("\nlabel_type,counterparties,usd,share_pct,share_in_pct,share_out_pct,top_counterparties\n")
	for _, ty := range r.Types {
		tops := make([]string, 0, len(ty.TopCounterparties))
		for _, c := range ty.TopCounterparties {
			tops = append(tops, c.Counterparty)
		}
		fmt.Fprintf(&b, "%s,%d,%s,%.2f,%s,%s,%s\n",
			ty.LabelType, ty.Counterparties, usd(ty.Usd), ty.Share*100, pctP(ty.ShareIn), pctP(ty.ShareOut), strings.Join(tops, ";"))
	}
	if r.PickRule != "" {
		fmt.Fprintf(&b, "\n%s\n", r.PickRule)
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
