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
	"slices"
	"strings"
	"testing"
)

// labelsStmt looks up the directory over a chain list (chain IN), not one chain.
func TestLabelsStmtChainIn(t *testing.T) {
	s := labelsStmt([]string{"ethereum", "bsc"}, []string{"0xabc", "0xdef"}, 5)
	if !strings.Contains(s, "chain IN ('ethereum', 'bsc')") {
		t.Fatalf("want chain IN list, got:\n%s", s)
	}
	if !strings.Contains(s, "address IN ('0xabc', '0xdef')") {
		t.Fatalf("want address IN list, got:\n%s", s)
	}
	if strings.Contains(s, "chain =") {
		t.Fatalf("should be chain IN, not single-chain equality:\n%s", s)
	}
	// chain IS selected and grouped, so each row carries it (the shaping restricts a
	// contract to its tool chain, and the pick reports the label's chain); the pick's
	// source count stays by source name, so a repeated source is not over-counted.
	if !strings.Contains(s, "SELECT address, chain, label_type, label_value, source") {
		t.Fatalf("chain must be selected, got:\n%s", s)
	}
	if !strings.Contains(s, "GROUP BY address, chain, label_type, label_value, source") {
		t.Fatalf("GROUP BY must include chain, got:\n%s", s)
	}
}

// labelChainsFor returns the configured list, or [chain] when none is configured.
func TestLabelChainsFor(t *testing.T) {
	tool := Tool{labelChains: map[string][]string{"ethereum": {"ethereum", "bsc"}}}
	if got := tool.labelChainsFor("ethereum"); !slices.Equal(got, []string{"ethereum", "bsc"}) {
		t.Fatalf("ethereum = %v", got)
	}
	if got := tool.labelChainsFor("tron"); !slices.Equal(got, []string{"tron"}) {
		t.Fatalf("tron fallback = %v, want [tron]", got)
	}
}

// resolveLabelChains maps each statements chain to its directory list (fallback
// [chain]) and rejects a malformed directory chain name.
func TestResolveLabelChains(t *testing.T) {
	cfg := Config{
		Statements:  map[string]string{"ethereum": "x", "tron": "y"},
		LabelChains: map[string][]string{"ethereum": {"ethereum", "bsc"}},
	}
	m, err := resolveLabelChains(cfg)
	if err != nil {
		t.Fatalf("resolveLabelChains: %v", err)
	}
	if !slices.Equal(m["ethereum"], []string{"ethereum", "bsc"}) {
		t.Fatalf("ethereum = %v", m["ethereum"])
	}
	if !slices.Equal(m["tron"], []string{"tron"}) {
		t.Fatalf("tron (fallback) = %v, want [tron]", m["tron"])
	}
	bad := Config{
		Statements:  map[string]string{"ethereum": "x"},
		LabelChains: map[string][]string{"ethereum": {"Ethereum"}}, // uppercase: invalid
	}
	if _, err := resolveLabelChains(bad); err == nil {
		t.Fatalf("expected an error for a malformed directory chain name")
	}
}

// The within-type source count is by distinct SOURCE, so the same (type,value,source)
// returned on several chains (which would be several rows) counts as one source — the
// cross-chain over-count the EVM-wide lookup must avoid.
func TestPickSourceCountByDistinctSource(t *testing.T) {
	m, err := compileLabelMethod(nil, nil, nil, []string{"dex-router"}, nil)
	if err != nil {
		t.Fatalf("compileLabelMethod: %v", err)
	}
	lt, v, _, _ := m.pick([]labelRow{
		// value A: the SAME source three times (as if on three chains) -> 1 source.
		{Address: "x", Chain: "ethereum", LabelType: "dex-router", Value: "A", Source: "s1"},
		{Address: "x", Chain: "bsc", LabelType: "dex-router", Value: "A", Source: "s1"},
		{Address: "x", Chain: "base", LabelType: "dex-router", Value: "A", Source: "s1"},
		// value B: two distinct sources -> 2 sources, so B wins the source-count tier.
		{Address: "x", Chain: "ethereum", LabelType: "dex-router", Value: "B", Source: "s1"},
		{Address: "x", Chain: "ethereum", LabelType: "dex-router", Value: "B", Source: "s2"},
	})
	if lt != "dex-router" || v != "B" {
		t.Fatalf("pick = (%q,%q), want (dex-router,B) — A's repeated source must count once", lt, v)
	}
}

// restrictForContract: an EVM address that is a CONTRACT on the tool chain must NOT
// inherit another chain's same-key labels (a different owner); a plain wallet keeps
// every chain's labels.
func TestRestrictForContract(t *testing.T) {
	m, err := compileLabelMethod(nil, nil, nil, []string{"dex-router", "cex-deposit-address"}, nil)
	if err != nil {
		t.Fatalf("compileLabelMethod: %v", err)
	}
	// Contract on ethereum (dex-router) + an unrelated cex-deposit on bsc (same key,
	// different deployment). The contract address must pick dex-router from ethereum,
	// NOT the cex-deposit that belongs to a different owner on bsc.
	rows := []labelRow{
		{Address: "x", Chain: "ethereum", LabelType: "dex-router", Value: "uniswap", Source: "v"},
		{Address: "x", Chain: "bsc", LabelType: "cex-deposit-address", Value: "binance", Source: "v"},
	}
	kept := restrictForContract(rows, "ethereum")
	if len(kept) != 1 || kept[0].Chain != "ethereum" {
		t.Fatalf("restrictForContract kept %+v, want only the ethereum contract row", kept)
	}
	lt, _, chain, _ := m.pick(kept)
	if lt != "dex-router" || chain != "ethereum" {
		t.Fatalf("pick after restrict = (%q,%q), want (dex-router, ethereum) — no cross-owner merge", lt, chain)
	}
	// A WALLET (no contract-type label on the tool chain): keep both chains' labels so
	// a label on any EVM chain is found (here the cex-deposit on bsc is the only pick).
	wallet := []labelRow{
		{Address: "y", Chain: "bsc", LabelType: "cex-deposit-address", Value: "binance", Source: "v"},
	}
	if kept := restrictForContract(wallet, "ethereum"); len(kept) != 1 {
		t.Fatalf("wallet restrict dropped rows: %+v (a wallet must keep every chain)", kept)
	}
	// asset-manager is a contract-type too (it was missed in the first cut): a
	// Wintermute asset-manager contract on ethereum must not inherit a bsc exchange label.
	am := []labelRow{
		{Address: "z", Chain: "ethereum", LabelType: "asset-manager", Value: "wintermute", Source: "v"},
		{Address: "z", Chain: "bsc", LabelType: "cex-deposit-address", Value: "binance", Source: "v"},
	}
	if kept := restrictForContract(am, "ethereum"); len(kept) != 1 || kept[0].LabelType != "asset-manager" {
		t.Fatalf("asset-manager restrict kept %+v, want only the ethereum asset-manager row", kept)
	}
}
