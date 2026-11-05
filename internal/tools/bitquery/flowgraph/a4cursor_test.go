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
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/tools/bitquery/chainexec"
)

func encode64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// cpBind is the bind hash of a minimal address-mode request, for the encode /
// decode tests.
func cpReq() request {
	req, err := testTool().parse(map[string]any{"chain": "ethereum", "addresses": addrA})
	if err != nil {
		panic(err)
	}
	return req
}

func TestCursorRoundTrip(t *testing.T) {
	req := cpReq()
	bind := bindHash(req)
	k := cursorKey{RowNull: 0, UsdRank: 12345.67, Tsum: 42, Addr: strings.ToLower(addrA)}
	tok := encodeCursor(k, bind)
	// The token is base64url (the cfg allowedValues) — no '+', '/', '='.
	if strings.ContainsAny(tok, "+/=") {
		t.Fatalf("cursor is not base64url-clean: %q", tok)
	}
	got, err := decodeCursor(tok, bind)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != k {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, k)
	}
}

func TestCursorRejects(t *testing.T) {
	req := cpReq()
	bind := bindHash(req)
	good := encodeCursor(cursorKey{RowNull: 1, UsdRank: 0, Tsum: 3, Addr: strings.ToLower(addrA)}, bind)

	cases := map[string]func() (string, string){
		"wrong bind":   func() (string, string) { return good, "a-different-bind" },
		"garbage b64":  func() (string, string) { return "!!!not base64!!!", bind },
		"not json":     func() (string, string) { return encode64("hello"), bind },
		"bad version":  func() (string, string) { return encode64(`{"v":99,"h":"` + bind + `","k":{"r":0,"u":0,"t":0,"a":"` + strings.ToLower(addrA) + `"}}`), bind },
		"bad rownull":  func() (string, string) { return encode64(`{"v":1,"h":"` + bind + `","k":{"r":5,"u":0,"t":0,"a":"` + strings.ToLower(addrA) + `"}}`), bind },
		"bad addr":     func() (string, string) { return encode64(`{"v":1,"h":"` + bind + `","k":{"r":0,"u":0,"t":0,"a":"not-an-address"}}`), bind },
		"negative t":   func() (string, string) { return encode64(`{"v":1,"h":"` + bind + `","k":{"r":0,"u":0,"t":-1,"a":"` + strings.ToLower(addrA) + `"}}`), bind },
		"negative usd": func() (string, string) { return encode64(`{"v":1,"h":"` + bind + `","k":{"r":0,"u":-5,"t":0,"a":"` + strings.ToLower(addrA) + `"}}`), bind },
	}
	for name, mk := range cases {
		tok, b := mk()
		if _, err := decodeCursor(tok, b); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// TestBindHashSensitivity: anything that changes which counterparties are
// returned, or their order, changes the bind hash, so a cursor cannot be
// replayed against a different query.
func TestBindHashSensitivity(t *testing.T) {
	base := cpReq()
	h := bindHash(base)
	variants := map[string]func(r *request){
		"window":     func(r *request) { r.wEnd = r.wEnd.Add(time.Hour) },
		"direction":  func(r *request) { r.direction = "in" },
		"token":      func(r *request) { r.token = strings.Repeat("a", 40) },
		"internal":   func(r *request) { r.includeInternal = 1 },
		"zero":       func(r *request) { r.includeZero = 1 },
		"min_usd":    func(r *request) { r.minUsd = 10 },
		"group_by":   func(r *request) { r.groupBy = "label_type" },
		"label":      func(r *request) { r.labelTypes = []string{"mixer"} },
		"excl_label": func(r *request) { r.excludeLabelTypes = []string{"mixer"} },
		"addr":       func(r *request) { r.addrs = []string{strings.ToLower(addrB)} },
	}
	for name, mut := range variants {
		r := base
		mut(&r)
		if bindHash(r) == h {
			t.Errorf("%s: bind hash did not change", name)
		}
	}
	// limit and format are NOT bound (page size / rendering may vary mid-walk).
	r := base
	r.limit = 7
	r.format = "json"
	if bindHash(r) != h {
		t.Error("limit/format should not change the bind hash")
	}
}

// TestParseCursorRejectedForLabelType: a cursor with group_by=label_type is
// refused before any decode (that mode has no deep paging).
func TestParseCursorRejectedForLabelType(t *testing.T) {
	if _, err := testTool().parse(map[string]any{
		"chain": "ethereum", "addresses": addrA, "group_by": "label_type", "cursor": "anything",
	}); err == nil {
		t.Fatal("expected label_type + cursor to be refused")
	}
}

// TestParseCursorMismatchRejected: a cursor minted for one query is rejected by
// another (different address set).
func TestParseCursorMismatchRejected(t *testing.T) {
	reqA := cpReq()
	tok := encodeCursor(cursorKey{RowNull: 0, UsdRank: 1, Tsum: 1, Addr: strings.ToLower(addrA)}, bindHash(reqA))
	if _, err := testTool().parse(map[string]any{
		"chain": "ethereum", "addresses": addrB, "cursor": tok,
	}); err == nil {
		t.Fatal("expected a mismatched cursor to be refused")
	}
}

// TestParseCursorRoundTripThroughParse: a cursor minted for a query parses back
// into req.cur on the same query.
func TestParseCursorRoundTripThroughParse(t *testing.T) {
	params := map[string]any{"chain": "ethereum", "addresses": addrA, "after_time": "2026-08-01", "before_time": "2026-11-01"}
	first, err := testTool().parse(params)
	if err != nil {
		t.Fatalf("parse first: %v", err)
	}
	want := cursorKey{RowNull: 0, UsdRank: 999.99, Tsum: 5, Addr: strings.ToLower(addrA)}
	params["cursor"] = encodeCursor(want, bindHash(first))
	second, err := testTool().parse(params)
	if err != nil {
		t.Fatalf("parse second: %v", err)
	}
	if second.cur == nil || *second.cur != want {
		t.Fatalf("req.cur = %+v, want %+v", second.cur, want)
	}
}

// havingOrderTmpl mirrors the real statement's HAVING + ORDER BY so the render
// of the keyset predicate is tested without a live DB (same technique as
// TestRenderArmsAndLiterals). It must stay in step with cfg/counterparties.yaml.
const havingOrderTmpl = `HAVING ({{.MinUsd}} = 0 OR Usd_Total >= {{.MinUsd}}){{if .Cursor}}
  AND (row_null, -Usd_Rank, -toInt64(Transfers_In + Transfers_Out), Counterparty) > ({{.CurRowNull}}, -{{.CurUsd}}, -{{.CurTsum}}, '{{.CurAddr}}'){{end}}
ORDER BY row_null ASC, Usd_Rank DESC, (Transfers_In + Transfers_Out) DESC, Counterparty
LIMIT {{.TopK}}`

func renderHO(v stmtVars) string {
	tmpl := template.Must(template.New("ho").Option("missingkey=error").Parse(havingOrderTmpl))
	out, err := render(tmpl, v)
	if err != nil {
		panic(err)
	}
	return out
}

// TestPageOneHasNoPredicate: with no cursor, the HAVING carries only min_usd —
// page one is the same statement as before the cursor existed.
func TestPageOneHasNoPredicate(t *testing.T) {
	out := renderHO(stmtVars{MinUsd: "0", TopK: 100})
	if strings.Contains(out, ">") && strings.Contains(out, "CurAddr") {
		t.Fatalf("page one leaked a predicate: %q", out)
	}
	if strings.Contains(out, "row_null, -Usd_Rank") {
		t.Fatalf("page one rendered the keyset predicate: %q", out)
	}
}

// TestKeysetExpressionsMatchOrderBy: the ranking expressions in the predicate
// are byte-identical to the ORDER BY's (a drift silently skips or repeats rows),
// and the predicate ranks on the cent-quantized Usd_Rank, never the raw NULL-able
// Usd_Total (which would drop the whole unpriced tail on page two).
func TestKeysetExpressionsMatchOrderBy(t *testing.T) {
	out := renderHO(stmtVars{MinUsd: "0", TopK: 100, Cursor: true, CurRowNull: 0, CurUsd: "100.5", CurTsum: 7, CurAddr: strings.ToLower(addrA)})
	predLine, orderLine := "", ""
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "AND (row_null") {
			predLine = ln
		}
		if strings.HasPrefix(strings.TrimSpace(ln), "ORDER BY") {
			orderLine = ln
		}
	}
	if predLine == "" || orderLine == "" {
		t.Fatalf("missing predicate or order line:\n%s", out)
	}
	for _, expr := range []string{"row_null", "Usd_Rank", "(Transfers_In + Transfers_Out)"} {
		if !strings.Contains(predLine, expr) {
			t.Errorf("predicate missing %q: %s", expr, predLine)
		}
		if !strings.Contains(orderLine, expr) {
			t.Errorf("order by missing %q: %s", expr, orderLine)
		}
	}
	// The predicate must rank on Usd_Rank (coalesced, cent-quantized), never the
	// NULL-able Usd_Total — a NULL in the tuple drops the row.
	if strings.Contains(predLine, "Usd_Total") {
		t.Errorf("predicate uses the NULL-able Usd_Total, not Usd_Rank: %s", predLine)
	}
	// The cursor literals substituted.
	if !strings.Contains(predLine, "-100.5") || !strings.Contains(predLine, "-7") || !strings.Contains(predLine, "'"+strings.ToLower(addrA)+"'") {
		t.Errorf("cursor literals not substituted: %s", predLine)
	}
}

