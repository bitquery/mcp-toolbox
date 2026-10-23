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

// How an address's label rows become an entity: attribute-only rows describe
// the address and do nothing else; every other row goes through labelMap, first
// match wins, to an entity type or to evidence. An address with at least one
// entity type stops its branch and is attributed, per direction, to the type
// with the highest weight x level (a tie goes to the type listed first in the
// profile).

import (
	"slices"
	"sort"
)

// labelRow is one row of the labels source.
type labelRow struct {
	Address    string
	LabelType  string
	Value      string
	Source     string
	RecordedAt string
}

// typedLabel is a label row that maps to an entity type.
type typedLabel struct {
	Type    string
	Subtype string
	Row     labelRow
}

// addressLabels is what the label map makes of one address's rows.
type addressLabels struct {
	Types      []typedLabel
	Evidence   []labelRow
	Attributes []labelRow
	// Unmapped label types: in neither labelMap nor attributeOnly. They are
	// treated as evidence and counted in the report's parameters.
	Unmapped []string
}

// classify applies the label map to one address's rows.
func (m *method) classify(chain string, rows []labelRow) addressLabels {
	var out addressLabels
	for _, r := range rows {
		if m.attributeOnly[r.LabelType] {
			out.Attributes = append(out.Attributes, r)
			continue
		}
		rule, ok := m.match(chain, r.LabelType, r.Value)
		switch {
		case !ok:
			out.Evidence = append(out.Evidence, r)
			if !slices.Contains(out.Unmapped, r.LabelType) {
				out.Unmapped = append(out.Unmapped, r.LabelType)
			}
		case rule.evidenceOnly:
			out.Evidence = append(out.Evidence, r)
		default:
			out.Types = append(out.Types, typedLabel{Type: rule.typ, Subtype: rule.subtype, Row: r})
		}
	}
	return out
}

// match returns the first labelMap rule that matches a label row.
func (m *method) match(chain, labelType, value string) (compiledRule, bool) {
	for _, r := range m.rules {
		if len(r.types) > 0 && !slices.Contains(r.types, labelType) {
			continue
		}
		if r.chain != "" && r.chain != chain {
			continue
		}
		if r.value != nil && !r.value.MatchString(value) {
			continue
		}
		return r, true
	}
	return compiledRule{}, false
}

// sourcePriority orders label rows of one type: the row printed on a
// connection is the one from the most specific source class.
var sourcePriority = []string{"sanctions-list", "curated", "research", "vendor", "community", "heuristic"}

func (m *method) sourceRank(source string) int {
	if i := slices.Index(sourcePriority, m.sourceClass(source)); i >= 0 {
		return i
	}
	return len(sourcePriority)
}

// attribution is the entity an address is attributed to for one direction.
type attribution struct {
	Label      typedLabel
	Entity     string
	OtherTypes []string
	// SourceClass, when set, is the row's label_source_class instead of the
	// class labelSources gives its source (a mint has no label row).
	SourceClass string
	// CommunityTypes are the entity types of the address's community-class rows
	// excluded from score-driving because a higher-class row is present. They are
	// surfaced on the connection for transparency but never move the score.
	CommunityTypes []string
	// TypeConflict is set when an excluded community type is riskier (a higher
	// weight) than the chosen type: the community labels disagree with the scored
	// attribution.
	TypeConflict bool
}

// eligibleTypes splits an address's typed rows into those eligible to drive the
// score and the community-class rows excluded because the address carries a
// higher-class typed row. Community is the lowest-trust class; when no
// higher-class typed row is present every row stays eligible, so a community-only
// address is still attributed (its community rows simply never set the floor).
// Only typed rows count as "a higher-class row" — an attribute-only or
// evidence-only row must not strip a community-only address of its connection.
func (m *method) eligibleTypes(types []typedLabel) (eligible, excluded []typedLabel) {
	communityRank := m.classRankOf(communityClass)
	higher := false
	for _, l := range types {
		if m.classRankOf(m.sourceClass(l.Row.Source)) < communityRank {
			higher = true
			break
		}
	}
	if !higher {
		return slices.Clone(types), nil
	}
	for _, l := range types {
		if m.sourceClass(l.Row.Source) == communityClass {
			excluded = append(excluded, l)
		} else {
			eligible = append(eligible, l)
		}
	}
	return eligible, excluded
}

// attribute picks the entity type of an address for one direction: the highest
// weight x level, a tie going to the type listed first in the profile, and
// within the type the row from the most specific source. Community-class rows
// are excluded from the choice when the address carries a higher-class row (the
// excluded types are surfaced as CommunityTypes, never scored).
func (m *method) attribute(a addressLabels, w weights, dir direction) (attribution, bool) {
	if len(a.Types) == 0 {
		return attribution{}, false
	}
	cands, excluded := m.eligibleTypes(a.Types)
	sort.SliceStable(cands, func(i, j int) bool {
		fi, fj := w.factor(cands[i].Type, dir), w.factor(cands[j].Type, dir)
		if fi != fj {
			return fi > fj
		}
		ri, rj := m.typeRank(cands[i].Type), m.typeRank(cands[j].Type)
		if ri != rj {
			return ri < rj
		}
		return m.rowBefore(cands[i], cands[j])
	})
	best := cands[0]
	out := attribution{Label: best, Entity: m.entityKey(best.Row.LabelType, best.Row.Value)}
	for _, c := range cands[1:] {
		if c.Type != best.Type && !slices.Contains(out.OtherTypes, c.Type) {
			out.OtherTypes = append(out.OtherTypes, c.Type)
		}
	}
	// Excluded community rows are surfaced but never score. A community type that
	// is riskier (a higher weight) than the chosen type is a conflict worth
	// flagging; a community row of the chosen type itself is not listed.
	bestWeight := w.types[best.Type].Weight
	for _, c := range excluded {
		if c.Type == best.Type || slices.Contains(out.CommunityTypes, c.Type) {
			continue
		}
		out.CommunityTypes = append(out.CommunityTypes, c.Type)
		if w.types[c.Type].Weight > bestWeight {
			out.TypeConflict = true
		}
	}
	return out, true
}

// rowBefore orders two label rows of one entity type: the most specific source
// class first, then sourcePreference, then the source name, then the value.
// Nothing time-dependent takes part, so a list refresh never renames an entity.
// Every place that picks one row of a type uses it, so a report names an entity
// the same way everywhere.
func (m *method) rowBefore(a, b typedLabel) bool {
	if sa, sb := m.sourceRank(a.Row.Source), m.sourceRank(b.Row.Source); sa != sb {
		return sa < sb
	}
	if pa, pb := m.prefRank(a.Row.Source), m.prefRank(b.Row.Source); pa != pb {
		return pa < pb
	}
	if a.Row.Source != b.Row.Source {
		return a.Row.Source < b.Row.Source
	}
	return a.Row.Value < b.Row.Value
}

func (m *method) prefRank(source string) int {
	if i := slices.Index(m.sourcePref, source); i >= 0 {
		return i
	}
	return len(m.sourcePref)
}

// sourcesOf lists the sources of an address's rows of one entity type, the
// chosen row's first.
func sourcesOf(a addressLabels, typ string) []string {
	var out []string
	for _, l := range a.Types {
		if l.Type == typ && !slices.Contains(out, l.Row.Source) {
			out = append(out, l.Row.Source)
		}
	}
	return out
}
