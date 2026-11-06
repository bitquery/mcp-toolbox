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
	"testing"
)

const (
	btcP2SH   = "34xp4vRoCGJym3xR7yCVPFHoCNxv4Twseo"  // Bitfinex cold wallet (the BTC gate address)
	btcP2PKH  = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"  // genesis coinbase
	btcBech32 = "bc1qpzry9x8gf2tvdw0s3jn54khce6mua7l" // bc1 + the bech32 alphabet (form only)
	zecT1     = "t1f2zP1sPiE39kdomdfawXhssTPu8gK3Pv4" // the ZEC gate transparent address
)

func TestUTXOCodecValid(t *testing.T) {
	btc, ok := codecFor("bitcoin")
	if !ok {
		t.Fatal("no bitcoin codec")
	}
	for _, a := range []string{btcP2SH, btcP2PKH, btcBech32} {
		if !btc.valid(a) {
			t.Errorf("bitcoin rejected a valid address %q", a)
		}
	}
	// Not Bitcoin: an EVM address, a Tron base58, a Zcash transparent.
	for _, a := range []string{addrA, tronA, zecT1} {
		if btc.valid(a) {
			t.Errorf("bitcoin accepted a non-BTC address %q", a)
		}
	}
	zec, ok := codecFor("zcash")
	if !ok {
		t.Fatal("no zcash codec")
	}
	if !zec.valid(zecT1) {
		t.Errorf("zcash rejected a valid transparent address %q", zecT1)
	}
	// Shielded, unified, TEX and the zcash_flow d-<hex> placeholder are all refused.
	for _, a := range []string{
		"zs1z7rejlpsa98s2rrrfkwmaxu53e4ue0ulcrw0h4x5g8jl04tak0d3mm47vdtahatqrlkngh9sly",
		"u1l9f0l4348negsncgr9pxd9d3qaxag305pvk0jxq4x0ks7k7fy2vczjeg0d7l0z7wpzd9yk4",
		"tex1s2rt77ggv6q989lr49rkgzmh5slsksa9khdgte",
		"d-76a914abcdef",
		btcP2SH, addrA,
	} {
		if zec.valid(a) {
			t.Errorf("zcash accepted a non-transparent address %q", a)
		}
	}
}

func TestUTXONormalizeIdentity(t *testing.T) {
	btc, _ := codecFor("bitcoin")
	// base58/bech32 is case-sensitive (bech32 is lower already): normalize must not touch it.
	if btc.normalize(btcP2SH) != btcP2SH {
		t.Fatal("bitcoin normalize changed the address")
	}
}

func TestUTXOTokenKeyRefused(t *testing.T) {
	btc, _ := codecFor("bitcoin")
	if _, ok := btc.tokenKey("anything"); ok {
		t.Fatal("bitcoin tokenKey accepted a token (there is only the native coin)")
	}
	if _, ok := btc.tokenKey(btcP2SH); ok {
		t.Fatal("bitcoin tokenKey accepted an address as a token")
	}
}

func TestUTXOSetLiteral(t *testing.T) {
	btc, _ := codecFor("bitcoin")
	lit, err := btc.setLiteral([]string{btcP2SH})
	if err != nil {
		t.Fatalf("setLiteral: %v", err)
	}
	if lit != "'"+btcP2SH+"'" {
		t.Fatalf("setLiteral = %q, want the quoted address", lit)
	}
	if _, err := btc.setLiteral([]string{addrA}); err == nil {
		t.Fatal("setLiteral accepted an EVM address")
	}
	if _, err := btc.setLiteral(nil); err == nil {
		t.Fatal("setLiteral accepted an empty set")
	}
}

func TestUTXOOutputAddr(t *testing.T) {
	btc, _ := codecFor("bitcoin")
	zec, _ := codecFor("zcash")
	if !btc.outputAddr(btcP2SH) || !zec.outputAddr(zecT1) {
		t.Fatal("outputAddr rejected a chain's own output form")
	}
	if btc.outputAddr(zecT1) || zec.outputAddr(btcP2SH) {
		t.Fatal("outputAddr accepted another chain's form")
	}
}

// A UTXO chain takes one address per call for now (the per-address fan-out is not
// built): a set of more than one is refused, one is accepted.
func TestUTXOSingleAddressOnly(t *testing.T) {
	if _, err := testTool().parse(map[string]any{"chain": "bitcoin", "addresses": btcP2SH + " " + btcP2PKH}); err == nil {
		t.Fatal("expected a two-address bitcoin call to be refused")
	}
	if _, err := testTool().parse(map[string]any{"chain": "bitcoin", "addresses": btcP2SH}); err != nil {
		t.Fatalf("a single bitcoin address must be accepted: %v", err)
	}
	// The account chains still take a set.
	if _, err := testTool().parse(map[string]any{"chain": "ethereum", "addresses": addrA + " " + addrB}); err != nil {
		t.Fatalf("ethereum must still accept a set: %v", err)
	}
}

