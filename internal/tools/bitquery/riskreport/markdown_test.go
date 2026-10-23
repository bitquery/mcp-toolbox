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

import (
	"fmt"
	"strings"
	"testing"
)

// P1-1: a poisoning flag with 1053 look-alikes is trimmed to the top 20 by
// transfers (with lookalikes_total / lookalikes_omitted and a few evidence
// examples), and the markdown report stays small (<= 8 KB) with data_as_of in
// the first 500 bytes.
func TestMarkdownBoundedOnManyLookalikes(t *testing.T) {
	imitated := synth(t, "LK", 1)
	var looks []lookalike
	for i := 1; i <= 1053; i++ {
		looks = append(looks, lookalike{
			Address: synth(t, "LK", i), Imitates: imitated, Kind: "asset_small_transfer",
			Transfers: int64(i), EvidenceTxs: []string{fmt.Sprintf("tx%04d", i)},
		})
	}
	flag := poisoningFlag(looks)
	kept := flag.Detail["lookalikes"].([]lookalike)
	if len(kept) != lookalikeListCap {
		t.Fatalf("kept %d look-alikes, want %d", len(kept), lookalikeListCap)
	}
	if flag.Detail["lookalikes_total"].(int) != 1053 || flag.Detail["lookalikes_omitted"].(int) != 1053-lookalikeListCap {
		t.Errorf("total/omitted = %v/%v, want 1053/%d", flag.Detail["lookalikes_total"], flag.Detail["lookalikes_omitted"], 1053-lookalikeListCap)
	}
	if flag.Detail["count"].(int) != 1053 {
		t.Errorf("count = %v, want 1053 (the total is kept)", flag.Detail["count"])
	}
	// The kept ones are the largest by transfers.
	if kept[0].Transfers != 1053 || kept[len(kept)-1].Transfers != int64(1053-lookalikeListCap+1) {
		t.Errorf("kept not the top by transfers: first %d, last %d", kept[0].Transfers, kept[len(kept)-1].Transfers)
	}
	// The flag-level evidence is a few examples, all distinct (not every
	// look-alike's transactions repeated).
	if len(flag.EvidenceTxs) > poisoningEvidenceCap {
		t.Errorf("evidence_txs = %d, over the cap %d", len(flag.EvidenceTxs), poisoningEvidenceCap)
	}
	seen := map[string]bool{}
	for _, tx := range flag.EvidenceTxs {
		if seen[tx] {
			t.Errorf("duplicated evidence tx %s", tx)
		}
		seen[tx] = true
	}

	rep := &reportOut{
		Summary: summaryOut{Chain: "tron", Address: imitated, Asset: assetOut{Symbol: "USDT"}, Received: "1.0", Sent: "0.0", Balance: "1.0"},
		Score:   scoreOut{Value: 0},
		Params: paramsOut{DataAsOf: "2026-10-01T00:00:00Z", View: "direct", Complete: true, MaxHopsReached: 1,
			USDConversion: "constant: 1 USDT = 1 USD", FlagsStatus: map[string]string{"pass_through": "not_found", "frozen": "not_evaluated"}},
		Flags: []flagOut{flag},
	}
	md := markdownReport(rep)
	t.Logf("markdown on 1053 look-alikes = %d bytes", len(md))
	if len(md) > 8192 {
		t.Errorf("markdown = %d bytes, want <= 8192", len(md))
	}
	if i := strings.Index(md, "2026-10-01T00:00:00Z"); i < 0 || i >= 500 {
		t.Errorf("data_as_of at byte %d, want within the first 500", i)
	}
	if !strings.Contains(md, "poisoning: found (1053 look-alike(s))") {
		t.Errorf("markdown missing the poisoning headline:\n%s", md)
	}
	// Every flag is listed in the fixed order, direct sanctions first, with its
	// status word (none translated to yes/no).
	for _, want := range []string{"- direct sanctions: not_found", "- risky dust: not_found",
		"- other-asset direct risk: not_found", "- pass-through: not_found", "- frozen: not_evaluated"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown Flags section missing %q:\n%s", want, md)
		}
	}
	if di, pi := strings.Index(md, "- direct sanctions:"), strings.Index(md, "- poisoning:"); di < 0 || pi < 0 || di > pi {
		t.Errorf("direct sanctions must lead the flags, before poisoning (direct at %d, poisoning at %d)", di, pi)
	}
}

