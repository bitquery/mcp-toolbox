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
	"time"
)

// TestParseDirectionDefault: direction defaults to both, and the arm selectors
// follow in/out/both.
func TestParseDirectionDefault(t *testing.T) {
	req, err := testTool().parse(map[string]any{"chain": "ethereum", "addresses": addrA})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.direction != "both" || !req.keepSender() || !req.keepReceiver() {
		t.Fatalf("default direction=%q keepSender=%v keepReceiver=%v", req.direction, req.keepSender(), req.keepReceiver())
	}
	out, _ := testTool().parse(map[string]any{"chain": "ethereum", "addresses": addrA, "direction": "out"})
	if !out.keepSender() || out.keepReceiver() {
		t.Fatalf("direction=out keepSender=%v keepReceiver=%v, want true/false", out.keepSender(), out.keepReceiver())
	}
	in, _ := testTool().parse(map[string]any{"chain": "ethereum", "addresses": addrA, "direction": "in"})
	if in.keepSender() || !in.keepReceiver() {
		t.Fatalf("direction=in keepSender=%v keepReceiver=%v, want false/true", in.keepSender(), in.keepReceiver())
	}
}

// TestParseFiltersCarried: include_zero_value, token and the label-type lists.
func TestParseFiltersCarried(t *testing.T) {
	req, err := testTool().parse(map[string]any{
		"chain":              "ethereum",
		"addresses":          addrA,
		"include_zero_value": int64(1),
		"token":              "0x" + strings.Repeat("a", 40),
		"label_type":         "cex-hot-wallet, cex-deposit-address",
		"exclude_label_type": "mixer",
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.includeZero != 1 {
		t.Fatalf("includeZero = %d, want 1", req.includeZero)
	}
	if req.token != strings.Repeat("a", 40) {
		t.Fatalf("token key = %q, want 40 'a' (lower hex, no 0x)", req.token)
	}
	if len(req.labelTypes) != 2 || req.labelTypes[0] != "cex-hot-wallet" {
		t.Fatalf("labelTypes = %v", req.labelTypes)
	}
	if len(req.excludeLabelTypes) != 1 || req.excludeLabelTypes[0] != "mixer" {
		t.Fatalf("excludeLabelTypes = %v", req.excludeLabelTypes)
	}
}

func TestParseFilterRejections(t *testing.T) {
	cases := map[string]map[string]any{
		"bad direction": {"chain": "ethereum", "addresses": addrA, "direction": "sideways"},
		"bad token":     {"chain": "ethereum", "addresses": addrA, "token": "not-a-token"},
		"bad label":     {"chain": "ethereum", "addresses": addrA, "label_type": "Bad Type!"},
	}
	for name, p := range cases {
		if _, err := testTool().parse(p); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// TestParseWindowUnclamped: a window far longer than 366 days is passed through
// as given (the eth_flows_between 366-day cut must not apply here — AT1/AT9).
func TestParseWindowUnclamped(t *testing.T) {
	req, err := testTool().parse(map[string]any{
		"chain":       "ethereum",
		"addresses":   addrA,
		"after_time":  "2025-01-01",
		"before_time": "2026-11-04",
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !req.wStart.Equal(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("wStart = %v, want 2025-01-01 (unclamped)", req.wStart)
	}
	if d := req.wEnd.Sub(req.wStart).Hours() / 24; d < 600 {
		t.Fatalf("window span %.0f days, want the full >600 (not cut to 366)", d)
	}
}

func TestParseTimeUnixSeconds(t *testing.T) {
	ts, ok, err := parseTime("1735689600") // 2025-01-01T00:00:00Z
	if err != nil || !ok {
		t.Fatalf("parseTime unix: ok=%v err=%v", ok, err)
	}
	if ts.UTC().Format("2006-01-02") != "2025-01-01" {
		t.Fatalf("unix parse = %v", ts)
	}
}

// TestRenderArmsAndLiterals renders a template shaped like the real statement's
// direction arms + the new literals, and checks each direction keeps the right
// arm and the zero/token literals substitute.
func TestRenderArmsAndLiterals(t *testing.T) {
	tmpl := template.Must(template.New("x").Option("missingkey=error").Parse(
		"inc={{.IncludeInternal}} zero={{.IncludeZeroValue}} tok='{{.TokenFilter}}' k={{.TopK}} mu={{.MinUsd}}\n" +
			"{{if .KeepSender}}SENDER_ARM{{end}}{{if and .KeepSender .KeepReceiver}} UNION {{end}}{{if .KeepReceiver}}RECEIVER_ARM{{end}}"))
	render1 := func(ks, kr bool) string {
		out, err := render(tmpl, stmtVars{
			SetLiteral: "[]", WStart: "''", WEnd: "''", MinUsd: "0", TopK: 100,
			IncludeInternal: 1, IncludeZeroValue: 1, TokenFilter: "abc",
			KeepSender: ks, KeepReceiver: kr,
		})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		return out
	}
	both := render1(true, true)
	if !strings.Contains(both, "SENDER_ARM") || !strings.Contains(both, "RECEIVER_ARM") || !strings.Contains(both, "UNION") {
		t.Fatalf("both: %q", both)
	}
	out := render1(true, false)
	if !strings.Contains(out, "SENDER_ARM") || strings.Contains(out, "RECEIVER_ARM") || strings.Contains(out, "UNION") {
		t.Fatalf("out-only kept the wrong arm: %q", out)
	}
	in := render1(false, true)
	if strings.Contains(in, "SENDER_ARM") || !strings.Contains(in, "RECEIVER_ARM") {
		t.Fatalf("in-only kept the wrong arm: %q", in)
	}
	if !strings.Contains(both, "zero=1") || !strings.Contains(both, "tok='abc'") {
		t.Fatalf("literals not substituted: %q", both)
	}
}

// TestSelfTransfersGuard: the Self_Transfers subquery is emitted only for a
// multi-address set; a single address renders a literal 0 and no subquery (so it
// pays no extra scan on the billing fork).
func TestSelfTransfersGuard(t *testing.T) {
	tmpl := template.Must(template.New("x").Option("missingkey=error").Parse(
		"{{if .MultiAddr}}( SELECT count() FROM eth_api.transfers_sender ) AS self_transfers{{else}}0 AS self_transfers{{end}}"))
	v := stmtVars{SetLiteral: "[]", WStart: "''", WEnd: "''", MinUsd: "0"}
	v.MultiAddr = false
	single, err := render(tmpl, v)
	if err != nil {
		t.Fatalf("render single: %v", err)
	}
	if strings.Contains(single, "SELECT count()") || !strings.Contains(single, "0 AS self_transfers") {
		t.Fatalf("single-address should render literal 0, no subquery: %q", single)
	}
	v.MultiAddr = true
	multi, err := render(tmpl, v)
	if err != nil {
		t.Fatalf("render multi: %v", err)
	}
	if !strings.Contains(multi, "SELECT count()") {
		t.Fatalf("multi-address should render the subquery: %q", multi)
	}
}

// TestParseMultiAddrFlag: run() sets MultiAddr from the set size; verify the set
// sizes a single vs a two-address call parse to.
func TestParseMultiAddrFlag(t *testing.T) {
	one, _ := testTool().parse(map[string]any{"chain": "ethereum", "addresses": addrA})
	if len(one.addrs) != 1 {
		t.Fatalf("one-address set = %d", len(one.addrs))
	}
	two, _ := testTool().parse(map[string]any{"chain": "ethereum", "addresses": addrA + "," + addrB})
	if len(two.addrs) != 2 {
		t.Fatalf("two-address set = %d", len(two.addrs))
	}
}

// TestTokenKey: the EVM codec turns a contract into the 40-hex key, no 0x.
func TestTokenKey(t *testing.T) {
	c, _ := codecFor("ethereum")
	k, ok := c.tokenKey("0x" + strings.ToUpper(strings.Repeat("a", 40)))
	if !ok || k != strings.Repeat("a", 40) {
		t.Fatalf("tokenKey = %q ok=%v", k, ok)
	}
	if _, ok := c.tokenKey("nope"); ok {
		t.Fatal("tokenKey accepted an invalid id")
	}
}
