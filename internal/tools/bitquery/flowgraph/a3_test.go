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

import (
	"strings"
	"testing"
)

func fp(v float64) *float64 { return &v }

// addrA is a valid lower-hex EVM address for the parse tests (local to avoid a
// cross-file dependency on engine_test's fixture).
const a3AddrA = "0x1111111111111111111111111111111111111111"

func a3Result() result {
	return result{
		Counterparties: []cpRow{
			{Counterparty: "0xaaa", UsdIn: fp(100), UsdTotal: 100}, // labeled dex-router
			{Counterparty: "0xbbb", UsdOut: fp(50), UsdTotal: 50},  // labeled asset-manager
			{Counterparty: "0xccc", UsdIn: fp(100), UsdTotal: 100}, // unlabeled, priced in
		},
		BelowCutoff: remainder{Usd: 350, Transfers: 22},
	}
}

func a3ByAddr() map[string][]labelRow {
	return map[string][]labelRow{
		"0xaaa": {{Address: "0xaaa", LabelType: "dex-router", Value: "cow", Source: "v"}},
		"0xbbb": {{Address: "0xbbb", LabelType: "asset-manager", Value: "wm", Source: "v"}},
		"0xccc": {}, // looked up, no labels at all -> Label_Status none
	}
}

func TestLabelFilterInclude(t *testing.T) {
	tool := Tool{lm: testLabelMethod(t)}
	res := a3Result()
	tool.attachLabels(request{labelTypes: []string{"dex-router"}}, &res, a3ByAddr(), false)
	if len(res.Counterparties) != 1 || res.Counterparties[0].Counterparty != "0xaaa" {
		t.Fatalf("include filter: want only 0xaaa, got %+v", res.Counterparties)
	}
	if res.FilteredOut != 2 {
		t.Errorf("FilteredOut = %d, want 2", res.FilteredOut)
	}
	if res.BelowCutoff.Usd != 350 || res.BelowCutoff.Transfers != 22 {
		t.Errorf("BelowCutoff must be unchanged by the filter, got %+v", res.BelowCutoff)
	}
	if res.Counterparties[0].LabelStatus != statusLabeled {
		t.Errorf("label_status = %q, want labeled", res.Counterparties[0].LabelStatus)
	}
}

func TestLabelFilterExclude(t *testing.T) {
	tool := Tool{lm: testLabelMethod(t)}
	res := a3Result()
	// exclude dex-router: drop 0xaaa; keep 0xbbb (asset-manager) and 0xccc (no primary
	// label is NOT removed by exclude).
	tool.attachLabels(request{excludeLabelTypes: []string{"dex-router"}}, &res, a3ByAddr(), false)
	got := map[string]bool{}
	for _, c := range res.Counterparties {
		got[c.Counterparty] = true
	}
	if len(res.Counterparties) != 2 || !got["0xbbb"] || !got["0xccc"] {
		t.Fatalf("exclude filter: want 0xbbb + 0xccc, got %+v", res.Counterparties)
	}
	if res.FilteredOut != 1 {
		t.Errorf("FilteredOut = %d, want 1", res.FilteredOut)
	}
}

func TestNoFilterKeepsAllAndSetsStatus(t *testing.T) {
	tool := Tool{lm: testLabelMethod(t)}
	res := a3Result()
	tool.attachLabels(request{}, &res, a3ByAddr(), false)
	if len(res.Counterparties) != 3 || res.FilteredOut != 0 {
		t.Fatalf("no filter: want all 3, FilteredOut 0; got %d rows, %d filtered", len(res.Counterparties), res.FilteredOut)
	}
	// 0xccc was looked up and carries NO label rows -> none (not unlabeled, which is
	// reserved for a counterparty that has labels but only never-pick attribute types).
	if res.Counterparties[2].LabelStatus != statusNone {
		t.Errorf("0xccc label_status = %q, want none", res.Counterparties[2].LabelStatus)
	}
	if res.PickRule == "" {
		t.Error("Pick_Rule must be set on every call")
	}
	// Share base over the returned page: in = 100 (0xaaa, labeled) + 100 (0xccc,
	// unlabeled) = 200; labeled in = 100 → labeled share 0.5 (discriminates
	// "computed" from "always 1").
	if res.ShareBaseUsdIn == nil || *res.ShareBaseUsdIn != 200 {
		t.Errorf("ShareBaseUsdIn = %v, want 200", res.ShareBaseUsdIn)
	}
	if res.LabeledUsdShareIn == nil || *res.LabeledUsdShareIn != 0.5 {
		t.Errorf("LabeledUsdShareIn = %v, want 0.5 (100 of 200 priced-in is labeled)", res.LabeledUsdShareIn)
	}
}

