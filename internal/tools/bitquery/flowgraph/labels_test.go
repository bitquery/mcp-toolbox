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
		[]string{"high-transacting", "signer", "deployer", "spam"}, // never-pick
		[]string{ // priority (a subset in BIT-16068 section-3 order)
			"sanctioned-entity", "bridge",
			"dex-router", "asset-manager", "contract",
		},
		nil, // other-order empty -> alphabetical fallback
	)
	if err != nil {
		t.Fatalf("compileLabelMethod: %v", err)
	}
	return m
}

func TestPick(t *testing.T) {
	m := testLabelMethod(t)

	// CoW-like set: dex-router alongside several never-pick/attribute types and the
	// lower-priority asset-manager and contract types. The pick is dex-router: the
	// never-pick types are dropped and dex-router outranks asset-manager/contract in
	// the priority list. (This is AT3: Label_Type is dex-router, never
	// high-transacting/signer/deployer.)
	lt, _, types := m.pick([]labelRow{
		{LabelType: "high-transacting", Value: "x", Source: "some-vendor"},
		{LabelType: "signer", Value: "y", Source: "some-vendor"},
		{LabelType: "deployer", Value: "z", Source: "some-vendor"},
		{LabelType: "asset-manager", Value: "wintermute", Source: "some-vendor"},
		{LabelType: "dex-router", Value: "cowswap", Source: "some-vendor"},
		{LabelType: "contract", Value: "settlement", Source: "some-vendor"},
	})
	if lt != "dex-router" {
		t.Fatalf("pick = %q, want dex-router (priority beats asset-manager/contract; never-pick dropped)", lt)
	}
	if len(types) != 6 {
		t.Fatalf("types = %v, want all 6 listed (including never-pick types)", types)
	}

	// Risk first: a risk type outranks a DeFi type whatever the source.
	if lt, _, _ := m.pick([]labelRow{
		{LabelType: "dex-router", Value: "d", Source: "some-vendor"},
		{LabelType: "sanctioned-entity", Value: "ofac-123", Source: "ofac"},
	}); lt != "sanctioned-entity" {
		t.Fatalf("pick = %q, want sanctioned-entity (risk first in priority)", lt)
	}

	// Within one chosen type, the more-trusted source class wins the value.
	if _, v, _ := m.pick([]labelRow{
		{LabelType: "bridge", Value: "vendor-name", Source: "some-vendor"},
		{LabelType: "bridge", Value: "ofac-name", Source: "ofac"},
	}); v != "ofac-name" {
		t.Fatalf("within-type value = %q, want ofac-name (sanctions-list class beats vendor)", v)
	}

	// Only never-pick / attribute-only types: no primary, but they still appear in types.
	lt, _, types = m.pick([]labelRow{
		{LabelType: "high-transacting", Value: "x", Source: "v"},
		{LabelType: "signer", Value: "y", Source: "v"},
	})
	if lt != "" {
		t.Fatalf("pick = %q, want empty (only never-pick types -> unlabeled)", lt)
	}
	if len(types) != 2 {
		t.Fatalf("types = %v, want both never-pick types listed", types)
	}

	// No labels: no primary.
	if lt, _, _ := m.pick(nil); lt != "" {
		t.Fatalf("pick(nil) = %q, want empty", lt)
	}

	// A type in neither priority nor other-order ranks after every priority type.
	if lt, _, _ := m.pick([]labelRow{
		{LabelType: "zebra-type", Value: "z", Source: "v"},
		{LabelType: "contract", Value: "c", Source: "v"},
	}); lt != "contract" {
		t.Fatalf("pick = %q, want contract (a priority type beats an unknown type)", lt)
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
