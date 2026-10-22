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

// The method block of the tool's configuration: the risk profile, the label
// map, how label values become entities, how label sources are classed, and the
// thresholds. Everything that defines the published method lives here, so a
// recalibration is a configuration change, not a new image.

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
)

// StringList accepts a single string or a list of strings.
type StringList []string

func (s *StringList) UnmarshalYAML(unmarshal func(any) error) error {
	var one string
	if err := unmarshal(&one); err == nil {
		*s = StringList{one}
		return nil
	}
	var many []string
	if err := unmarshal(&many); err != nil {
		return fmt.Errorf("expected a string or a list of strings")
	}
	*s = many
	return nil
}

// ProfileEntry is one entity type of the risk profile. The profile is a list:
// its order is the tie-break order, and a mapping would lose it on the way
// through the server's configuration decoder.
type ProfileEntry struct {
	Type      string  `yaml:"type"`
	Weight    float64 `yaml:"weight"`
	Receiving string  `yaml:"receiving"`
	Sending   string  `yaml:"sending"`
}

// LabelRule is one row of labelMap. A rule matches on label_type (omitted = any
// type), and optionally on chain and on value (an RE2 pattern, unanchored unless
// written with ^ / $). It names an entity type (with an optional subtype) or says
// evidence_only.
type LabelRule struct {
	LabelType    StringList `yaml:"label_type"`
	Chain        string     `yaml:"chain"`
	Value        string     `yaml:"value"`
	Type         string     `yaml:"type"`
	Subtype      string     `yaml:"subtype"`
	EvidenceOnly bool       `yaml:"evidence_only"`
}

// AliasRule names the entity of a label value that matches it ("" = no entity).
type AliasRule struct {
	Match  string `yaml:"match"`
	Entity string `yaml:"entity"`
}

// EntityKeyConfig turns a label value into the entity a connection row names.
type EntityKeyConfig struct {
	StripPrefix []string    `yaml:"stripPrefix"`
	StripSuffix []string    `yaml:"stripSuffix"`
	Alias       []AliasRule `yaml:"alias"`
}

// SourceClassRule classes a label source; the first match wins.
type SourceClassRule struct {
	Match string `yaml:"match"`
	Class string `yaml:"class"`
}

// Thresholds are the tunable numbers of the method. A zero value takes the
// default in defaultThresholds.
type Thresholds struct {
	DustUSD                    float64  `yaml:"dust_usd"`
	LookalikeAmountCapUSD      float64  `yaml:"lookalike_amount_cap_usd"`
	MinAmountUSD               float64  `yaml:"min_amount_usd"`
	NativeDustUnits            int64    `yaml:"native_dust_units"`
	LookalikeMinSuffix         int      `yaml:"lookalike_min_suffix"`
	PassThroughDays            int      `yaml:"pass_through_days"`
	PassThroughOutShare        float64  `yaml:"pass_through_out_share"`
	PassThroughEndBalanceShare float64  `yaml:"pass_through_end_balance_share"`
	OtherAssetRiskMin          float64  `yaml:"other_asset_risk_min"`
	DirectSanctionsTypes       []string `yaml:"direct_sanctions_types"`
	CounterpartyLimit          int      `yaml:"counterparty_limit"`
	UnnamedLimit               int      `yaml:"unnamed_limit"`
	LabelBatch                 int      `yaml:"label_batch"`
	StatementMaxRows           int64    `yaml:"statement_max_rows"`
	StatementMaxSeconds        int      `yaml:"statement_max_seconds"`
	ReportRowsBudget           int64    `yaml:"report_rows_budget"`
	DeadlineSeconds            int      `yaml:"deadline_s"`
	// Read by the multi-hop search (view all).
	HubTransfers int     `yaml:"hub_transfers"`
	BranchMinUSD float64 `yaml:"branch_min_usd"`
	MaxHops      int     `yaml:"max_hops"`
	// GateRowsPerAddress is what one address costs to read at least, whatever
	// its transfers (a granule in every partition it touches): a history read's
	// row budget is this plus hub_transfers per address.
	GateRowsPerAddress int64 `yaml:"gate_rows_per_address"`
	// ExplainLimit is the most size estimates (one statement each) a report
	// sends; past it, the rest of a refused history read are hubs that were
	// not measured.
	ExplainLimit int `yaml:"explain_limit"`
}

