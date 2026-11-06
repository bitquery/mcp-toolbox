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

// Labels: for the returned counterparties, read directory.labels in Go and pick
// one primary label type per counterparty by the BIT-16068 section-3 rule. The
// pick drops the never-picked types (attributes of the address, not of who owns
// it), then chooses the remaining type that comes FIRST in the labelPriority list
// (risk types, then exchanges/services, then DeFi/contracts); a type in neither
// labelPriority nor labelOtherOrder is ranked after both, alphabetically. Within
// the chosen type the row of the most-trusted source class wins (classRank), then
// the most distinct sources, then the lowest value. A counterparty with no
// pickable label (none, or only never-pick/attribute types) has no primary and
// counts as unlabeled; its labels still list every type. The lists are returned
// verbatim as Pick_Rule on every call so a reader can check the pick.

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/googleapis/mcp-toolbox/internal/tools/bitquery/chainexec"
)

const (
	labelBatch       = 2000
	labelMaxRows     = 200_000_000
	noneLabel        = "(none)"
	belowCutoffLabel = "(below_cutoff)"
)

// labelStatus is why a counterparty has, or lacks, a primary label:
//   - labeled    — a primary label type was picked;
//   - unlabeled  — the counterparty carries label rows, but only never-pick
//     (attribute) types, so none is a primary;
//   - none       — the directory returned no label row at all for it, on a chain
//     the directory DOES cover (so the absence is informative);
//   - no_coverage — the whole chain has no label data (cfg noCoverageChains, e.g.
//     dash/arc); the labels statement is skipped and the absence says nothing.
//
// notLookedUp (a hub gate that labels only the top-N) is reserved for later work.
type labelStatus string

const (
	statusLabeled     labelStatus = "labeled"
	statusUnlabeled   labelStatus = "unlabeled"
	statusNone        labelStatus = "none"
	statusNoCoverage  labelStatus = "no_coverage"
	statusNotLookedUp labelStatus = "not_looked_up" //nolint:unused // reserved for a hub top-N gate
)

// pickStatus is the Label_Status for a covered chain from the pick result: labeled
// when a primary type was chosen, unlabeled when the counterparty carries label rows
// but only never-pick (attribute) types, and none when the directory returned no row
// for it at all. (no_coverage is decided a level up, per chain, not here.)
func pickStatus(picked string, types []string) labelStatus {
	switch {
	case picked != "":
		return statusLabeled
	case len(types) > 0:
		return statusUnlabeled
	default:
		return statusNone
	}
}

// matchLabelFilter reports whether a picked primary label type passes the caller's
// label_type / exclude_label_type filters: it must be in include (when include is
// non-empty) and not in exclude. An unlabeled counterparty (picked == "") fails a
// non-empty include — it has no primary type to match — but passes an exclude-only
// filter, since exclude removes named types, not the unlabeled.
func matchLabelFilter(picked string, include, exclude []string) bool {
	if len(include) > 0 && !slices.Contains(include, picked) {
		return false
	}
	return !slices.Contains(exclude, picked)
}

type labelRow struct {
	Address   string
	Chain     string
	LabelType string
	Value     string
	Source    string
}

// contractLabelTypes are the label types that mark the address as a CONTRACT: the
// whole DeFi/contracts block of the priority, plus proxy. When an EVM address carries
// one ON THE TOOL CHAIN, the same 20-byte key on another EVM chain is an unrelated
// deployment, so its labels must NOT be merged in (labels.md: do not auto-widen
// contract-type labels across chains). Exchange/service types (cex-*, otc-desk,
// custodian, …) are deliberately NOT here: those are wallets and SHOULD widen.
var contractLabelTypes = map[string]bool{
	"contract": true, "token-contract": true, "dex-router": true, "dex-pool": true,
	"dex-aggregator": true, "lending-protocol": true, "staking-protocol": true,
	"derivatives-protocol": true, "yield-aggregator": true, "bridge": true, "proxy": true,
	"asset-manager": true, "market-maker": true, "service": true, "infrastructure": true,
}

