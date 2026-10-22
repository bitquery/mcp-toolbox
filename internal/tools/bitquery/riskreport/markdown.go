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
	fmt.Fprintf(&b, "- Data as of: %s | view: %s | complete: %t | max hops reached: %d\n\n",
		p.DataAsOf, p.View, p.Complete, p.MaxHopsReached)

	sc := rep.Score
	fmt.Fprintf(&b, "## Score: %.1f / 100 (%s)\n\n", sc.Value, band(sc.Value))
	fmt.Fprintf(&b, "score = max(received %s, sent %s, direct_floor %.2f) = %.1f\n\n",
		scorePart(sc.ReceivedScore), scorePart(sc.SentScore), sc.DirectFloor, sc.Value)
	if sc.DirectFloorFrom != nil {
		fmt.Fprintf(&b, "Direct floor set by %v (%v, %v).\n\n",
			sc.DirectFloorFrom["entity"], sc.DirectFloorFrom["entity_type"], sc.DirectFloorFrom["direction"])
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

// writeFlagHeadlines writes the flags as headline counts, never the per-flag
// evidence or the look-alikes (those are in the structured object).
func writeFlagHeadlines(b *strings.Builder, rep *reportOut) {
	counts := map[string]int{}
	poisoning := -1
	for _, f := range rep.Flags {
		counts[f.Type]++
		if f.Type == "poisoning" {
			poisoning = intOf(f.Detail["count"])
		}
	}
	if n := counts["direct_sanctions"]; n > 0 {
		fmt.Fprintf(b, "- direct sanctions: %d\n", n)
	}
	if n := counts["risky_dust"]; n > 0 {
		fmt.Fprintf(b, "- risky dust: %d\n", n)
	}
	if n := counts["other_asset_direct_risk"]; n > 0 {
		fmt.Fprintf(b, "- other-asset direct risk: %d\n", n)
	}
	if poisoning >= 0 {
		fmt.Fprintf(b, "- poisoning: %d look-alike(s)\n", poisoning)
	}
	fmt.Fprintf(b, "- pass-through: %s\n", flagWord(rep.Params.FlagsStatus["pass_through"]))
	fmt.Fprintf(b, "- frozen: %s\n", flagWord(rep.Params.FlagsStatus["frozen"]))
}

// flagWord turns a flags_status value into a word for the human report.
func flagWord(status string) string {
	switch status {
	case "found":
		return "yes"
	case "not_found":
		return "no"
	case "":
		return "no"
	default:
		return status
	}
}