// TestNeverPickGuardOnlyAsGoodAsCfg pins the advisor's caveat: the parse-time
// rejection of a never-pick filter value is only as strong as the tool's cfg
// neverPick list. testTool() has an EMPTY neverPick, so it ACCEPTS a value that the
// real counterparties cfg (13 never-pick types) would reject — a second tool of
// this kind must populate neverPick or the guard is a no-op.
func TestNeverPickGuardOnlyAsGoodAsCfg(t *testing.T) {
	tr := testTool() // empty neverPick
	if _, err := tr.parse(map[string]any{"chain": "ethereum", "addresses": a3AddrA, "label_type": "high-transacting"}); err != nil {
		t.Errorf("with an empty neverPick cfg the guard cannot fire, so the value is accepted; got %v", err)
	}
}

func TestNeverPickRejectedAtParse(t *testing.T) {
	tr := testTool()
	tr.lm = testLabelMethod(t) // has high-transacting in never-pick
	if _, err := tr.parse(map[string]any{"chain": "ethereum", "addresses": a3AddrA, "label_type": "high-transacting"}); err == nil {
		t.Error("label_type=high-transacting (a never-pick attribute) must be rejected at parse")
	}
	if _, err := tr.parse(map[string]any{"chain": "ethereum", "addresses": a3AddrA, "exclude_label_type": "spam"}); err == nil {
		t.Error("exclude_label_type=spam (a never-pick attribute) must be rejected at parse")
	}
	// a real primary type is accepted
	if _, err := tr.parse(map[string]any{"chain": "ethereum", "addresses": a3AddrA, "label_type": "dex-router"}); err != nil {
		t.Errorf("label_type=dex-router must be accepted, got %v", err)
	}
}

func TestSetMembersOnlyMultiAddress(t *testing.T) {
	doc := map[string]any{
		"data": []any{
			map[string]any{"Counterparty": "0xaaa", "Usd_Total": 1.0, "Set_Members": []any{"0xS1", "0xS2"},
				"Token_Count": "2", "Transfers_In": "1", "Transfers_Out": "0"},
		},
		"totals": map[string]any{"Usd_Total": 1.0, "Transfers_In": "1", "Transfers_Out": "0"},
	}
	single := shapeAddress(request{chain: "ethereum", addrs: []string{"0xone"}}, doc)
	if single.Counterparties[0].SetMembers != nil {
		t.Errorf("single address: Set_Members must be omitted, got %v", single.Counterparties[0].SetMembers)
	}
	multi := shapeAddress(request{chain: "ethereum", addrs: []string{"0xa", "0xb"}}, doc)
	if len(multi.Counterparties[0].SetMembers) != 2 {
		t.Errorf("multi address: Set_Members must be present, got %v", multi.Counterparties[0].SetMembers)
	}
	if multi.Counterparties[0].TokenCount != 2 {
		t.Errorf("Token_Count = %d, want 2", multi.Counterparties[0].TokenCount)
	}
}