var defaultThresholds = Thresholds{
	DustUSD:                    5,
	LookalikeAmountCapUSD:      100,
	MinAmountUSD:               100,
	NativeDustUnits:            10,
	LookalikeMinSuffix:         3,
	PassThroughDays:            10,
	PassThroughOutShare:        0.9,
	PassThroughEndBalanceShare: 0.01,
	OtherAssetRiskMin:          75,
	DirectSanctionsTypes:       []string{"Sanctions", "Terrorism Financing"},
	CounterpartyLimit:          5000,
	UnnamedLimit:               50,
	LabelBatch:                 2000,
	StatementMaxRows:           200_000_000,
	StatementMaxSeconds:        60,
	ReportRowsBudget:           500_000_000,
	DeadlineSeconds:            50,
	HubTransfers:               10000,
	BranchMinUSD:               1,
	MaxHops:                    2,
	// Measured per address and table: 0.15M rows (62 addresses, 9.08M) to
	// about 1.0M (one address read alone); 3M leaves room above both.
	GateRowsPerAddress: 3_000_000,
	ExplainLimit:       200,
}

// MethodConfig is the method block, inline in the tool's configuration.
type MethodConfig struct {
	Profile            []ProfileEntry     `yaml:"profile"`
	WeightUnattributed *float64           `yaml:"weight_unattributed"`
	Levels             map[string]float64 `yaml:"levels"`
	LabelMap           []LabelRule        `yaml:"labelMap"`
	AttributeOnly      []string           `yaml:"attributeOnly"`
	EntityKey          EntityKeyConfig    `yaml:"entityKey"`
	LabelSources       []SourceClassRule  `yaml:"labelSources"`
	// SourcePreference orders sources within one class: when two rows of one
	// entity type come from two lists, the row of the source listed first names
	// the entity. A fixed order, so a list refresh never renames an entity.
	SourcePreference []string                      `yaml:"sourcePreference"`
	Thresholds       Thresholds                    `yaml:"thresholds"`
	AssetUSD         map[string]map[string]float64 `yaml:"assetUsd"`
	CoverageTier     map[string]string             `yaml:"coverageTier"`
}

// typeWeight is one entity type of the profile.
type typeWeight struct {
	Weight    float64
	Receiving string
	Sending   string
}

type compiledRule struct {
	types        []string
	chain        string
	value        *regexp.Regexp
	typ          string
	subtype      string
	evidenceOnly bool
}

type compiledAlias struct {
	match  *regexp.Regexp
	entity string
}

type compiledSource struct {
	match *regexp.Regexp
	class string
}

// method is the compiled method block.
type method struct {
	order              []string
	profile            map[string]typeWeight
	levels             map[string]float64
	weightUnattributed float64
	rules              []compiledRule
	attributeOnly      map[string]bool
	stripPrefix        []*regexp.Regexp
	stripSuffix        []*regexp.Regexp
	alias              []compiledAlias
	sources            []compiledSource
	sourcePref         []string
	th                 Thresholds
	assetUSD           map[string]map[string]float64
	coverage           map[string]string
}

