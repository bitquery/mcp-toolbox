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

// The concise human report (format markdown). The whole report travels beside
// it as structured content; this is the text a model and a text-only host read,
// so it stays small and bounded whatever the report holds: data_as_of leads
// (within the first bytes, so a reader or a truncating client sees it), the
// connections and cuts are capped, and the look-alikes are already trimmed on
// the report object, so even a poisoned address prints one count here.

import (
	"fmt"
	"strings"
)

// markdownConnCap and markdownCutCap bound the two lists that can grow with the
// report, so the document stays well under a client's response cap.
const (
	markdownConnCap = 10
	markdownCutCap  = 16
)

// band is a coarse verdict for a score, for the human report only. The method
// configuration defines no bands, so these cut points are a display heuristic
// (the parent may want them configurable); they never feed back into the score.
func band(score float64) string {
	switch {
	case score >= 75:
		return "high"
	case score >= 50:
		return "elevated"
	case score >= 25:
		return "medium"
	case score > 0:
		return "low"
	default:
		return "none"
	}
}

func scorePart(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.2f", *v)
}

func hopsNote(h *int) string {
	if h == nil {
		return ""
	}
	if *h == 1 {
		return " [1 hop]"
	}
	return fmt.Sprintf(" [%d hops]", *h)
}

// nonZeroAmount is whether a formatted decimal amount carries any nonzero digit.
func nonZeroAmount(s string) bool {
	for _, r := range s {
		if r >= '1' && r <= '9' {
			return true
		}
	}
	return false
}

