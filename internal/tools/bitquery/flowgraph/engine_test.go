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
	"text/template"

	"github.com/googleapis/mcp-toolbox/internal/tools/bitquery/chainexec"
)

func testTool() Tool {
	tmpl := template.Must(template.New("x").Parse("SELECT {{.TopK}}"))
	lm, _ := compileLabelMethod(nil, nil, nil, nil, nil)
	return Tool{
		stmts:      map[string]*template.Template{"ethereum": tmpl, "arc": tmpl, "tron": tmpl, "bitcoin": tmpl, "zcash": tmpl},
		chains:     map[string]chainexec.HTTPSource{"ethereum": nil, "arc": nil, "tron": nil, "bitcoin": nil, "zcash": nil},
		budget:     Budget{}.withDefaults(),
		lm:         lm,
		noCoverage: map[string]bool{"dash": true, "arc": true},
	}
}

const (
	addrA = "0xAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	addrB = "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestParseValid(t *testing.T) {
	req, err := testTool().parse(map[string]any{
		"chain":     "ethereum",
		"addresses": addrA + " | " + addrB,
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(req.addrs) != 2 || req.addrs[0] != strings.ToLower(addrA) {
		t.Fatalf("addrs = %v", req.addrs)
	}
	if req.requested != 2 || len(req.invalid) != 0 {
		t.Fatalf("requested=%d invalid=%v", req.requested, req.invalid)
	}
	if req.limit != defaultLimit || req.format != "markdown" || req.groupBy != "address" {
		t.Fatalf("defaults wrong: limit=%d format=%q group=%q", req.limit, req.format, req.groupBy)
	}
	// default window is defaultDays wide.
	if d := req.wEnd.Sub(req.wStart).Hours() / 24; d < defaultDays-1 || d > defaultDays+1 {
		t.Fatalf("default window span %.1f days, want ~%d", d, defaultDays)
	}
	if req.includeInternal != 0 {
		t.Fatalf("ethereum include_internal default = %d, want 0", req.includeInternal)
	}
}

func TestParseInvalidAndDedupe(t *testing.T) {
	req, err := testTool().parse(map[string]any{
		"chain":     "ethereum",
		"addresses": addrA + ", garbage, " + strings.ToLower(addrA),
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(req.addrs) != 1 {
		t.Fatalf("dedupe failed: addrs = %v", req.addrs)
	}
	if len(req.invalid) != 1 || req.invalid[0] != "garbage" {
		t.Fatalf("invalid = %v", req.invalid)
	}
	if req.requested != 3 {
		t.Fatalf("requested = %d, want 3", req.requested)
	}
}

func TestParseRejections(t *testing.T) {
	cases := []struct {
		name string
		p    map[string]any
	}{
		{"no valid address", map[string]any{"chain": "ethereum", "addresses": "garbage"}},
		{"unknown chain", map[string]any{"chain": "dogecoin", "addresses": addrA}},
		{"no statement", map[string]any{"chain": "bsc", "addresses": addrA}},
		{"bad group_by", map[string]any{"chain": "ethereum", "addresses": addrA, "group_by": "token"}},
		{"after after before", map[string]any{"chain": "ethereum", "addresses": addrA, "after_time": "2026-11-03", "before_time": "2026-08-05"}},
		{"bad time", map[string]any{"chain": "ethereum", "addresses": addrA, "before_time": "nonsense"}},
		{"negative min_usd", map[string]any{"chain": "ethereum", "addresses": addrA, "min_usd": -1.0}},
		{"bad format", map[string]any{"chain": "ethereum", "addresses": addrA, "format": "xml"}},
	}
	for _, c := range cases {
		if _, err := testTool().parse(c.p); err == nil {
			t.Errorf("%s: expected an error", c.name)
		}
	}
}

func TestParseKnobs(t *testing.T) {
	// limit clamps to [1, maxLimit].
	hi, _ := testTool().parse(map[string]any{"chain": "ethereum", "addresses": addrA, "limit": int64(5000)})
	if hi.limit != maxLimit {
		t.Fatalf("limit clamp high = %d, want %d", hi.limit, maxLimit)
	}
	lo, _ := testTool().parse(map[string]any{"chain": "ethereum", "addresses": addrA, "limit": int64(0)})
	if lo.limit != 1 {
		t.Fatalf("limit clamp low = %d, want 1", lo.limit)
	}
	// Arc defaults include_internal to 1; an explicit value overrides.
	arc, _ := testTool().parse(map[string]any{"chain": "arc", "addresses": addrA})
	if arc.includeInternal != 1 {
		t.Fatalf("arc include_internal default = %d, want 1", arc.includeInternal)
	}
	off, _ := testTool().parse(map[string]any{"chain": "arc", "addresses": addrA, "include_internal": int64(0)})
	if off.includeInternal != 0 {
		t.Fatalf("explicit include_internal = %d, want 0", off.includeInternal)
	}
}

func TestShapeAddressRemainder(t *testing.T) {
	doc := map[string]any{
		"data": []any{
			map[string]any{"Counterparty": "0xaaa", "Usd_In": 100.0, "Usd_Out": nil, "Usd_Total": 100.0,
				"Transfers_In": "5", "Transfers_Out": "0", "Unpriced_Transfers_In": "1", "Unpriced_Transfers_Out": "0",
				"First_Seen": "2026-08-05 00:00:00", "Last_Seen": "2026-11-01 00:00:00"},
			map[string]any{"Counterparty": "0xbbb", "Usd_In": nil, "Usd_Out": 50.0, "Usd_Total": 50.0,
				"Transfers_In": "0", "Transfers_Out": "3", "Unpriced_Transfers_In": "0", "Unpriced_Transfers_Out": "2",
				"First_Seen": "", "Last_Seen": ""},
		},
		"totals": map[string]any{"Usd_In": 100.0, "Usd_Out": 50.0, "Usd_Total": 500.0,
			"Transfers_In": "10", "Transfers_Out": "20", "T_No_Price": "4", "T_Suspect": "0", "T_Stale": "2"},
	}
	req := request{chain: "ethereum"}
	res := shapeAddress(req, doc)
	if len(res.Counterparties) != 2 {
		t.Fatalf("rows = %d", len(res.Counterparties))
	}
	if res.Counterparties[0].UsdOut != nil {
		t.Error("row 0 Usd_Out should be nil (no priced outflow)")
	}
	if res.Counterparties[1].UsdIn != nil {
		t.Error("row 1 Usd_In should be nil")
	}
	// totals 500 - (100+50) = 350; transfers (10+20) - (5+3) = 22.
	if res.BelowCutoff.Usd != 350 {
		t.Errorf("below_cutoff.Usd = %v, want 350", res.BelowCutoff.Usd)
	}
	if res.BelowCutoff.Transfers != 22 {
		t.Errorf("below_cutoff.Transfers = %d, want 22", res.BelowCutoff.Transfers)
	}
	if res.Totals.Stale != 2 || res.Totals.NoPrice != 4 {
		t.Errorf("status counters wrong: %+v", res.Totals)
	}
	// Markdown renders without panicking and carries the CSV header (A3 added
	// label_status + the unpriced / token_count columns).
	if md := res.markdown(); !strings.Contains(md, "counterparty,label,label_status,usd_in,usd_out") {
		t.Error("markdown missing CSV header")
	}
}
