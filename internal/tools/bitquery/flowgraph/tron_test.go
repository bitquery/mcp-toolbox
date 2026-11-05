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
	tronA = "TDDaCrpQueZmPZBVouNbZqyZFS3Cnx4QL9" // the AT10 hub
	tronB = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t" // USDT TRC-20 contract (also a valid T-address)
)

func TestTronCodecBasics(t *testing.T) {
	c := tronCodec{}
	if c.chain() != "tron" {
		t.Fatalf("chain = %q", c.chain())
	}
	if !c.valid(tronA) {
		t.Fatalf("valid rejected %s", tronA)
	}
	if c.valid(addrA) {
		t.Fatal("valid accepted an EVM address")
	}
	if c.valid("T"+strings.Repeat("1", 40)) {
		t.Fatal("valid accepted a wrong-length address")
	}
	// base58 is case-sensitive: normalize must not change it (unlike EVM lower-casing).
	if c.normalize(tronA) != tronA {
		t.Fatal("normalize changed a base58 address")
	}
	if c.supportsIncludeInternal() {
		t.Fatal("Tron must not support include_internal")
	}
	if !c.outputAddr(tronA) {
		t.Fatal("outputAddr rejected a T-address")
	}
	if c.outputAddr(strings.ToLower(addrA)) {
		t.Fatal("outputAddr accepted an EVM output string")
	}
}

func TestTronTokenKey(t *testing.T) {
	c := tronCodec{}
	if k, ok := c.tokenKey(tronB); !ok || k != tronB {
		t.Fatalf("TRC-20 contract: key=%q ok=%v", k, ok)
	}
	if k, ok := c.tokenKey("1002000"); !ok || k != "1002000" {
		t.Fatalf("TRC-10 id: key=%q ok=%v", k, ok)
	}
	if _, ok := c.tokenKey(addrA); ok {
		t.Fatal("accepted an EVM contract as a Tron token")
	}
	if _, ok := c.tokenKey("not-a-token"); ok {
		t.Fatal("accepted junk as a Tron token")
	}
}

func TestTronSetLiteral(t *testing.T) {
	c := tronCodec{}
	lit, err := c.setLiteral([]string{tronA, tronB})
	if err != nil {
		t.Fatalf("setLiteral: %v", err)
	}
	// The addresses are quoted literals; the base58 decode + 4-byte checksum
	// verification happen IN SQL (tryBase58Decode idiom), and the result is cast to
	// FixedString(20) so the statement reads it exactly as the EVM set.
	if !strings.Contains(lit, "'"+tronA+"'") || !strings.Contains(lit, "'"+tronB+"'") {
		t.Fatalf("addresses not in the set literal: %s", lit)
	}
	if !strings.Contains(lit, "tryBase58Decode") || !strings.Contains(lit, "toFixedString(x, 20)") {
		t.Fatalf("set literal is not the checksum-verified form: %s", lit)
	}
	if _, err := c.setLiteral([]string{addrA}); err == nil {
		t.Fatal("setLiteral accepted an EVM address")
	}
}

func TestOutputAddrPerChain(t *testing.T) {
	ev := evmCodec{"ethereum"}
	tr := tronCodec{}
	if !ev.outputAddr(strings.ToLower(addrA)) {
		t.Fatal("evm rejected its own output form")
	}
	if ev.outputAddr(tronA) {
		t.Fatal("evm accepted a Tron address")
	}
	if !tr.outputAddr(tronA) {
		t.Fatal("tron rejected its own output form")
	}
	if tr.outputAddr(strings.ToLower(addrA)) {
		t.Fatal("tron accepted an EVM output")
	}
}

// TestCursorCrossChainReject: a cursor whose address is one chain's form is
// rejected when decoded under the other chain's codec. The bind is identical, so
// it is the per-codec outputAddr check doing the rejecting (the cursorVersion-2
// generalization).
func TestCursorCrossChainReject(t *testing.T) {
	const bind = "somebind"
	evTok := encodeCursor(cursorKey{RowNull: 0, UsdRank: 1, Tsum: 1, Addr: strings.ToLower(addrA)}, bind)
	trTok := encodeCursor(cursorKey{RowNull: 0, UsdRank: 1, Tsum: 1, Addr: tronA}, bind)
	if _, err := decodeCursor(evTok, bind, tronCodec{}); err == nil {
		t.Fatal("an EVM cursor decoded under the Tron codec")
	}
	if _, err := decodeCursor(trTok, bind, evmCodec{"ethereum"}); err == nil {
		t.Fatal("a Tron cursor decoded under the EVM codec")
	}
	if _, err := decodeCursor(trTok, bind, tronCodec{}); err != nil {
		t.Fatalf("a Tron cursor under the Tron codec: %v", err)
	}
	if _, err := decodeCursor(evTok, bind, evmCodec{"ethereum"}); err != nil {
		t.Fatalf("an EVM cursor under the EVM codec: %v", err)
	}
}

// TestTronCursorThroughParse: a cursor minted for a Tron query (base58
// Counterparty) parses back into req.cur on the same query.
func TestTronCursorThroughParse(t *testing.T) {
	params := map[string]any{"chain": "tron", "addresses": tronA}
	first, err := testTool().parse(params)
	if err != nil {
		t.Fatalf("parse first: %v", err)
	}
	want := cursorKey{RowNull: 0, UsdRank: 123.45, Tsum: 9, Addr: tronB}
	params["cursor"] = encodeCursor(want, bindHash(first))
	second, err := testTool().parse(params)
	if err != nil {
		t.Fatalf("parse second: %v", err)
	}
	if second.cur == nil || *second.cur != want {
		t.Fatalf("req.cur = %+v, want %+v", second.cur, want)
	}
}

// TestIncludeInternalRejectedForTron: include_internal=1 is EVM-only and refused
// on Tron (keyed on the codec); =0 is a no-op and allowed; EVM still accepts 1.
func TestIncludeInternalRejectedForTron(t *testing.T) {
	if _, err := testTool().parse(map[string]any{
		"chain": "tron", "addresses": tronA, "include_internal": 1,
	}); err == nil {
		t.Fatal("expected include_internal=1 on Tron to be refused")
	}
	if _, err := testTool().parse(map[string]any{
		"chain": "tron", "addresses": tronA, "include_internal": 0,
	}); err != nil {
		t.Fatalf("include_internal=0 on Tron should be allowed: %v", err)
	}
	if _, err := testTool().parse(map[string]any{
		"chain": "ethereum", "addresses": addrA, "include_internal": 1,
	}); err != nil {
		t.Fatalf("include_internal=1 on ethereum should be allowed: %v", err)
	}
}
