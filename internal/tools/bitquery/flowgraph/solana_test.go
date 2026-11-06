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

const (
	solA = "5tzFkiKscXHK5ZXCGbXZxdw7gTjjD1mBwuoFbhUvuAi9" // a Solana wallet (the live-verify hub)
	solB = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v" // the USDC mint, also a valid pubkey form
	solC = "So11111111111111111111111111111111111111112"  // wrapped-SOL mint (43 chars)
)

func TestSolanaCodecBasics(t *testing.T) {
	c := solanaCodec{}
	if c.chain() != "solana" {
		t.Fatalf("chain = %q", c.chain())
	}
	if !c.valid(solA) || !c.valid(solC) {
		t.Fatalf("valid rejected a Solana pubkey")
	}
	if c.valid(addrA) {
		t.Fatal("valid accepted an EVM address")
	}
	if c.valid("0OIl" + strings.Repeat("1", 30)) {
		t.Fatal("valid accepted non-base58 characters (0 O I l)")
	}
	if c.valid(strings.Repeat("1", 31)) {
		t.Fatal("valid accepted a too-short (31) address")
	}
	if c.valid(strings.Repeat("1", 45)) {
		t.Fatal("valid accepted a too-long (45) address")
	}
	// base58 is case-sensitive: normalize must not change it (unlike EVM lower-casing).
	if c.normalize(solA) != solA {
		t.Fatal("normalize changed a base58 address")
	}
	if c.supportsIncludeInternal() {
		t.Fatal("Solana must not support include_internal")
	}
	if !c.outputAddr(solA) {
		t.Fatal("outputAddr rejected a Solana pubkey")
	}
	if c.outputAddr(strings.ToLower(addrA)) {
		t.Fatal("outputAddr accepted an EVM output string")
	}
}

// TestSolanaTokenKey: the token filter is not supported on Solana (the token
// identity is the numeric currency_id, not a base58 token account), so tokenKey
// refuses every id — a base58 mint, a numeric id and junk alike.
func TestSolanaTokenKey(t *testing.T) {
	c := solanaCodec{}
	if _, ok := c.tokenKey(solB); ok {
		t.Fatal("accepted a Solana mint as a token id (token is not supported on Solana)")
	}
	if _, ok := c.tokenKey("732705"); ok {
		t.Fatal("accepted a numeric currency_id as a token id")
	}
	if _, ok := c.tokenKey("not-a-token"); ok {
		t.Fatal("accepted junk as a Solana token id")
	}
}

func TestSolanaSetLiteral(t *testing.T) {
	c := solanaCodec{}
	lit, err := c.setLiteral([]string{solA, solB})
	if err != nil {
		t.Fatalf("setLiteral: %v", err)
	}
	// The addresses are plain quoted base58 literals; the statement wraps the list as
	// IN identity([<list>]) so the constant set is not re-scanned per shard.
	if !strings.Contains(lit, "'"+solA+"'") || !strings.Contains(lit, "'"+solB+"'") {
		t.Fatalf("addresses not in the set literal: %s", lit)
	}
	if strings.Contains(lit, "base58Decode") || strings.Contains(lit, "unhex") {
		t.Fatalf("Solana addresses are read verbatim, not decoded: %s", lit)
	}
	if _, err := c.setLiteral([]string{addrA}); err == nil {
		t.Fatal("setLiteral accepted an EVM address")
	}
	if _, err := c.setLiteral(nil); err == nil {
		t.Fatal("setLiteral accepted an empty set")
	}
}

// TestSolanaLegacyCluster: Solana is a CH 20.8 cluster, so isLegacyCluster is true
// (short_circuit_function_evaluation omitted); an EVM chain is not legacy.
func TestSolanaLegacyCluster(t *testing.T) {
	c := solanaCodec{}
	if !c.legacyCluster() {
		t.Fatal("solanaCodec.legacyCluster() must be true")
	}
	if !isLegacyCluster(solanaCodec{}) {
		t.Fatal("isLegacyCluster must treat Solana as legacy (CH 20.8)")
	}
	if isLegacyCluster(evmCodec{"ethereum"}) {
		t.Fatal("isLegacyCluster must NOT treat an EVM chain as legacy")
	}
}

// TestIncludeInternalRejectedForSolana: include_internal=1 is EVM-only and refused
// on Solana (keyed on the codec); =0 is a no-op and allowed.
func TestIncludeInternalRejectedForSolana(t *testing.T) {
	if _, err := testTool().parse(map[string]any{
		"chain": "solana", "addresses": solA, "include_internal": 1,
	}); err == nil {
		t.Fatal("expected include_internal=1 on Solana to be refused")
	}
	if _, err := testTool().parse(map[string]any{
		"chain": "solana", "addresses": solA, "include_internal": 0,
	}); err != nil {
		t.Fatalf("include_internal=0 on Solana should be allowed: %v", err)
	}
}

// TestSolanaTokenParamRejected: a token param is refused on Solana at parse (tokenKey
// returns false), whatever its form.
func TestSolanaTokenParamRejected(t *testing.T) {
	if _, err := testTool().parse(map[string]any{
		"chain": "solana", "addresses": solA, "token": solB,
	}); err == nil {
		t.Fatal("expected a token filter on Solana to be refused")
	}
}

// TestSolanaCursorThroughParse: a cursor minted for a Solana query (base58
// Counterparty) parses back into req.cur on the same query, and a cursor whose
// address is an EVM form is rejected under the Solana codec.
func TestSolanaCursorThroughParse(t *testing.T) {
	params := map[string]any{"chain": "solana", "addresses": solA}
	first, err := testTool().parse(params)
	if err != nil {
		t.Fatalf("parse first: %v", err)
	}
	want := cursorKey{RowNull: 0, UsdRank: 123.45, Tsum: 9, Addr: solB}
	params["cursor"] = encodeCursor(want, bindHash(first))
	second, err := testTool().parse(params)
	if err != nil {
		t.Fatalf("parse second: %v", err)
	}
	if second.cur == nil || *second.cur != want {
		t.Fatalf("req.cur = %+v, want %+v", second.cur, want)
	}
	// An EVM-form cursor address must not decode under the Solana codec.
	evTok := encodeCursor(cursorKey{RowNull: 0, UsdRank: 1, Tsum: 1, Addr: strings.ToLower(addrA)}, bindHash(first))
	params["cursor"] = evTok
	if _, err := testTool().parse(params); err == nil {
		t.Fatal("an EVM-form cursor decoded under the Solana codec")
	}
}