func compileMethod(mc MethodConfig) (*method, error) {
	m := &method{
		profile:       map[string]typeWeight{},
		levels:        mc.Levels,
		attributeOnly: map[string]bool{},
		assetUSD:      mc.AssetUSD,
		coverage:      mc.CoverageTier,
	}
	if len(m.levels) == 0 {
		return nil, fmt.Errorf("levels is required")
	}
	for name, f := range m.levels {
		if f < 0 || f > 1 || math.IsNaN(f) {
			return nil, fmt.Errorf("level %q must be between 0 and 1", name)
		}
	}
	if len(mc.Profile) == 0 {
		return nil, fmt.Errorf("profile is required")
	}
	for _, e := range mc.Profile {
		name := strings.TrimSpace(e.Type)
		if name == "" {
			return nil, fmt.Errorf("profile: every entity type needs a type name")
		}
		if _, dup := m.profile[name]; dup {
			return nil, fmt.Errorf("profile: entity type %q is listed twice", name)
		}
		tw := typeWeight{Weight: e.Weight, Receiving: e.Receiving, Sending: e.Sending}
		if err := m.checkTypeWeight(tw); err != nil {
			return nil, fmt.Errorf("profile %q: %w", name, err)
		}
		m.order = append(m.order, name)
		m.profile[name] = tw
	}
	if mc.WeightUnattributed == nil {
		return nil, fmt.Errorf("weight_unattributed is required")
	}
	if w := *mc.WeightUnattributed; w < 0 || w > 100 || math.IsNaN(w) {
		return nil, fmt.Errorf("weight_unattributed must be between 0 and 100")
	}
	m.weightUnattributed = *mc.WeightUnattributed

	for _, t := range mc.AttributeOnly {
		m.attributeOnly[t] = true
	}
	for i, r := range mc.LabelMap {
		if (r.Type != "") == r.EvidenceOnly {
			return nil, fmt.Errorf("labelMap rule %d: give exactly one of type and evidence_only", i+1)
		}
		if r.Type != "" {
			if _, ok := m.profile[r.Type]; !ok {
				return nil, fmt.Errorf("labelMap rule %d: type %q is not in the profile", i+1, r.Type)
			}
		}
		for _, t := range r.LabelType {
			if m.attributeOnly[t] {
				return nil, fmt.Errorf("labelMap rule %d: label type %q is also in attributeOnly", i+1, t)
			}
		}
		cr := compiledRule{types: r.LabelType, chain: r.Chain, typ: r.Type, subtype: r.Subtype, evidenceOnly: r.EvidenceOnly}
		if r.Value != "" {
			re, err := regexp.Compile(r.Value)
			if err != nil {
				return nil, fmt.Errorf("labelMap rule %d: value: %w", i+1, err)
			}
			cr.value = re
		}
		m.rules = append(m.rules, cr)
	}
	var err error
	if m.stripPrefix, err = compileAll("entityKey.stripPrefix", mc.EntityKey.StripPrefix); err != nil {
		return nil, err
	}
	if m.stripSuffix, err = compileAll("entityKey.stripSuffix", mc.EntityKey.StripSuffix); err != nil {
		return nil, err
	}
	for _, a := range mc.EntityKey.Alias {
		re, err := regexp.Compile(a.Match)
		if err != nil {
			return nil, fmt.Errorf("entityKey.alias %q: %w", a.Match, err)
		}
		m.alias = append(m.alias, compiledAlias{match: re, entity: a.Entity})
	}
	for _, s := range mc.LabelSources {
		re, err := regexp.Compile(s.Match)
		if err != nil {
			return nil, fmt.Errorf("labelSources %q: %w", s.Match, err)
		}
		m.sources = append(m.sources, compiledSource{match: re, class: s.Class})
	}
	m.sourcePref = mc.SourcePreference
	m.th = withDefaults(mc.Thresholds)
	// A call without max_hops takes this one, so it is held to the same cap
	// as the parameter.
	if m.th.MaxHops > maxHopsCap {
		return nil, fmt.Errorf("thresholds.max_hops must be from 1 to %d", maxHopsCap)
	}
	for _, t := range m.th.DirectSanctionsTypes {
		if _, ok := m.profile[t]; !ok {
			return nil, fmt.Errorf("thresholds.direct_sanctions_types: %q is not in the profile", t)
		}
	}
	for chain, assets := range m.assetUSD {
		for asset, usd := range assets {
			if usd <= 0 || math.IsNaN(usd) || math.IsInf(usd, 0) {
				return nil, fmt.Errorf("assetUsd %s %s must be a positive number", chain, asset)
			}
		}
	}
	return m, nil
}

func (m *method) checkTypeWeight(tw typeWeight) error {
	if tw.Weight < 0 || tw.Weight > 100 || math.IsNaN(tw.Weight) {
		return fmt.Errorf("weight must be between 0 and 100")
	}
	if _, ok := m.levels[tw.Receiving]; !ok {
		return fmt.Errorf("unknown receiving level %q", tw.Receiving)
	}
	if _, ok := m.levels[tw.Sending]; !ok {
		return fmt.Errorf("unknown sending level %q", tw.Sending)
	}
	return nil
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	case int32:
		return float64(n), true
	case uint32:
		return float64(n), true
	}
	return 0, false
}

func compileAll(what string, patterns []string) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("%s %q: %w", what, p, err)
		}
		out = append(out, re)
	}
	return out, nil
}