// With no after_time a UTXO chain floors the window at genesis (its spends predate the
// 90-day default); an account chain keeps the 90-day default.
func TestUTXOWindowFloorGenesis(t *testing.T) {
	btc, err := testTool().parse(map[string]any{"chain": "bitcoin", "addresses": btcP2SH})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if btc.wStart.Year() > 2010 {
		t.Fatalf("bitcoin default window start = %v, want the genesis floor (~2009)", btc.wStart)
	}
	eth, _ := testTool().parse(map[string]any{"chain": "ethereum", "addresses": addrA})
	if d := eth.wEnd.Sub(eth.wStart).Hours() / 24; d < defaultDays-1 || d > defaultDays+1 {
		t.Fatalf("ethereum default window span %.1f days, want ~%d", d, defaultDays)
	}
	// An explicit after_time is honoured on a UTXO chain (not overridden by the floor).
	exp, _ := testTool().parse(map[string]any{"chain": "bitcoin", "addresses": btcP2SH, "after_time": "2024-01-01"})
	if exp.wStart.Year() != 2024 {
		t.Fatalf("explicit after_time ignored: wStart = %v", exp.wStart)
	}
}

func TestUTXOMaxTxsClamp(t *testing.T) {
	def, _ := testTool().parse(map[string]any{"chain": "bitcoin", "addresses": btcP2SH})
	if def.maxTxs != defaultMaxTxs {
		t.Fatalf("default max_txs = %d, want %d", def.maxTxs, defaultMaxTxs)
	}
	hi, _ := testTool().parse(map[string]any{"chain": "bitcoin", "addresses": btcP2SH, "max_txs": int64(99999)})
	if hi.maxTxs != maxMaxTxs {
		t.Fatalf("max_txs high clamp = %d, want %d", hi.maxTxs, maxMaxTxs)
	}
	lo, _ := testTool().parse(map[string]any{"chain": "bitcoin", "addresses": btcP2SH, "max_txs": int64(0)})
	if lo.maxTxs != 1 {
		t.Fatalf("max_txs low clamp = %d, want 1", lo.maxTxs)
	}
}

// token and include_internal do not apply on a UTXO chain and are refused.
func TestUTXOTokenAndInternalRejected(t *testing.T) {
	if _, err := testTool().parse(map[string]any{"chain": "bitcoin", "addresses": btcP2SH, "token": btcP2PKH}); err == nil {
		t.Error("a token on bitcoin must be refused (native coin only)")
	}
	if _, err := testTool().parse(map[string]any{"chain": "bitcoin", "addresses": btcP2SH, "include_internal": int64(1)}); err == nil {
		t.Error("include_internal=1 on bitcoin must be refused")
	}
	// include_internal=0 is a no-op and allowed, so a cross-chain caller need not special-case it.
	if _, err := testTool().parse(map[string]any{"chain": "bitcoin", "addresses": btcP2SH, "include_internal": int64(0)}); err != nil {
		t.Errorf("include_internal=0 on bitcoin should be allowed: %v", err)
	}
}

func TestPickStatusThreeWay(t *testing.T) {
	if s := pickStatus("dex-router", []string{"dex-router"}); s != statusLabeled {
		t.Errorf("picked type -> %q, want labeled", s)
	}
	if s := pickStatus("", []string{"signer"}); s != statusUnlabeled {
		t.Errorf("only never-pick types -> %q, want unlabeled", s)
	}
	if s := pickStatus("", nil); s != statusNone {
		t.Errorf("no label rows -> %q, want none", s)
	}
}

// AT8: a counterparty on a chain the directory COVERS but with no row reads none;
// a chain in noCoverageChains reads no_coverage and the labels read is skipped.
func TestLabelStatusCoverageVsNone(t *testing.T) {
	tool := testTool()
	// Covered chain (bitcoin), looked up, no label row for the counterparty -> none.
	res := result{Counterparties: []cpRow{{Counterparty: btcP2SH}}}
	tool.attachLabels(request{chain: "bitcoin"}, &res, map[string][]labelRow{}, false)
	if res.Counterparties[0].LabelStatus != statusNone {
		t.Fatalf("covered chain, no row -> %q, want none", res.Counterparties[0].LabelStatus)
	}
	// No-coverage chain -> no_coverage, regardless of any (absent) rows.
	res2 := result{Counterparties: []cpRow{{Counterparty: btcP2SH}}}
	tool.attachLabels(request{chain: "dash"}, &res2, nil, true)
	if res2.Counterparties[0].LabelStatus != statusNoCoverage {
		t.Fatalf("no-coverage chain -> %q, want no_coverage", res2.Counterparties[0].LabelStatus)
	}
}

// The noCoverageChain set is read case-insensitively from cfg.
func TestNoCoverageChainMembership(t *testing.T) {
	tool := testTool() // noCoverage {dash, arc}
	if !tool.noCoverageChain("dash") || !tool.noCoverageChain("ARC") {
		t.Fatal("dash/arc must be no-coverage")
	}
	if tool.noCoverageChain("bitcoin") || tool.noCoverageChain("zcash") {
		t.Fatal("bitcoin/zcash must NOT be no-coverage (they resolve to none per counterparty)")
	}
}

// A base58 Bitcoin Counterparty survives normalize unchanged (it is the labels key).
func TestUTXONormalizeThroughNormalizerFor(t *testing.T) {
	if got := normalizerFor("bitcoin")(btcP2SH); got != btcP2SH {
		t.Fatalf("normalizerFor(bitcoin) changed %q to %q", btcP2SH, got)
	}
	if got := normalizerFor("zcash")(zecT1); got != zecT1 {
		t.Fatalf("normalizerFor(zcash) changed %q to %q", zecT1, got)
	}
}
