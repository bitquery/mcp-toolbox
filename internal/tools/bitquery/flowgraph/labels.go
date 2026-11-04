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
// one primary label type per counterparty. The pick is by source-class trust
// (sanctions-list > curated > vendor > research > heuristic > community, from
// classRank), then label type, then value; a type in neverPick is never the
// primary (it still appears in the counterparty's Labels). A ticket-specified
// priority order over types is a later refinement (it would slot in before the
// class-rank tie-break); v1 uses the class-rank default, which needs no product
// list to be correct.

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

type labelRow struct {
	Address   string
	LabelType string
	Value     string
	Source    string
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

// labelMethod is the compiled label-pick configuration.
type labelMethod struct {
	sources   []compiledSource
	classRank []string
	neverPick map[string]bool
}

var defaultClassRank = []string{"sanctions-list", "curated", "vendor", "research", "heuristic", "community"}

func compileLabelMethod(srcs []SourceClassRule, classRank, neverPick []string) (*labelMethod, error) {
	m := &labelMethod{classRank: slices.Clone(classRank), neverPick: map[string]bool{}}
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

// pick chooses the primary label type of a counterparty from its label rows:
// the row of the most-trusted source class (then type, then value) whose type
// is not in neverPick. It returns ("", "", types) when nothing is pickable;
// types is every distinct label type present, sorted, for the Labels field.
func (m *labelMethod) pick(rows []labelRow) (labelType, value string, types []string) {
	seen := map[string]bool{}
	var cand []labelRow
	for _, r := range rows {
		if !seen[r.LabelType] {
			seen[r.LabelType] = true
			types = append(types, r.LabelType)
		}
		if !m.neverPick[r.LabelType] {
			cand = append(cand, r)
		}
	}
	sort.Strings(types)
	if len(cand) == 0 {
		return "", "", types
	}
	sort.Slice(cand, func(i, j int) bool {
		a, b := cand[i], cand[j]
		ca, cb := m.classRankOf(m.sourceClass(a.Source)), m.classRankOf(m.sourceClass(b.Source))
		if ca != cb {
			return ca < cb
		}
		if a.LabelType != b.LabelType {
			return a.LabelType < b.LabelType
		}
		return a.Value < b.Value
	})
	return cand[0].LabelType, cand[0].Value, types
}

// labelsStmt reads the label rows of a batch of validated addresses on one
// chain. The addresses are 0x-hex (validated by the codec and lower-cased), so
// each is quoted as a literal; directory.labels stores EVM addresses in that
// form. The read is capped and billed through the labels source.
func labelsStmt(chain string, addrs []string, maxSeconds int) string {
	q := make([]string, len(addrs))
	for i, a := range addrs {
		q[i] = "'" + a + "'"
	}
	return fmt.Sprintf(`SELECT address, label_type, label_value, source
FROM directory.labels
WHERE chain = '%s' AND address IN (%s)
GROUP BY address, label_type, label_value, source
SETTINGS max_rows_to_read = %d, read_overflow_mode = 'throw', max_execution_time = %d`,
		chain, strings.Join(q, ", "), labelMaxRows, maxSeconds)
}

// fetchLabels reads the labels of the counterparties, in batches, each counted
// in the call's cost. The labels source returns result rows only (no statistics
// block), so a label statement adds a cost line with no rows/bytes. A batch that
// cannot start in the time left ends the fetch and the labels gathered so far
// are used (the call is marked partial by the caller).
func (t Tool) fetchLabels(ctx context.Context, ex *chainexec.Exec, chain string, addrs []string) (map[string][]labelRow, bool, error) {
	out := map[string][]labelRow{}
	for i := 0; i < len(addrs); i += labelBatch {
		batch := addrs[i:min(i+labelBatch, len(addrs))]
		if err := ex.MayStart(ctx, "labels"); err != nil {
			return out, true, nil // out of time: partial, not an error
		}
		res, err := t.labels.RunSQL(ctx, labelsStmt(chain, batch, ex.Secs(ctx)), nil)
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
			a := strings.ToLower(chainexec.AsStr(r["address"]))
			out[a] = append(out[a], labelRow{
				Address:   a,
				LabelType: chainexec.AsStr(r["label_type"]),
				Value:     chainexec.AsStr(r["label_value"]),
				Source:    chainexec.AsStr(r["source"]),
			})
		}
	}
	return out, false, nil
}
