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
	"math"
	"testing"
)

func testLabelMethod(t *testing.T) *labelMethod {
	m, err := compileLabelMethod(
		[]SourceClassRule{
			{Match: `^ofac$`, Class: "sanctions-list"},
			{Match: `heuristic`, Class: "heuristic"},
			{Match: ``, Class: "vendor"},
		},
		nil, // default class rank
		[]string{"spam"},
	)
	if err != nil {
		t.Fatalf("compileLabelMethod: %v", err)
	}
	return m
}

func TestPickClassRank(t *testing.T) {
	m := testLabelMethod(t)
	// A sanctions-list source outranks a vendor one, whatever the type names.
	lt, _, types := m.pick([]labelRow{
		{LabelType: "exchange", Value: "binance", Source: "some-vendor"},
		{LabelType: "sanctioned-entity", Value: "ofac-123", Source: "ofac"},
	})
	if lt != "sanctioned-entity" {
		t.Fatalf("pick = %q, want sanctioned-entity (sanctions-list beats vendor)", lt)
	}
	if len(types) != 2 {
		t.Fatalf("types = %v, want both", types)
	}
	// neverPick type is never the primary, but still listed.
	lt, _, types = m.pick([]labelRow{
		{LabelType: "spam", Value: "x", Source: "some-vendor"},
		{LabelType: "exchange", Value: "kraken", Source: "some-vendor"},
	})
	if lt != "exchange" {
		t.Fatalf("pick = %q, want exchange (spam is never-pick)", lt)
	}
	if len(types) != 2 {
		t.Fatalf("types = %v, want spam listed too", types)
	}
	// All never-pick -> no primary.
	if lt, _, _ := m.pick([]labelRow{{LabelType: "spam", Source: "v"}}); lt != "" {
		t.Fatalf("pick = %q, want empty", lt)
	}
	// No labels -> no primary.
	if lt, _, _ := m.pick(nil); lt != "" {
		t.Fatalf("pick(nil) = %q, want empty", lt)
	}
}

func TestShapeLabelTypeShares(t *testing.T) {
	tool := Tool{lm: testLabelMethod(t)}
	doc := map[string]any{
		"data": []any{
			map[string]any{"Counterparty": "0xAAA", "Usd_Total": 100.0},
			map[string]any{"Counterparty": "0xBBB", "Usd_Total": 50.0},
		},
		"totals": map[string]any{"Usd_Total": 1000.0},
	}
	byAddr := map[string][]labelRow{
		"0xaaa": {{LabelType: "exchange", Value: "kraken", Source: "some-vendor"}},
		// 0xbbb has no labels -> (none)
	}
	res := tool.shapeLabelType(request{chain: "ethereum"}, doc, byAddr)
	if res.ShareBaseUsd != 1000 {
		t.Fatalf("base = %v, want 1000", res.ShareBaseUsd)
	}
	got := map[string]float64{}
	var sum float64
	for _, ty := range res.Types {
		got[ty.LabelType] = ty.Usd
		sum += ty.Share
	}
	if got["exchange"] != 100 || got[noneLabel] != 50 || got[belowCutoffLabel] != 850 {
		t.Fatalf("usd by type wrong: %+v", got)
	}
	if math.Abs(sum-1.0) > 1e-9 {
		t.Fatalf("shares sum to %v, want 1.0", sum)
	}
}
