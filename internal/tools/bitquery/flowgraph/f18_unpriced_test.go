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
	"strings"
	"testing"
)

// TestMarkdownTotalFlowUnpricedF18 covers the group_by=address summary's
// "Total flow" segment. When the whole flow is unpriced (both Usd_In and Usd_Out
// NULL, so Usd_Total collapsed to 0) the old text printed a misleading
// "$0.00 (in $, out $)": a $0.00 that reads as a measured zero, and a bare "$"
// after "in" and "out" (acceptance finding F18). The fix prints "unpriced"
// there. Every case with a priced side — and the empty window — must render the
// segment byte-for-byte as before.
func TestMarkdownTotalFlowUnpricedF18(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	render := func(tot totalsOut) string {
		r := result{
			Chain:  "ethereum",
			Window: windowOut{AfterTime: "2026-11-09 00:00:00", BeforeTime: "2026-11-11 00:00:00"},
			Totals: tot,
		}
		return r.markdown()
	}

	cases := []struct {
		name     string
		tot      totalsOut
		contains []string // segment(s) that must be present (byte-identical proof)
		absent   []string // substrings that must NOT appear
	}{
		{
			name:     "both sides priced",
			tot:      totalsOut{UsdIn: f(1000), UsdOut: f(234.56), UsdTotal: 1234.56, TransfersIn: 2, TransfersOut: 1},
			contains: []string{"Total flow $1234.56 (in $1000.00, out $234.56); "},
		},
		{
			name:     "one side priced (out only) — unchanged, bare $ for in kept",
			tot:      totalsOut{UsdIn: nil, UsdOut: f(234.56), UsdTotal: 234.56, TransfersIn: 0, TransfersOut: 1},
			contains: []string{"Total flow $234.56 (in $, out $234.56); "},
		},
		{
			name:     "whole flow unpriced with transfers — F18 fix",
			tot:      totalsOut{UsdIn: nil, UsdOut: nil, UsdTotal: 0, TransfersIn: 3, TransfersOut: 5, NoPrice: 8},
			contains: []string{"Total flow: unpriced; "},
			absent:   []string{"Total flow $", "(in $, out $)"},
		},
		{
			name:     "empty window (no transfers) — unchanged",
			tot:      totalsOut{UsdIn: nil, UsdOut: nil, UsdTotal: 0, TransfersIn: 0, TransfersOut: 0},
			contains: []string{"Total flow $0.00 (in $, out $); "},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			md := render(c.tot)
			for _, want := range c.contains {
				if !strings.Contains(md, want) {
					t.Errorf("want segment %q in:\n%s", want, md)
				}
			}
			for _, no := range c.absent {
				if strings.Contains(md, no) {
					t.Errorf("did not want %q in:\n%s", no, md)
				}
			}
		})
	}
}