func withDefaults(t Thresholds) Thresholds {
	d := defaultThresholds
	if t.DustUSD > 0 {
		d.DustUSD = t.DustUSD
	}
	if t.LookalikeAmountCapUSD > 0 {
		d.LookalikeAmountCapUSD = t.LookalikeAmountCapUSD
	}
	if t.MinAmountUSD > 0 {
		d.MinAmountUSD = t.MinAmountUSD
	}
	if t.NativeDustUnits > 0 {
		d.NativeDustUnits = t.NativeDustUnits
	}
	if t.LookalikeMinSuffix > 0 {
		d.LookalikeMinSuffix = t.LookalikeMinSuffix
	}
	if t.PassThroughDays > 0 {
		d.PassThroughDays = t.PassThroughDays
	}
	if t.PassThroughOutShare > 0 {
		d.PassThroughOutShare = t.PassThroughOutShare
	}
	if t.PassThroughEndBalanceShare > 0 {
		d.PassThroughEndBalanceShare = t.PassThroughEndBalanceShare
	}
	if t.OtherAssetRiskMin > 0 {
		d.OtherAssetRiskMin = t.OtherAssetRiskMin
	}
	if len(t.DirectSanctionsTypes) > 0 {
		d.DirectSanctionsTypes = slices.Clone(t.DirectSanctionsTypes)
	}
	if t.CounterpartyLimit > 0 {
		d.CounterpartyLimit = t.CounterpartyLimit
	}
	if t.UnnamedLimit > 0 {
		d.UnnamedLimit = t.UnnamedLimit
	}
	if t.LabelBatch > 0 {
		d.LabelBatch = t.LabelBatch
	}
	if t.StatementMaxRows > 0 {
		d.StatementMaxRows = t.StatementMaxRows
	}
	if t.StatementMaxSeconds > 0 {
		d.StatementMaxSeconds = t.StatementMaxSeconds
	}
	if t.ReportRowsBudget > 0 {
		d.ReportRowsBudget = t.ReportRowsBudget
	}
	if t.DeadlineSeconds > 0 {
		d.DeadlineSeconds = t.DeadlineSeconds
	}
	if t.HubTransfers > 0 {
		d.HubTransfers = t.HubTransfers
	}
	if t.BranchMinUSD > 0 {
		d.BranchMinUSD = t.BranchMinUSD
	}
	if t.MaxHops > 0 {
		d.MaxHops = t.MaxHops
	}
	if t.GateRowsPerAddress > 0 {
		d.GateRowsPerAddress = t.GateRowsPerAddress
	}
	if t.ExplainLimit > 0 {
		d.ExplainLimit = t.ExplainLimit
	}
	return d
}

// sourceClass classes a label source by labelSources; "" when nothing matches.
func (m *method) sourceClass(source string) string {
	for _, s := range m.sources {
		if s.match.MatchString(source) {
			return s.class
		}
	}
	return ""
}

// entityKey names the entity a label value stands for; "" = it names none.
// Applied in this order: a value equal to its label type names no entity;
// prefixes are removed while one matches (the first in list order each time,
// never down to nothing); the first matching alias names the entity; if none
// matches, suffixes are removed while one matches and the aliases are tried
// again; what is left is the entity.
func (m *method) entityKey(labelType, value string) string {
	if value == labelType {
		return ""
	}
	k := value
	for changed := true; changed; {
		changed = false
		for _, p := range m.stripPrefix {
			if loc := p.FindStringIndex(k); loc != nil {
				if next := k[:loc[0]] + k[loc[1]:]; next != "" {
					k = next
					changed = true
					break
				}
			}
		}
	}
	if e, ok := m.aliasOf(k); ok {
		return e
	}
	for changed := true; changed; {
		changed = false
		for _, s := range m.stripSuffix {
			if s.MatchString(k) {
				if next := s.ReplaceAllString(k, ""); next != "" {
					k = next
					changed = true
				}
			}
		}
	}
	if e, ok := m.aliasOf(k); ok {
		return e
	}
	return k
}

func (m *method) aliasOf(k string) (string, bool) {
	for _, a := range m.alias {
		if a.match.MatchString(k) {
			return a.entity, true
		}
	}
	return "", false
}

// factor is weight x level of an entity type for one direction, in percent.
func (w weights) factor(typ string, dir direction) float64 {
	tw, ok := w.types[typ]
	if !ok {
		return 0
	}
	lvl := tw.Receiving
	if dir == sent {
		lvl = tw.Sending
	}
	return tw.Weight * w.levels[lvl]
}

func (m *method) typeRank(typ string) int {
	if i := slices.Index(m.order, typ); i >= 0 {
		return i
	}
	return len(m.order)
}

func normalizeChain(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