func TestTokensSurfaced(t *testing.T) {
	doc := map[string]any{
		"data": []any{
			map[string]any{"Counterparty": "0xaaa", "Usd_Total": 1.0,
				"Tokens": []any{
					[]any{"0xdac17f958d2ee523a2206206994597c13d831ec7", "87000", "0", 86890.12, nil, "1", "0"},
				},
			},
		},
		"totals": map[string]any{"Usd_Total": 1.0},
	}
	res := shapeAddress(request{chain: "ethereum", addrs: []string{"0xone"}}, doc)
	tk := res.Counterparties[0].Tokens
	if len(tk) != 1 || tk[0].AmountIn != "87000" || tk[0].UsdIn == nil || tk[0].UsdOut != nil || tk[0].TransfersIn != 1 {
		t.Fatalf("Tokens not surfaced correctly: %+v", tk)
	}
}

func TestLabelTypeBaseExcludesNone(t *testing.T) {
	tool := Tool{lm: testLabelMethod(t)}
	doc := map[string]any{
		"data": []any{
			map[string]any{"Counterparty": "0xAAA", "Usd_Total": 100.0, "Usd_In": 100.0, "Usd_Out": nil},
			map[string]any{"Counterparty": "0xBBB", "Usd_Total": 50.0, "Usd_In": nil, "Usd_Out": 50.0}, // unlabeled
		},
		"totals": map[string]any{"Usd_Total": 500.0, "Usd_In": 300.0, "Usd_Out": 200.0},
	}
	byAddr := map[string][]labelRow{"0xaaa": {{Address: "0xaaa", LabelType: "dex-router", Value: "cow", Source: "v"}}}

	// label_type=dex-router: (none) is excluded from the base; base = 100; no below_cutoff row.
	inc := tool.shapeLabelType(request{labelTypes: []string{"dex-router"}}, doc, byAddr)
	if inc.ShareBaseUsd != 100 {
		t.Errorf("include base = %v, want 100 (none excluded)", inc.ShareBaseUsd)
	}
	for _, ty := range inc.Types {
		if ty.LabelType == noneLabel || ty.LabelType == belowCutoffLabel {
			t.Errorf("include filter must not emit %q", ty.LabelType)
		}
	}

	// exclude_label_type=asset-manager: (none) is kept; base = 100 + 50 = 150.
	exc := tool.shapeLabelType(request{excludeLabelTypes: []string{"asset-manager"}}, doc, byAddr)
	if exc.ShareBaseUsd != 150 {
		t.Errorf("exclude base = %v, want 150 (none kept)", exc.ShareBaseUsd)
	}
	var hasNone bool
	for _, ty := range exc.Types {
		if ty.LabelType == noneLabel {
			hasNone = true
		}
	}
	if !hasNone {
		t.Error("exclude-only filter must keep the (none) row")
	}
}

func TestShapeLabelTypeUnfilteredHasBelowCutoff(t *testing.T) {
	tool := Tool{lm: testLabelMethod(t)}
	doc := map[string]any{
		"data": []any{
			map[string]any{"Counterparty": "0xAAA", "Usd_Total": 100.0, "Usd_In": 100.0, "Usd_Out": nil},
		},
		"totals": map[string]any{"Usd_Total": 500.0, "Usd_In": 300.0, "Usd_Out": 200.0, "Self_Transfers": "0"},
	}
	byAddr := map[string][]labelRow{"0xaaa": {{Address: "0xaaa", LabelType: "dex-router", Value: "c", Source: "v"}}}
	res := tool.shapeLabelType(request{}, doc, byAddr)
	if res.ShareBaseUsd != 500 {
		t.Errorf("unfiltered base = %v, want the grand total 500", res.ShareBaseUsd)
	}
	var below *typeRow
	for i := range res.Types {
		if res.Types[i].LabelType == belowCutoffLabel {
			below = &res.Types[i]
		}
	}
	if below == nil || below.Usd != 400 {
		t.Fatalf("want a (below_cutoff) row of 400, got %+v", res.Types)
	}
	if !strings.Contains(res.markdown(), "share_in_pct") {
		t.Error("label_type markdown missing the new share_in_pct column")
	}
}