func TestIsStopError(t *testing.T) {
	ctx := context.Background()
	stop := []error{
		context.DeadlineExceeded,
		fmt.Errorf("DB::Exception: ... Code: 159. TIMEOUT_EXCEEDED"),
		fmt.Errorf("Code: 241. DB::Exception: Memory limit (total) exceeded: MEMORY_LIMIT_EXCEEDED"),
		fmt.Errorf("Code: 158. Limit for rows or bytes to read exceeded"),
	}
	for _, e := range stop {
		if !isStopError(ctx, e) {
			t.Errorf("isStopError(%v) = false, want true", e)
		}
	}
	notStop := []error{
		fmt.Errorf("Code: 62. Syntax error"),
		fmt.Errorf("connection refused"),
		nil,
	}
	for _, e := range notStop {
		if isStopError(ctx, e) {
			t.Errorf("isStopError(%v) = true, want false", e)
		}
	}
}

// TestPartialResult: a budget stop returns Partial=1, a hint, no counterparties
// and no cursor, in both formats and both group_by modes.
func TestPartialResult(t *testing.T) {
	exec := &chainexec.Exec{}

	req := cpReq()
	req.format = "json"
	res, ok := testTool().partialResult(req, exec, time.Now()).(result)
	if !ok {
		t.Fatalf("address json partial is not a result")
	}
	if !res.Partial || res.Hint == "" || res.NextCursor != "" || len(res.Counterparties) != 0 {
		t.Fatalf("address partial wrong: %+v", res)
	}

	req.format = "markdown"
	md, ok := testTool().partialResult(req, exec, time.Now()).(tools.TextResult)
	if !ok || !strings.Contains(md.Text, "Partial") {
		t.Fatalf("address markdown partial wrong: %+v", md)
	}

	// label_type mode, json.
	lt := cpReq()
	lt.groupBy = "label_type"
	lt.format = "json"
	tr, ok := testTool().partialResult(lt, exec, time.Now()).(typeResult)
	if !ok || !tr.Partial || tr.Hint == "" {
		t.Fatalf("label_type partial wrong: %+v", tr)
	}
}