// restrictForContract narrows an address's cross-chain label rows to the tool chain
// when the address is a contract ON THE TOOL CHAIN: a different chain's same-key
// deployment is a different owner, so merging its labels would manufacture a false
// attribution. A plain wallet (no contract-type label on the tool chain) keeps every
// chain's labels — one owner spans EVM. A single-chain lookup (every row already on
// the tool chain) is a no-op. (A contract whose only label is attributed to another
// chain reads as a wallet here and widens — correct, since that other-chain label is
// then the address's only one.)
func restrictForContract(rows []labelRow, toolChain string) []labelRow {
	contract := false
	for _, r := range rows {
		if r.Chain == toolChain && contractLabelTypes[r.LabelType] {
			contract = true
			break
		}
	}
	if !contract {
		return rows
	}
	out := make([]labelRow, 0, len(rows))
	for _, r := range rows {
		if r.Chain == toolChain {
			out = append(out, r)
		}
	}
	return out
}

// SourceClassRule classes a label source; the first match wins.
type SourceClassRule struct {
	Match string `yaml:"match"`
	Class string `yaml:"class"`
}

type compiledSource struct {
	re    *regexp.Regexp
	class string
}

// labelMethod is the compiled label-pick configuration (BIT-16068 section 3).
type labelMethod struct {
	sources   []compiledSource
	classRank []string
	// priority is the type-priority list; priorityIndex maps a type to its position.
	priority      []string
	priorityIndex map[string]int
	// otherOrder ranks the types outside priority (a measured descending uniq(address)
	// order); a type in neither list is ranked last, alphabetically. otherIndex maps a
	// type to its position in otherOrder.
	otherOrder []string
	otherIndex map[string]int
	neverPick  map[string]bool
}

var defaultClassRank = []string{"sanctions-list", "curated", "vendor", "research", "heuristic", "community"}

func compileLabelMethod(srcs []SourceClassRule, classRank, neverPick, priority, otherOrder []string) (*labelMethod, error) {
	m := &labelMethod{
		classRank:     slices.Clone(classRank),
		priority:      slices.Clone(priority),
		otherOrder:    slices.Clone(otherOrder),
		priorityIndex: map[string]int{},
		otherIndex:    map[string]int{},
		neverPick:     map[string]bool{},
	}
	if len(m.classRank) == 0 {
		m.classRank = slices.Clone(defaultClassRank)
	}
	for _, s := range srcs {
		re, err := regexp.Compile(s.Match)
		if err != nil {
			return nil, fmt.Errorf("labelSources %q: %w", s.Match, err)
		}
		m.sources = append(m.sources, compiledSource{re: re, class: s.Class})
	}
	for i, t := range m.priority {
		if _, dup := m.priorityIndex[t]; !dup {
			m.priorityIndex[t] = i
		}
	}
	for i, t := range m.otherOrder {
		if _, dup := m.otherIndex[t]; !dup {
			m.otherIndex[t] = i
		}
	}
	for _, t := range neverPick {
		m.neverPick[t] = true
	}
	return m, nil
}

func (m *labelMethod) sourceClass(source string) string {
	for _, s := range m.sources {
		if s.re.MatchString(source) {
			return s.class
		}
	}
	return ""
}

func (m *labelMethod) classRankOf(class string) int {
	if i := slices.Index(m.classRank, class); i >= 0 {
		return i
	}
	return len(m.classRank)
}

// typeTier ranks a label type for the pick: tier 0 = in the priority list (by its
// position there), tier 1 = in the other-order list (by position), tier 2 = a type
// in neither, ordered alphabetically among themselves after both lists.
func (m *labelMethod) typeTier(t string) (tier, index int) {
	if i, ok := m.priorityIndex[t]; ok {
		return 0, i
	}
	if i, ok := m.otherIndex[t]; ok {
		return 1, i
	}
	return 2, 0
}

