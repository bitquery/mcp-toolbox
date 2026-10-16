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

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/googleapis/mcp-toolbox/internal/server"
	"github.com/googleapis/mcp-toolbox/internal/testutils"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/util"
)

// The whole tool, as a configuration file holds it, goes through the server's
// strict decoder: an unknown or mistyped key there refuses the whole
// configuration, so this is the check that matters before any cfg carries it.
func TestToolConfigLoads(t *testing.T) {
	ctx, err := testutils.ContextWithNewLogger()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/tool.yaml")
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, toolCfgs, _, _, err := server.UnmarshalPrimitiveConfig(ctx, raw)
	if err != nil {
		t.Fatalf("unable to unmarshal: %s", err)
	}
	tc, ok := toolCfgs["address_risk_report"]
	if !ok {
		t.Fatalf("tool not decoded: %v", toolCfgs)
	}
	chain := &fakeChain{
		assets: []map[string]any{usdtAssetRow("1000000000", "0", 1, 0)},
		edges:  []map[string]any{edgeRow("in", plainSender, "1000000000", "1000000000", "1000000000", 1, [4]string{"1", "1", "1000000000", "1000000000"}, "aa")},
	}
	lctx := tools.WithSourceLookup(ctx, tools.SourceMap{"tron-src": chain, "labels-src": &fakeLabels{}})
	tool, err := tc.Initialize(lctx)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	rt := tool.(Tool)
	if got := len(rt.StaticParameters); got != 11 {
		t.Errorf("parameters = %d, want 11", got)
	}
	if len(rt.m.order) != 26 || len(rt.m.rules) != 41 {
		t.Errorf("method: %d types, %d rules", len(rt.m.order), len(rt.m.rules))
	}
	// The profile order is the tie-break order; the server's decoder must keep it.
	if rt.m.order[0] != "Terrorism Financing" || rt.m.order[1] != "Sanctions" {
		t.Errorf("profile order lost through the server's decoder: %v", rt.m.order[:4])
	}
	// A weights override and the numeric parameters arrive with the types the
	// kind reads.
	p := mustParams(t, rt, map[string]any{"chain": "tron", "address": at5Target, "min_amount_usd": 50.0, "max_hops": 1,
		"weights": map[string]any{"weight_unattributed": 30.0}})
	res, terr := rt.Invoke(ctx, chain, p, "")
	if terr != nil {
		t.Fatalf("Invoke: %v", terr)
	}
	out := decode(t, res)
	if s := out["score"].(map[string]any)["value"].(float64); s != 30 {
		t.Errorf("weights override not applied: score %v, want 30", s)
	}
	if m := out["params"].(map[string]any)["min_amount_usd"].(float64); m != 50 {
		t.Errorf("min_amount_usd = %v", m)
	}

	// Offline flows initialize without sources and never invoke; a call is refused.
	offline, err := tc.Initialize(ctx)
	if err != nil {
		t.Fatalf("offline Initialize: %v", err)
	}
	if _, terr := offline.Invoke(ctx, chain, p, ""); terr == nil {
		t.Errorf("an offline tool answered a call")
	}
}

func TestToolConfigRefusesUnknownKey(t *testing.T) {
	ctx, err := testutils.ContextWithNewLogger()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/tool.yaml")
	if err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(string(raw), "labelsSource: labels-src\n", "labelsSource: labels-src\nlabelSource: typo\n", 1)
	if _, _, _, _, _, _, err := server.UnmarshalPrimitiveConfig(ctx, []byte(bad)); err == nil {
		t.Errorf("a misspelt key was accepted")
	}
}

// translateUTF8 throws when its two alphabets differ in length, which would
// fail every report.
func TestConfusableAlphabetsMatch(t *testing.T) {
	if a, b := utf8.RuneCountInString(confusableFrom), utf8.RuneCountInString(confusableTo); a != b {
		t.Fatalf("confusable alphabets differ: %d against %d characters", a, b)
	}
	for _, r := range confusableTo {
		if r > 127 {
			t.Fatalf("the target alphabet must be ASCII: %q", r)
		}
	}
}

// Every statement of a report carries its own billing query_id when the call
// has a caller identity.
func TestEachStatementGetsItsOwnQueryID(t *testing.T) {
	chain := &fakeChain{
		assets: []map[string]any{usdtAssetRow("1000000000", "0", 1, 0)},
		edges:  []map[string]any{edgeRow("in", plainSender, "1000000000", "1000000000", "1000000000", 1, [4]string{"1", "1", "1000000000", "1000000000"}, "aa")},
	}
	tool := newTestTool(t, chain, &fakeLabels{})
	h := http.Header{}
	h.Set("X-Bitquery-Payer-Id", "42")
	h.Set("X-Bitquery-User-Id", "42")
	ctx := util.WithBitqueryIdentity(context.Background(), h)
	if _, err := tool.Invoke(ctx, chain, mustParams(t, tool, map[string]any{"chain": "tron", "address": at5Target}), ""); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(chain.urls) != 2 {
		t.Fatalf("statements = %d, want 2", len(chain.urls))
	}
	seen := map[string]bool{}
	for _, u := range chain.urls {
		i := strings.Index(u, "query_id=")
		if i < 0 {
			t.Fatalf("no query_id on %s", u)
		}
		id := u[i:]
		if seen[id] {
			t.Errorf("query_id reused: %s", id)
		}
		seen[id] = true
	}
}