// P3-1 / P3-2: when no counterparty flow was scored (score 0, empty top_drivers,
// no direct floor), the markdown still shows a Drivers block, and the Exposure
// says the flow was at or below min_amount_usd instead of an "unattributed 0, 0"
// line that reads as "all attributed".
func TestMarkdownEmptyScoredFlow(t *testing.T) {
	rep := &reportOut{
		Summary:      summaryOut{Chain: "tron", Address: synth(t, "T", 1), Asset: assetOut{Symbol: "TRX"}, Received: "0.100056", Sent: "0.010720", Balance: "0.089336"},
		Score:        scoreOut{Value: 0, TopDrivers: []driver{}},
		Exposure:     noExposureFlagged,
		Unattributed: map[string]string{"received": "0.000000", "sent": "0.000000"},
		Params: paramsOut{DataAsOf: "2026-10-23T00:00:00Z", View: "direct", Complete: true, MinAmountUSD: 100,
			USDConversion: "store: 1 TRX = 0.33 USD (as of 2026-10-23T00:00:00Z)",
			FlagsStatus:   map[string]string{"poisoning": "found", "pass_through": "found", "frozen": "not_evaluated"}},
		Flags: []flagOut{{Type: "poisoning", Detail: map[string]any{"count": 3}}},
	}
	md := markdownReport(rep)
	if !strings.Contains(md, "Drivers (share of the scored flow → contribution to the score):") {
		t.Errorf("no Drivers header on an empty-flow report:\n%s", md)
	}
	if !strings.Contains(md, "- none: all transfers are at or below min_amount_usd (100 USD), so no flow was scored") {
		t.Errorf("Drivers 'none' line missing or wrong:\n%s", md)
	}
	if !strings.Contains(md, "All transfers are at or below min_amount_usd (100 USD), so no flow was scored — see flags.") {
		t.Errorf("Exposure does not explain the empty flow with a flags pointer:\n%s", md)
	}
	// The contradictory "no labelled exposure in the scored flow" line is gone.
	if strings.Contains(md, "no labelled exposure in the scored flow") {
		t.Errorf("the contradictory no-exposure line is still present:\n%s", md)
	}
	if strings.Contains(md, "Unattributed TRX: received 0.000000") {
		t.Errorf("the misleading zero Unattributed line is still present:\n%s", md)
	}
	// With no flags (noExposure), the one line carries no "see flags" pointer.
	rep.Exposure = noExposure
	rep.Flags = nil
	rep.Params.FlagsStatus = map[string]string{"frozen": "not_evaluated"}
	md2 := markdownReport(rep)
	if !strings.Contains(md2, "All transfers are at or below min_amount_usd (100 USD), so no flow was scored.") {
		t.Errorf("no-flags empty report should end the line at 'scored.':\n%s", md2)
	}
	if strings.Contains(md2, "so no flow was scored — see flags") {
		t.Errorf("no-flags empty report must not point at flags:\n%s", md2)
	}
}

// With an empty top_drivers but a nonzero direct floor, the score comes from the
// floor; the Drivers block says so and the Exposure keeps its normal line (the
// below-threshold sentence is only for a zero-floor empty report).
func TestMarkdownEmptyDriversWithDirectFloor(t *testing.T) {
	rep := &reportOut{
		Summary:      summaryOut{Chain: "tron", Address: synth(t, "T", 2), Asset: assetOut{Symbol: "USDT"}, Received: "5.0", Sent: "0.0", Balance: "5.0"},
		Score:        scoreOut{Value: 100, DirectFloor: 100, TopDrivers: []driver{}, DirectFloorFrom: map[string]any{"entity": "ml", "entity_type": "Illegal Service", "direction": "received"}},
		Unattributed: map[string]string{"received": "0.000000", "sent": "0.000000"},
		Params: paramsOut{DataAsOf: "2026-10-23T00:00:00Z", View: "direct", Complete: true, MinAmountUSD: 100,
			USDConversion: "constant: 1 USDT = 1 USD", FlagsStatus: map[string]string{"frozen": "not_evaluated"}},
		Flags: []flagOut{},
	}
	md := markdownReport(rep)
	if !strings.Contains(md, "- none: the score is the direct floor") {
		t.Errorf("Drivers block does not attribute the score to the direct floor:\n%s", md)
	}
	if strings.Contains(md, "no flow was scored") {
		t.Errorf("a direct-floor score must not say 'no flow was scored':\n%s", md)
	}
	if !strings.Contains(md, "Unattributed USDT: received 0.000000, sent 0.000000.") {
		t.Errorf("the unattributed line should be kept when a direct floor set the score:\n%s", md)
	}
}