// typeLess reports whether label type a outranks b for the pick.
func (m *labelMethod) typeLess(a, b string) bool {
	ta, ia := m.typeTier(a)
	tb, ib := m.typeTier(b)
	if ta != tb {
		return ta < tb
	}
	if ta == 2 {
		return a < b // truly unknown types: alphabetical
	}
	return ia < ib
}

// pick chooses a counterparty's primary label type and value from its label rows,
// by the BIT-16068 section-3 rule (see the package comment). It returns
// ("", "", types) when nothing is pickable (no labels, or only never-pick /
// attribute-only types); types is every distinct label type present, sorted, for
// the Labels field.
func (m *labelMethod) pick(rows []labelRow) (labelType, value, chain string, types []string) {
	typeSeen := map[string]bool{}
	candByType := map[string][]labelRow{}
	var candTypes []string
	for _, r := range rows {
		if !typeSeen[r.LabelType] {
			typeSeen[r.LabelType] = true
			types = append(types, r.LabelType)
		}
		if m.neverPick[r.LabelType] {
			continue
		}
		if _, ok := candByType[r.LabelType]; !ok {
			candTypes = append(candTypes, r.LabelType)
		}
		candByType[r.LabelType] = append(candByType[r.LabelType], r)
	}
	sort.Strings(types)
	if len(candTypes) == 0 {
		return "", "", "", types
	}
	// Choose the type highest in the priority order (then other-order, then
	// alphabetical among unknown types).
	chosen := candTypes[0]
	for _, t := range candTypes[1:] {
		if m.typeLess(t, chosen) {
			chosen = t
		}
	}
	// Within the chosen type, pick the best value: most-trusted source class, then
	// the most distinct sources, then the lowest value alphabetically. source_count
	// is the number of distinct sources behind a (type, value), from the fetched
	// rows (one row per source).
	type valueAgg struct {
		value     string
		chain     string // the alphabetically-lowest chain carrying this value (the final tiebreak, and the reported Label chain)
		bestClass int
		sources   map[string]bool
	}
	aggs := map[string]*valueAgg{}
	var order []string
	for _, r := range candByType[chosen] {
		a := aggs[r.Value]
		if a == nil {
			a = &valueAgg{value: r.Value, chain: r.Chain, bestClass: len(m.classRank) + 1, sources: map[string]bool{}}
			aggs[r.Value] = a
			order = append(order, r.Value)
		}
		if r.Chain != "" && (a.chain == "" || r.Chain < a.chain) {
			a.chain = r.Chain
		}
		if cr := m.classRankOf(m.sourceClass(r.Source)); cr < a.bestClass {
			a.bestClass = cr
		}
		a.sources[r.Source] = true
	}
	best := aggs[order[0]]
	for _, v := range order[1:] {
		a := aggs[v]
		switch {
		case a.bestClass != best.bestClass:
			if a.bestClass < best.bestClass {
				best = a
			}
		case len(a.sources) != len(best.sources):
			if len(a.sources) > len(best.sources) {
				best = a
			}
		case a.value < best.value:
			best = a
		}
	}
	return chosen, best.value, best.chain, types
}

// PickRule renders the label-pick rule as text, for a tool to return verbatim on
// every call (the Pick_Rule scope row, BIT-16068 section 3) so a reader can check
// the pick. It emits nothing itself; the output layer places it in a scope row.
func (m *labelMethod) PickRule() string {
	var b strings.Builder
	b.WriteString("Label_Type pick: of a counterparty's labels, the never-picked types are dropped, ")
	b.WriteString("the primary is the remaining type earliest in PRIORITY (then OTHER, then alphabetical), ")
	b.WriteString("and within that type the most-trusted source class wins, then the most sources, then the lowest value.")
	b.WriteString(" On an EVM chain the labels of every EVM chain the address is known on compete together under this one rule.")
	b.WriteString("\nPRIORITY: " + strings.Join(m.priority, ", "))
	if len(m.otherOrder) > 0 {
		b.WriteString("\nOTHER (descending address count): " + strings.Join(m.otherOrder, ", "))
	} else {
		b.WriteString("\nOTHER: every remaining type, alphabetically")
	}
	never := make([]string, 0, len(m.neverPick))
	for t := range m.neverPick {
		never = append(never, t)
	}
	sort.Strings(never)
	b.WriteString("\nNEVER_PICK: " + strings.Join(never, ", "))
	return b.String()
}

