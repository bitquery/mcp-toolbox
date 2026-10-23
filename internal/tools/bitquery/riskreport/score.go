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

// The score: per direction, the share of each entity type times its weight and
// its level for that direction, plus the unattributed share times its own
// weight; the report's score is the larger direction, or the direct floor when
// that is higher.

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"
)

type direction int

const (
	received direction = iota
	sent
)

func (d direction) String() string {
	if d == sent {
		return "sent"
	}
	return "received"
}

// formulaText is returned with every report.
const formulaText = "level factor d: High 1.0, Medium 0.6, Low 0.3, None 0 (as in weights_used.levels). " +
	"Per direction: dir_score = sum over entity types c of share_c * weight_c * d_c + share_unattributed * weight_unattributed, " +
	"where share = amount / that direction's connection total (transfers above min_amount_usd). " +
	"direct_floor = the highest weight_c * d_c of a 1-hop counterparty with an entity label that is listed as a connection (its transfers total above min_amount_usd), in the report asset. " +
	"score = max(received_score, sent_score, direct_floor). " +
	"Connections marked same_entity are removed from the direction's total before shares are taken, so the score is that of the flow that reached other entities; they stay in the breakdown and connections. " +
	"Hops: 1 = a direct counterparty of the address."

// weights are the profile in force for one report: the configured profile with
// the caller's override merged in.
type weights struct {
	order        []string
	types        map[string]typeWeight
	levels       map[string]float64
	unattributed float64
}

func (m *method) defaultWeights() weights {
	return weights{
		order:        m.order,
		types:        maps.Clone(m.profile),
		levels:       m.levels,
		unattributed: m.weightUnattributed,
	}
}

// mergeWeights applies a caller's partial override: per entity type a weight
// (0-100) and receiving / sending levels, and weight_unattributed.
func (m *method) mergeWeights(override map[string]any) (weights, error) {
	w := m.defaultWeights()
	keys := slices.Sorted(maps.Keys(override))
	for _, k := range keys {
		v := override[k]
		if k == "weight_unattributed" {
			f, ok := toFloat(v)
			if !ok || f < 0 || f > 100 || math.IsNaN(f) {
				return w, fmt.Errorf("weights.weight_unattributed must be a number from 0 to 100")
			}
			w.unattributed = f
			continue
		}
		tw, ok := w.types[k]
		if !ok {
			return w, fmt.Errorf("weights: unknown entity type %q; use one of: %s, weight_unattributed", truncate(k, 64), strings.Join(m.order, ", "))
		}
		fields, ok := v.(map[string]any)
		if !ok {
			return w, fmt.Errorf("weights.%s must be an object with weight, receiving and/or sending", k)
		}
		for fk, fv := range fields {
			switch fk {
			case "weight":
				f, ok := toFloat(fv)
				if !ok || f < 0 || f > 100 || math.IsNaN(f) {
					return w, fmt.Errorf("weights.%s.weight must be a number from 0 to 100", k)
				}
				tw.Weight = f
			case "receiving", "sending":
				s, ok := fv.(string)
				if _, known := w.levels[s]; !ok || !known {
					return w, fmt.Errorf("weights.%s.%s must be one of: %s", k, fk, strings.Join(levelNames(w.levels), ", "))
				}
				if fk == "receiving" {
					tw.Receiving = s
				} else {
					tw.Sending = s
				}
			default:
				return w, fmt.Errorf("weights.%s: unknown field %q (use weight, receiving, sending)", k, truncate(fk, 32))
			}
		}
		w.types[k] = tw
	}
	return w, nil
}

func levelNames(levels map[string]float64) []string {
	names := slices.Collect(maps.Keys(levels))
	sort.Slice(names, func(i, j int) bool {
		if levels[names[i]] != levels[names[j]] {
			return levels[names[i]] > levels[names[j]]
		}
		return names[i] < names[j]
	})
	return names
}

// dirAmounts are one direction's connection amounts, in units of the report
// asset (only their ratios matter).
type dirAmounts struct {
	byType       map[string]float64
	unattributed float64
}

func (a dirAmounts) total() float64 {
	t := a.unattributed
	for _, v := range a.byType {
		t += v
	}
	return t
}

// dirScore is the score of one direction in percent; ok is false when the
// direction has no amount at all. An amount under a type that is not in the
// profile counts in the total with weight 0.
func (w weights) dirScore(a dirAmounts, dir direction) (float64, bool) {
	tot := a.total()
	if tot <= 0 {
		return 0, false
	}
	num := a.unattributed * w.unattributed
	for typ, v := range a.byType {
		num += v * w.factor(typ, dir)
	}
	return num / tot, true
}

// driver is one entity type's contribution to a direction's score.
type driver struct {
	Direction    string  `json:"direction"`
	EntityType   string  `json:"entity_type"`
	SharePct     float64 `json:"share_pct"`
	Contribution float64 `json:"contribution"`
}

// topDrivers lists the largest contributions to both directions' scores.
func (w weights) topDrivers(amounts map[direction]dirAmounts, n int) []driver {
	var out []driver
	for _, dir := range []direction{received, sent} {
		a, ok := amounts[dir]
		if !ok {
			continue
		}
		tot := a.total()
		if tot <= 0 {
			continue
		}
		for typ, v := range a.byType {
			c := v * w.factor(typ, dir) / tot
			if c <= 0 {
				continue
			}
			out = append(out, driver{Direction: dir.String(), EntityType: typ, SharePct: round(100*v/tot, 2), Contribution: round(c, 2)})
		}
		if a.unattributed > 0 && w.unattributed > 0 {
			out = append(out, driver{Direction: dir.String(), EntityType: "Unattributed", SharePct: round(100*a.unattributed/tot, 2), Contribution: round(a.unattributed*w.unattributed/tot, 2)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Contribution != out[j].Contribution {
			return out[i].Contribution > out[j].Contribution
		}
		if out[i].Direction != out[j].Direction {
			return out[i].Direction < out[j].Direction
		}
		return out[i].EntityType < out[j].EntityType
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// weightsUsed is the profile in force, as returned with the report.
func (w weights) used() map[string]any {
	types := map[string]any{}
	for _, name := range w.order {
		tw := w.types[name]
		types[name] = map[string]any{"weight": tw.Weight, "receiving": tw.Receiving, "sending": tw.Sending}
	}
	return map[string]any{
		"entity_types":        types,
		"weight_unattributed": w.unattributed,
		"levels":              w.levels,
	}
}

func round(v float64, places int) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
