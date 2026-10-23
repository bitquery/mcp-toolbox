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

// A low-trust community-class label (source bitcoinabuse) must not drive the
// score over a higher-trust identity: it is excluded from score-driving
// selection when a higher-class row is present, it never sets the direct floor,
// and it never fires the dust flag — while it is still surfaced on the
// connection (community_types / type_conflict). These tests encode the
// acceptance of that fix. The one community mapping in testdata is bitcoinabuse.

import (
	"context"
	"testing"
)

// communityStrings reads a decoded JSON string list.
func communityStrings(v any) []string {
	var out []string
	if a, ok := v.([]any); ok {
		for _, x := range a {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// connByType finds a decoded connection by its entity_type.
func connByType(out map[string]any, typ string) map[string]any {
	for _, c := range out["connections"].([]any) {
		cm := c.(map[string]any)
		if cm["entity_type"] == typ {
			return cm
		}
	}
	return nil
}

// TestCommunityEligibilityUnit exercises the method functions directly: a
// community-class row is dropped from score-driving when a higher-class row is
// present (and surfaced as a conflict), is kept when it is the only signal, and
// never counts toward the dust weight.
func TestCommunityEligibilityUnit(t *testing.T) {
	m := loadMethod(t)
	w := m.defaultWeights()

	// An exchange (vendor) that also carries community ransomware + scam.
	mixed := m.classify("tron", []labelRow{
		{LabelType: "cex-hot-wallet", Value: "binance-hot", Source: "vendor-x"},
		{LabelType: "ransomware", Value: "ransomware-x", Source: "bitcoinabuse"},
		{LabelType: "scam", Value: "scam-somebody", Source: "bitcoinabuse"},
	})
	elig, excl := m.eligibleTypes(mixed.Types)
	if len(elig) != 1 || elig[0].Type != licenceUnknownType {
		t.Fatalf("eligible types = %v, want only %q", elig, licenceUnknownType)
	}
	if len(excl) != 2 {
		t.Fatalf("excluded community rows = %v, want Ransom and Scam", excl)
	}
	at, ok := m.attribute(mixed, w, received)
	if !ok || at.Label.Type != licenceUnknownType {
		t.Fatalf("attribute chose %q, want %q", at.Label.Type, licenceUnknownType)
	}
	if !at.TypeConflict {
		t.Errorf("type_conflict not set on a mixed address")
	}
	if got := at.CommunityTypes; len(got) != 2 || got[0] != "Ransom" || got[1] != "Scam" {
		t.Errorf("community_types = %v, want [Ransom Scam]", got)
	}
	if m.attributionIsCommunity(at) {
		t.Errorf("a vendor-attributed row reported as community")
	}
	if mw := m.maxWeight(mixed, w); mw != 60 {
		t.Errorf("maxWeight = %v, want 60 (the exchange; community excluded)", mw)
	}

	// A community-only address is still attributed, but its row is community.
	only := m.classify("tron", []labelRow{{LabelType: "ransomware", Value: "ransomware-x", Source: "bitcoinabuse"}})
	elig, excl = m.eligibleTypes(only.Types)
	if len(elig) != 1 || len(excl) != 0 {
		t.Fatalf("community-only eligible=%v excluded=%v, want the row kept", elig, excl)
	}
	at, ok = m.attribute(only, w, received)
	if !ok || at.Label.Type != "Ransom" {
		t.Fatalf("community-only attribute = %q, want Ransom", at.Label.Type)
	}
	if len(at.CommunityTypes) != 0 || at.TypeConflict {
		t.Errorf("community-only must not report a conflict: %+v", at)
	}
	if !m.attributionIsCommunity(at) {
		t.Errorf("a community-only attribution not reported as community")
	}
	if mw := m.maxWeight(only, w); mw != 0 {
		t.Errorf("maxWeight (community only) = %v, want 0 (dust fires only on a non-community row)", mw)
	}
}

// TestCommunityClassNotScored: a received counterparty labelled an exchange
// (vendor) that also carries community ransomware + scam attributes as the
// exchange; the community labels are surfaced but set no floor (the floor is the
// exchange's, not 100) and do not win the score.
func TestCommunityClassNotScored(t *testing.T) {
	cp := at5Xinbi
	chain := &fakeChain{
		assets: []map[string]any{usdtAssetRow("100000000000", "0", 1, 0)},
		edges: []map[string]any{
			edgeRow("in", cp, "100000000000", "100000000000", "100000000000", 1, [4]string{"1", "1", "100000000000", "100000000000"}, "aa"),
		},
	}
	labels := &fakeLabels{rows: map[string][]map[string]any{
		cp: {
			{"label_type": "cex-hot-wallet", "label_value": "binance-hot", "source": "vendor-x", "recorded_at": "2026-01-01 00:00:00"},
			{"label_type": "ransomware", "label_value": "ransomware-x", "source": "bitcoinabuse", "recorded_at": "2026-01-01 00:00:00"},
			{"label_type": "scam", "label_value": "scam-somebody", "source": "bitcoinabuse", "recorded_at": "2026-01-01 00:00:00"},
		},
	}}
	tool := newTestTool(t, chain, labels)
	res, err := tool.Invoke(context.Background(), chain, mustParams(t, tool, map[string]any{"chain": "tron", "address": at5Target}), "")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := decode(t, res)
	conn := connByType(out, licenceUnknownType)
	if conn == nil {
		t.Fatalf("no Exchange connection: %v", out["connections"])
	}
	if conn["entity"] != "binance" {
		t.Errorf("entity = %v, want binance", conn["entity"])
	}
	if conn["type_conflict"] != true {
		t.Errorf("type_conflict not set: %v", conn)
	}
	if got := communityStrings(conn["community_types"]); len(got) != 2 || got[0] != "Ransom" || got[1] != "Scam" {
		t.Errorf("community_types = %v, want [Ransom Scam]", got)
	}
	score := out["score"].(map[string]any)
	if score["direct_floor"].(float64) == 100 {
		t.Errorf("direct_floor = 100: a community row set the floor")
	}
	if score["direct_floor"].(float64) != 36 {
		t.Errorf("direct_floor = %v, want 36 (the exchange's weight x level)", score["direct_floor"])
	}
	if ff, _ := score["direct_floor_from"].(map[string]any); ff == nil || ff["entity_type"] != licenceUnknownType {
		t.Errorf("direct_floor_from = %v, want the exchange", score["direct_floor_from"])
	}
	if score["value"].(float64) != 36 {
		t.Errorf("score.value = %v, want 36 (scored as the exchange, not the community Ransom)", score["value"])
	}
}

// TestCommunityOnlyNoFloor: an address whose only labels are community-class
// still forms a connection, but the direct floor stays 0 — a community row never
// sets it, even when it is the only signal.
func TestCommunityOnlyNoFloor(t *testing.T) {
	cp := at5Xinbi
	chain := &fakeChain{
		assets: []map[string]any{usdtAssetRow("100000000000", "0", 1, 0)},
		edges: []map[string]any{
			edgeRow("in", cp, "100000000000", "100000000000", "100000000000", 1, [4]string{"1", "1", "100000000000", "100000000000"}, "aa"),
		},
	}
	labels := &fakeLabels{rows: map[string][]map[string]any{
		cp: {{"label_type": "ransomware", "label_value": "ransomware-x", "source": "bitcoinabuse", "recorded_at": "2026-01-01 00:00:00"}},
	}}
	tool := newTestTool(t, chain, labels)
	res, err := tool.Invoke(context.Background(), chain, mustParams(t, tool, map[string]any{"chain": "tron", "address": at5Target}), "")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := decode(t, res)
	if connByType(out, "Ransom") == nil {
		t.Fatalf("no Ransom connection for a community-only address: %v", out["connections"])
	}
	score := out["score"].(map[string]any)
	if score["direct_floor"].(float64) != 0 {
		t.Errorf("direct_floor = %v, want 0 (community never floors)", score["direct_floor"])
	}
	if score["direct_floor_from"] != nil {
		t.Errorf("direct_floor_from = %v, want none", score["direct_floor_from"])
	}
	// Community-only still drives the direction score via its share (it is the
	// only signal and eligibility keeps it); only the floor is withheld. So the
	// received score and the value are the Ransom weight x level, 100 — the score
	// is a direction share, not a floor.
	if rs, _ := score["received_score"].(float64); rs != 100 {
		t.Errorf("received_score = %v, want 100 (community-only drives the direction share)", score["received_score"])
	}
	if score["value"].(float64) != 100 {
		t.Errorf("score.value = %v, want 100 (direction share, not the withheld floor)", score["value"])
	}
}

// TestCommunityDustNotFired: a below-dust exchange counterparty that also
// carries a community scam does NOT fire risky_dust, while a below-dust vendor
// ransomware counterparty still does — the flag fires only on a non-community
// row.
func TestCommunityDustNotFired(t *testing.T) {
	exch := at5Xinbi      // exchange (vendor) + community scam: must not fire
	ransom := plainSender // vendor ransomware: must still fire
	chain := &fakeChain{
		assets: []map[string]any{usdtAssetRow("2000000", "0", 2, 0)},
		edges: []map[string]any{
			edgeRow("in", exch, "1000000", "0", "1000000", 1, [4]string{"2", "2", "2000000", "0"}, "aa"),
			edgeRow("in", ransom, "1000000", "0", "1000000", 1, [4]string{"2", "2", "2000000", "0"}, "bb"),
		},
	}
	labels := &fakeLabels{rows: map[string][]map[string]any{
		exch: {
			{"label_type": "cex-hot-wallet", "label_value": "changenow-hot", "source": "vendor-x", "recorded_at": "2026-01-01 00:00:00"},
			{"label_type": "scam", "label_value": "scam-somebody", "source": "bitcoinabuse", "recorded_at": "2026-01-01 00:00:00"},
		},
		ransom: {{"label_type": "ransomware", "label_value": "ransomware-x", "source": "vendor-x", "recorded_at": "2026-01-01 00:00:00"}},
	}}
	tool := newTestTool(t, chain, labels)
	res, err := tool.Invoke(context.Background(), chain, mustParams(t, tool, map[string]any{"chain": "tron", "address": at5Target}), "")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := decode(t, res)
	var dustOn []string
	for _, f := range out["flags"].([]any) {
		fm := f.(map[string]any)
		if fm["type"] == "risky_dust" {
			dustOn = append(dustOn, fm["detail"].(map[string]any)["counterparty"].(string))
		}
	}
	if len(dustOn) != 1 || dustOn[0] != ransom {
		t.Errorf("risky_dust fired on %v, want only the vendor ransomware address %s", dustOn, ransom)
	}
}

// TestCommunitySameEntitySurfacesConflict: a counterparty that is the target's
// own entity (an exchange) and also carries community ransomware is excluded as
// same_entity, sets no floor, and still surfaces the community conflict.
func TestCommunitySameEntitySurfacesConflict(t *testing.T) {
	cp := at5Xinbi
	chain := &fakeChain{
		assets: []map[string]any{usdtAssetRow("100000000000", "0", 1, 0)},
		edges: []map[string]any{
			edgeRow("in", cp, "100000000000", "100000000000", "100000000000", 1, [4]string{"1", "1", "100000000000", "100000000000"}, "aa"),
		},
	}
	labels := &fakeLabels{rows: map[string][]map[string]any{
		at5Target: {{"label_type": "cex-hot-wallet", "label_value": "binance-hot", "source": "vendor-x", "recorded_at": "2026-01-01 00:00:00"}},
		cp: {
			{"label_type": "cex-hot-wallet", "label_value": "binance-hot", "source": "vendor-x", "recorded_at": "2026-01-01 00:00:00"},
			{"label_type": "ransomware", "label_value": "ransomware-x", "source": "bitcoinabuse", "recorded_at": "2026-01-01 00:00:00"},
		},
	}}
	tool := newTestTool(t, chain, labels)
	res, err := tool.Invoke(context.Background(), chain, mustParams(t, tool, map[string]any{"chain": "tron", "address": at5Target}), "")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := decode(t, res)
	if owner := out["summary"].(map[string]any)["labels"].(map[string]any)["owner"]; owner != "binance" {
		t.Fatalf("owner = %v, want binance", owner)
	}
	conn := connByType(out, licenceUnknownType)
	if conn == nil {
		t.Fatalf("no exchange connection: %v", out["connections"])
	}
	if conn["same_entity"] != true {
		t.Errorf("connection not same_entity: %v", conn)
	}
	if conn["type_conflict"] != true {
		t.Errorf("type_conflict not set: %v", conn)
	}
	if got := communityStrings(conn["community_types"]); len(got) != 1 || got[0] != "Ransom" {
		t.Errorf("community_types = %v, want [Ransom]", got)
	}
	if score := out["score"].(map[string]any); score["direct_floor"].(float64) != 0 {
		t.Errorf("direct_floor = %v, want 0 (same entity and community)", score["direct_floor"])
	}
}

// TestCommunityTronBaselineEquivalence: adding a synthetic community scam row to
// a vendor-exchange counterparty leaves the score, the direction scores and the
// direct floor exactly as the exchange row alone yields — the community row is
// ignored. This is the proof that the rule changes no real Tron score.
func TestCommunityTronBaselineEquivalence(t *testing.T) {
	cp := at5Xinbi
	run := func(withCommunity bool) map[string]any {
		rows := []map[string]any{
			{"label_type": "cex-hot-wallet", "label_value": "binance-hot", "source": "vendor-x", "recorded_at": "2026-01-01 00:00:00"},
		}
		if withCommunity {
			rows = append(rows, map[string]any{"label_type": "scam", "label_value": "scam-somebody", "source": "bitcoinabuse", "recorded_at": "2026-01-01 00:00:00"})
		}
		chain := &fakeChain{
			assets: []map[string]any{usdtAssetRow("100000000000", "40000000000", 1, 1)},
			edges: []map[string]any{
				edgeRow("in", cp, "100000000000", "100000000000", "100000000000", 1, [4]string{"1", "1", "100000000000", "100000000000"}, "aa"),
				edgeRow("out", cp, "40000000000", "40000000000", "40000000000", 1, [4]string{"1", "1", "40000000000", "40000000000"}, "bb"),
			},
		}
		labels := &fakeLabels{rows: map[string][]map[string]any{cp: rows}}
		tool := newTestTool(t, chain, labels)
		res, err := tool.Invoke(context.Background(), chain, mustParams(t, tool, map[string]any{"chain": "tron", "address": at5Target}), "")
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		return decode(t, res)
	}
	base := run(false)["score"].(map[string]any)
	with := run(true)
	withScore := with["score"].(map[string]any)
	for _, k := range []string{"value", "received_score", "sent_score", "direct_floor"} {
		if !sameScoreField(base[k], withScore[k]) {
			t.Errorf("%s changed: baseline %v, with community %v", k, base[k], withScore[k])
		}
	}
	// The community row was actually present (landed in Types) and ignored.
	conn := connByType(with, licenceUnknownType)
	if conn == nil || conn["type_conflict"] != true {
		t.Fatalf("the community row did not land as a surfaced conflict: %v", with["connections"])
	}
	if got := communityStrings(conn["community_types"]); len(got) != 1 || got[0] != "Scam" {
		t.Errorf("community_types = %v, want [Scam]", got)
	}
}

// sameScoreField compares two score fields, each a float64 or a JSON null.
func sameScoreField(a, b any) bool {
	af, aok := a.(float64)
	bf, bok := b.(float64)
	if aok != bok {
		return false
	}
	return af == bf
}
