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
	"bufio"
	"math"
	"os"
	"slices"
	"strings"
	"testing"

	yaml "github.com/goccy/go-yaml"
)

// testdata/method.yaml is the draft method block (profile, labelMap,
// attributeOnly, entityKey, labelSources) plus thresholds; label_types.txt is
// the measured label vocabulary; label_values.tsv is what the offline check of
// the draft makes of every measured value.

func loadMethodConfig(t *testing.T) MethodConfig {
	t.Helper()
	data, err := os.ReadFile("testdata/method.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var mc MethodConfig
	if err := yaml.UnmarshalWithOptions(data, &mc, yaml.Strict()); err != nil {
		t.Fatalf("method.yaml: %v", err)
	}
	return mc
}

func loadMethod(t *testing.T) *method {
	t.Helper()
	m, err := compileMethod(loadMethodConfig(t))
	if err != nil {
		t.Fatalf("compileMethod: %v", err)
	}
	return m
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := sc.Text(); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

func TestMethodCompiles(t *testing.T) {
	m := loadMethod(t)
	if got := len(m.order); got != 26 {
		t.Errorf("profile types = %d, want 26", got)
	}
	if got := len(m.rules); got != 41 {
		t.Errorf("labelMap rules = %d, want 41", got)
	}
	if got := len(m.attributeOnly); got != 22 {
		t.Errorf("attributeOnly = %d, want 22", got)
	}
	if m.order[0] != "Terrorism Financing" || m.order[1] != "Sanctions" {
		t.Errorf("profile order lost: %v", m.order[:2])
	}
	if m.weightUnattributed != 45 {
		t.Errorf("weight_unattributed = %v", m.weightUnattributed)
	}
}

// Every measured label type is disposed of exactly once: by one rule with no
// value and no chain condition, or by attributeOnly.
func TestEveryLabelTypeDisposedOnce(t *testing.T) {
	mc := loadMethodConfig(t)
	vocab := readLines(t, "testdata/label_types.txt")
	if len(vocab) != 85 {
		t.Fatalf("vocabulary = %d types, want 85", len(vocab))
	}
	for _, typ := range vocab {
		n := 0
		for _, r := range mc.LabelMap {
			if slices.Contains(r.LabelType, typ) && r.Value == "" && r.Chain == "" {
				n++
			}
		}
		if slices.Contains(mc.AttributeOnly, typ) {
			n++
		}
		if n != 1 {
			t.Errorf("%s: disposed of %d times", typ, n)
		}
	}
	for _, r := range mc.LabelMap {
		for _, typ := range r.LabelType {
			if !slices.Contains(vocab, typ) {
				t.Errorf("rule names a label type outside the vocabulary: %s", typ)
			}
		}
	}
}

func TestLabelValuesGolden(t *testing.T) {
	m := loadMethod(t)
	lines := readLines(t, "testdata/label_values.tsv")
	if len(lines) < 200 {
		t.Fatalf("only %d golden values", len(lines))
	}
	for _, l := range lines {
		f := strings.Split(l, "\t")
		if len(f) != 5 {
			t.Fatalf("bad golden line %q", l)
		}
		typ, chain, value, wantDisp, wantEntity := f[0], f[1], f[2], f[3], f[4]
		var disp string
		a := m.classify(chain, []labelRow{{LabelType: typ, Value: value}})
		switch {
		case len(a.Attributes) == 1:
			disp = "attribute-only"
		case len(a.Unmapped) == 1:
			disp = "UNMAPPED"
		case len(a.Evidence) == 1:
			disp = "evidence-only"
		default:
			disp = a.Types[0].Type
			if a.Types[0].Subtype != "" {
				disp += " / " + a.Types[0].Subtype
			}
		}
		if disp != wantDisp {
			t.Errorf("%s %s %s: disposition %q, want %q", typ, chain, value, disp, wantDisp)
		}
		entity := m.entityKey(typ, value)
		if entity == "" {
			entity = "(unnamed)"
		}
		if entity != wantEntity {
			t.Errorf("%s %s %s: entity %q, want %q", typ, chain, value, entity, wantEntity)
		}
	}
}

func TestAttributionPicksHighestWeightTimesLevel(t *testing.T) {
	m := loadMethod(t)
	w := m.defaultWeights()
	// An address under Scam and Gambling: Scam on the received side
	// (100 x High = 100 against 75), Gambling on the sent side (75 x High = 75
	// against 100 x Low = 30).
	a := m.classify("tron", []labelRow{
		{LabelType: "scam", Value: "whale-io-deposit", Source: "workflow:tron-cex-deposit-sweep-heuristic-v1"},
		{LabelType: "gambling", Value: "whale-io", Source: "verified-metasleuth"},
	})
	r, ok := m.attribute(a, w, received)
	if !ok || r.Label.Type != "Scam" || r.Entity != "whale-io" || !slices.Equal(r.OtherTypes, []string{"Gambling"}) {
		t.Errorf("received: %+v", r)
	}
	s, ok := m.attribute(a, w, sent)
	if !ok || s.Label.Type != "Gambling" {
		t.Errorf("sent: %+v", s)
	}
	// A tie on weight x level goes to the type listed first in the profile.
	b := m.classify("tron", []labelRow{
		{LabelType: "sanctioned-entity", Value: "sanctioned-islamic-revolutionary-guard-corps", Source: "ofac"},
		{LabelType: "terrorist", Value: "terrorist-islamic-revolutionary-guard-corps", Source: "vendor-x"},
	})
	if r, _ := m.attribute(b, w, received); r.Label.Type != "Terrorism Financing" {
		t.Errorf("tie-break: %+v", r)
	}
	// Attribute-only rows never make an entity; an unknown type is evidence and counted.
	c := m.classify("tron", []labelRow{{LabelType: "first-funder", Value: "x"}, {LabelType: "brand-new-type", Value: "y"}})
	if _, ok := m.attribute(c, w, received); ok || !slices.Equal(c.Unmapped, []string{"brand-new-type"}) {
		t.Errorf("attribute-only / unmapped: %+v", c)
	}
}

func TestCompileMethodRefusesBadBlocks(t *testing.T) {
	good := loadMethodConfig(t)
	cases := map[string]func(mc *MethodConfig){
		"type not in profile": func(mc *MethodConfig) {
			mc.LabelMap = append(mc.LabelMap, LabelRule{LabelType: StringList{"x"}, Type: "Nope"})
		},
		"type and evidence": func(mc *MethodConfig) {
			mc.LabelMap = append(mc.LabelMap, LabelRule{LabelType: StringList{"x"}, Type: "Other", EvidenceOnly: true})
		},
		"bad regexp":           func(mc *MethodConfig) { mc.LabelMap = append(mc.LabelMap, LabelRule{Value: "(", Type: "Other"}) },
		"no weight_unattrib.":  func(mc *MethodConfig) { mc.WeightUnattributed = nil },
		"attribute and mapped": func(mc *MethodConfig) { mc.AttributeOnly = append(mc.AttributeOnly, "scam") },
		"bad asset price":      func(mc *MethodConfig) { mc.AssetUSD = map[string]map[string]float64{"tron": {"X": -1}} },
		// N21: a NaN weight would make every score NaN and fail every report
		// at the JSON layer; max_hops is the default of a call without one.
		"NaN weight_unattrib.": func(mc *MethodConfig) { nan := math.NaN(); mc.WeightUnattributed = &nan },
		"max_hops over cap":    func(mc *MethodConfig) { mc.Thresholds.MaxHops = maxHopsCap + 3 },
	}
	for name, mutate := range cases {
		mc := loadMethodConfig(t)
		_ = good
		mutate(&mc)
		if _, err := compileMethod(mc); err == nil {
			t.Errorf("%s: compiled", name)
		}
	}
	mc := loadMethodConfig(t)
	mc.Thresholds.MaxHops = maxHopsCap
	if m, err := compileMethod(mc); err != nil || m.th.MaxHops != maxHopsCap {
		t.Errorf("max_hops at the cap: %v", err)
	}
	m := loadMethod(t)
	for _, bad := range []map[string]any{
		{"weight_unattributed": math.NaN()},
		{"Gambling": map[string]any{"weight": math.NaN()}},
	} {
		if _, err := m.mergeWeights(bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

// AT11: the formula over the reference report's own shares for the test
// wallet gives 43.4 with weight_unattributed 45 and 2.4 with 0. Amounts per
// entity type, as derived from the reference export; "Settled" is a type the
// profile does not hold and weighs 0.
func TestScoreReferenceShares(t *testing.T) {
	m := loadMethod(t)
	recv := dirAmounts{byType: map[string]float64{
		"Gambling": 14779.531484, "Exchange Unlicensed": 334698.83118, "P2P Exchange Unlicensed": 8188.927478,
		"Exchange Licensed": 291208.031279, "Payment Processor": 9294.544479, "Sanctions": 3722.141705,
		"Online Wallet": 358.15737, "Other": 29845.283362, "Darknet Marketplace": 12.94902, "Stolen Coins": 1512.067517,
		"Illegal Service": 44526.723598, "Scam": 807.077149,
	}, unattributed: 7580045.734379}
	snt := dirAmounts{byType: map[string]float64{
		"Gambling": 1285.760486, "Exchange Unlicensed": 630555.633902, "P2P Exchange Unlicensed": 15.868909,
		"Exchange Licensed": 27353.36248, "Sanctions": 2246.053471, "Other": 11007.385866, "Stolen Coins": 138.684695,
		"Illegal Service": 6047.561152, "Settled": 9544.248515,
	}, unattributed: 7630905.095918}
	for _, tc := range []struct {
		wu          float64
		r, s, score float64
	}{{45, 43.36, 42.75, 43.4}, {0, 2.36, 1.48, 2.4}} {
		w, err := m.mergeWeights(map[string]any{"weight_unattributed": tc.wu})
		if err != nil {
			t.Fatal(err)
		}
		r, _ := w.dirScore(recv, received)
		s, _ := w.dirScore(snt, sent)
		if round(r, 2) != tc.r || round(s, 2) != tc.s || round(math.Max(r, s), 1) != tc.score {
			t.Errorf("wu=%v: received %.4f sent %.4f, want %v %v %v", tc.wu, r, s, tc.r, tc.s, tc.score)
		}
	}
}

func TestMergeWeights(t *testing.T) {
	m := loadMethod(t)
	w, err := m.mergeWeights(map[string]any{"Gambling": map[string]any{"weight": 50.0}, "Scam": map[string]any{"sending": "High"}})
	if err != nil {
		t.Fatal(err)
	}
	if g := w.types["Gambling"]; g.Weight != 50 || g.Receiving != "High" {
		t.Errorf("partial override lost a field: %+v", g)
	}
	if s := w.types["Scam"]; s.Weight != 100 || s.Sending != "High" {
		t.Errorf("Scam: %+v", s)
	}
	if m.profile["Gambling"].Weight != 75 {
		t.Errorf("override leaked into the configured profile")
	}
	for _, bad := range []map[string]any{
		{"Nope": map[string]any{"weight": 1.0}},
		{"Gambling": map[string]any{"weight": 101.0}},
		{"Gambling": map[string]any{"receiving": "Extreme"}},
		{"Gambling": 3.0},
		{"weight_unattributed": -1.0},
	} {
		if _, err := m.mergeWeights(bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	// No exposure at all: every direction scores weight_unattributed.
	d := m.defaultWeights()
	if s, ok := d.dirScore(dirAmounts{unattributed: 123}, received); !ok || s != 45 {
		t.Errorf("all unattributed: %v %v", s, ok)
	}
}
