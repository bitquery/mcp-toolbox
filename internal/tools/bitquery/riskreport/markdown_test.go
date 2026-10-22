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
	if !strings.Contains(md, "poisoning: 1053 look-alike(s)") {
		t.Errorf("markdown missing the poisoning headline:\n%s", md)
	}
}