// intOf reads a count that may be an int (on the live object) or a float64
// (after a json round-trip).
func intOf(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

// markdownReport renders a report as a concise document.
func markdownReport(rep *reportOut) string {
	var b strings.Builder
	p := &rep.Params
	fmt.Fprintf(&b, "# Address risk report\n\n")
	fmt.Fprintf(&b, "- Chain: %s\n- Address: `%s`\n", rep.Summary.Chain, rep.Summary.Address)
	// max hops reached is meaningful only in view all; the direct view is always
	// one hop, so it is left off there.
	head := fmt.Sprintf("- Data as of: %s | view: %s | complete: %t", p.DataAsOf, p.View, p.Complete)
	if p.View == "all" {
		head += fmt.Sprintf(" | max hops reached: %d", p.MaxHopsReached)
	}
	fmt.Fprintf(&b, "%s\n\n", head)

	sc := rep.Score
	fmt.Fprintf(&b, "## Score: %.1f / 100 (%s)\n\n", sc.Value, band(sc.Value))
	fmt.Fprintf(&b, "score = max(received %s, sent %s, direct_floor %.2f) = %.1f\n\n",
		scorePart(sc.ReceivedScore), scorePart(sc.SentScore), sc.DirectFloor, sc.Value)
	if sc.DirectFloorFrom != nil {
		fmt.Fprintf(&b, "Direct floor set by %v (%v, %v).\n\n",
			sc.DirectFloorFrom["entity"], sc.DirectFloorFrom["entity_type"], sc.DirectFloorFrom["direction"])
	}
	if len(sc.TopDrivers) > 0 {
		fmt.Fprintf(&b, "Drivers (share of the scored flow → contribution to the score):\n")
		for _, d := range sc.TopDrivers {
			fmt.Fprintf(&b, "- %s (%s): %g%% → %g\n", d.EntityType, d.Direction, d.SharePct, d.Contribution)
		}
		b.WriteString("\n")
	}

	a := rep.Summary.Asset
	fmt.Fprintf(&b, "## Exposure\n\n")
	fmt.Fprintf(&b, "Asset %s: received %s, sent %s, balance %s. USD basis: %s.\n",
		a.Symbol, rep.Summary.Received, rep.Summary.Sent, rep.Summary.Balance, p.USDConversion)
	if rep.Exposure != "" {
		fmt.Fprintf(&b, "%s.\n", rep.Exposure)
	} else {
		fmt.Fprintf(&b, "Attributed %.2f%% of analysed flow to %d labelled connection(s).\n",
			rep.Summary.AttributedPct, len(rep.Connections))
	}
	if u := unattributedLine(rep, a.Symbol); u != "" {
		fmt.Fprintf(&b, "%s\n", u)
	}
	b.WriteString("\n")

	if len(rep.Connections) > 0 {
		fmt.Fprintf(&b, "## Top connections\n\n")
		for i, c := range rep.Connections {
			if i >= markdownConnCap {
				fmt.Fprintf(&b, "- … and %d more\n", len(rep.Connections)-markdownConnCap)
				break
			}
			tag := ""
			if c.SameEntity {
				tag = " [same entity as target — not scored]"
			}
			fmt.Fprintf(&b, "- %s (%s)%s: received %s%s, sent %s%s\n",
				c.Entity, c.EntityType, tag, c.Received, hopsNote(c.ReceivedHops), c.Sent, hopsNote(c.SentHops))
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "## Flags\n\n")
	writeFlagHeadlines(&b, rep)
	b.WriteString("\n")

	var cuts []cutOut
	for _, c := range p.Cuts {
		if nonZeroAmount(c.Amount) || (c.Amount == "" && c.Counterparts > 0) {
			cuts = append(cuts, c)
		}
	}
	if len(cuts) > 0 {
		fmt.Fprintf(&b, "## Cuts\n\n")
		for i, c := range cuts {
			if i >= markdownCutCap {
				fmt.Fprintf(&b, "- … and %d more\n", len(cuts)-markdownCutCap)
				break
			}
			dir := ""
			if c.Direction != "" {
				dir = " " + c.Direction
			}
			amt := c.Amount
			if amt == "" {
				amt = fmt.Sprintf("%d counterparties", c.Counterparts)
			}
			fmt.Fprintf(&b, "- %s%s: %s\n", c.Reason, dir, amt)
		}
		b.WriteString("\n")
	}

	if len(p.Notes) > 0 {
		fmt.Fprintf(&b, "## Notes\n\n")
		for _, n := range p.Notes {
			fmt.Fprintf(&b, "- %s\n", n)
		}
	}
	return b.String()
}

// unattributedLine sums the unattributed amounts per direction for the Exposure
// section, so a reader sees how much of the flow reached no labelled entity; ""
// when no direction is recorded.
func unattributedLine(rep *reportOut, symbol string) string {
	var parts []string
	for _, d := range []string{"received", "sent"} {
		if v, ok := rep.Unattributed[d]; ok {
			parts = append(parts, fmt.Sprintf("%s %s", d, v))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("Unattributed %s: %s.", symbol, strings.Join(parts, ", "))
}

// flagOrder is the fixed order the human report lists the flags in, each with a
// readable label; direct sanctions first.
var flagOrder = []struct{ key, label string }{
	{"direct_sanctions", "direct sanctions"},
	{"risky_dust", "risky dust"},
	{"other_asset_direct_risk", "other-asset direct risk"},
	{"poisoning", "poisoning"},
	{"pass_through", "pass-through"},
	{"frozen", "frozen"},
}

// writeFlagHeadlines writes every flag in the fixed order with its status word
// (found / not_found / not_evaluated, as recorded), and for a flag that fired a
// short headline from its evidence. The per-flag detail and the look-alikes
// stay in the structured object.
func writeFlagHeadlines(b *strings.Builder, rep *reportOut) {
	byType := map[string][]flagOut{}
	for _, f := range rep.Flags {
		byType[f.Type] = append(byType[f.Type], f)
	}
	for _, fo := range flagOrder {
		status := flagStatusOf(rep, fo.key, byType)
		line := fmt.Sprintf("- %s: %s", fo.label, status)
		if status == "found" {
			if d := flagHeadlineDetail(fo.key, byType[fo.key]); d != "" {
				line += " (" + d + ")"
			}
		}
		fmt.Fprintf(b, "%s\n", line)
	}
}

// flagStatusOf is a flag's status for the human report: the recorded
// flags_status when set, else derived from whether the flag fired (frozen,
// never a flag row, defaults to not_evaluated).
func flagStatusOf(rep *reportOut, key string, byType map[string][]flagOut) string {
	if s := rep.Params.FlagsStatus[key]; s != "" {
		return s
	}
	if len(byType[key]) > 0 {
		return "found"
	}
	if key == "frozen" {
		return "not_evaluated"
	}
	return "not_found"
}

// flagHeadlineDetail is a one-line summary of a fired flag, from its first row
// (with a count of any others): the look-alike count for poisoning, and the
// entity type, amount and direction for a labelled-counterparty flag.
func flagHeadlineDetail(key string, fs []flagOut) string {
	if len(fs) == 0 {
		return ""
	}
	switch key {
	case "poisoning":
		return fmt.Sprintf("%d look-alike(s)", intOf(fs[0].Detail["count"]))
	case "direct_sanctions", "risky_dust", "other_asset_direct_risk":
		d := fs[0].Detail
		et, _ := d["entity_type"].(string)
		dir, _ := d["direction"].(string)
		amt, _ := d["amount"].(string)
		s := et
		if amt != "" {
			s += ", " + amt
		}
		if dir != "" {
			s += " (" + dir + ")"
		}
		if len(fs) > 1 {
			s += fmt.Sprintf(" +%d more", len(fs)-1)
		}
		return s
	}
	return ""
}