// labelsStmt reads the label rows of a batch of validated addresses across one or
// more directory chains. An EVM tool-chain looks the address up over the whole EVM
// directory list (a label for one owner can sit under any EVM chain); a single-chain
// tool-chain (tron, …) looks up its one chain. chain is selected and grouped so each
// row carries it: the shaping restricts a CONTRACT address to its tool chain (a
// same-key deployment on another chain is a different owner), and the pick reports
// which chain its label came from. The pick's distinct-source count stays by source
// name, so the same (type,value,source) on several chains is not over-counted. Each
// address is already in the chain's directory form (0x-hex lower for EVM, base58 for
// Tron — codec.normalize, never ToLower) and quoted as a literal; chain names are
// cfg-validated (resolveLabelChains). Capped and billed through the labels source.
func labelsStmt(chains, addrs []string, maxSeconds int) string {
	qc := make([]string, len(chains))
	for i, c := range chains {
		qc[i] = "'" + c + "'"
	}
	qa := make([]string, len(addrs))
	for i, a := range addrs {
		qa[i] = "'" + a + "'"
	}
	return fmt.Sprintf(`SELECT address, chain, label_type, label_value, source
FROM directory.labels
WHERE chain IN (%s) AND address IN (%s)
GROUP BY address, chain, label_type, label_value, source
SETTINGS max_rows_to_read = %d, read_overflow_mode = 'throw', max_execution_time = %d`,
		strings.Join(qc, ", "), strings.Join(qa, ", "), labelMaxRows, maxSeconds)
}

// fetchLabels reads the labels of the counterparties, in batches, each counted
// in the call's cost. The labels source returns result rows only (no statistics
// block), so a label statement adds a cost line with no rows/bytes. A batch that
// cannot start in the time left ends the fetch and the labels gathered so far
// are used (the call is marked partial by the caller).
func (t Tool) fetchLabels(ctx context.Context, ex *chainexec.Exec, codec chainCodec, chains, addrs []string) (map[string][]labelRow, bool, error) {
	out := map[string][]labelRow{}
	for i := 0; i < len(addrs); i += labelBatch {
		batch := addrs[i:min(i+labelBatch, len(addrs))]
		if err := ex.MayStart(ctx, "labels"); err != nil {
			return out, true, nil // out of time: partial, not an error
		}
		res, err := t.labels.RunSQL(ctx, labelsStmt(chains, batch, ex.Secs(ctx)), nil)
		ex.Cost.Statements++
		ex.Cost.Detail = append(ex.Cost.Detail, chainexec.StatementCost{Name: "labels"})
		if err != nil {
			return out, false, err
		}
		rows, _ := res.([]any)
		for _, x := range rows {
			r, ok := x.(map[string]any)
			if !ok {
				continue
			}
			// Key by the chain's directory form (codec.normalize: identity on Tron,
			// lower on EVM) so it matches the counterparty lookup in attachLabels /
			// shapeLabelType; a ToLower here corrupts a base58 Tron address.
			a := codec.normalize(chainexec.AsStr(r["address"]))
			out[a] = append(out[a], labelRow{
				Address:   a,
				Chain:     chainexec.AsStr(r["chain"]),
				LabelType: chainexec.AsStr(r["label_type"]),
				Value:     chainexec.AsStr(r["label_value"]),
				Source:    chainexec.AsStr(r["source"]),
			})
		}
	}
	return out, false, nil
}
