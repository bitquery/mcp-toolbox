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

package bitquerylabels

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
)

const (
	evmAddr       = "0x28c6c06298d514db089934071355e5743bf21d60"
	evmAddr2      = "0x21a31ee1afc51d94c2efccaa2092ad1028285549"
	evmChecksum   = "0x28C6c06298d514Db089934071355E5743bf21d60"
	tronAddr      = "TNXoiAJ3dct8Fjg4M9fkLFh9S2v9TXc32G"
	solanaAddr    = "5Q544fKrFoe6tsEbD7S8EmxGTJYAKtTVhAW5Q5pge4j1"
	bech32Addr    = "bc1qm34lsc65zpw79lxes69zkqmk6ee3ewf0j77s3h"
	legacyBtcAddr = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"
)

type recordingClient struct {
	requests []Request
}

func (c *recordingClient) Refresh(_ context.Context, req Request) {
	c.requests = append(c.requests, req)
}

func TestBuildLabelsRequestsAutoDetect(t *testing.T) {
	got := buildLabelsRequests([]string{evmAddr, evmAddr2, tronAddr, solanaAddr, bech32Addr}, nil)
	want := []Request{
		{Addresses: []string{evmAddr, evmAddr2}, Chains: supportedEvmSlugs},
		{Addresses: []string{solanaAddr}, Chains: []string{"solana", "bitcoin"}},
		{Addresses: []string{tronAddr}, Chain: "tron"},
		{Addresses: []string{bech32Addr}, Chain: "bitcoin"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("mismatch (-want +got):\n%s", diff)
	}
}

// A legacy Bitcoin address is base58 like Solana: with no chain it goes to both.
func TestBuildLabelsRequestsLegacyBitcoinAmbiguous(t *testing.T) {
	got := buildLabelsRequests([]string{legacyBtcAddr}, nil)
	want := []Request{{Addresses: []string{legacyBtcAddr}, Chains: []string{"solana", "bitcoin"}}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("mismatch (-want +got):\n%s", diff)
	}
}

// Every alias the label tools' chain transform() accepts resolves to the slug
// the service expects; an unknown chain passes through lowercased.
func TestBuildLabelsRequestsExplicitChain(t *testing.T) {
	cases := map[string]string{
		"eth": "ethereum", "Ethereum": "ethereum",
		"bsc": "bsc", "bnb": "bsc", "Binance Smart Chain": "bsc",
		"matic": "polygon", "Polygon": "polygon",
		"arb": "arbitrum", "op": "optimism", "base": "base",
		"trx": "tron", "Tron": "tron",
		"sol": "solana", "btc": "bitcoin",
		"Robinhood": "robinhood",
	}
	for chain, slug := range cases {
		got := buildLabelsRequests([]string{legacyBtcAddr}, []string{chain})
		want := []Request{{Addresses: []string{legacyBtcAddr}, Chain: slug}}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("chain %q: mismatch (-want +got):\n%s", chain, diff)
		}
	}
}

func TestBuildLabelsRequestsExplicitMultiChain(t *testing.T) {
	got := buildLabelsRequests([]string{evmAddr}, []string{"eth", "matic", "ethereum", "base", " "})
	want := []Request{{Addresses: []string{evmAddr}, Chains: []string{"ethereum", "polygon", "base"}}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("mismatch (-want +got):\n%s", diff)
	}
}

// labels_for_addresses takes a free-form list: split like the SQL does, drop
// words, lowercase EVM, de-duplicate.
func TestPreprocessSplitsAddressList(t *testing.T) {
	client := &recordingClient{}
	p := NewPreprocessor(client, nil, "")
	p.Preprocess(context.Background(), map[string]any{
		"addresses": evmChecksum + ", " + evmAddr + " | " + evmAddr2 + "\nand " + tronAddr,
		"chain":     "",
		"limit":     200,
	})
	want := []Request{
		{Addresses: []string{evmAddr, evmAddr2}, Chains: supportedEvmSlugs},
		{Addresses: []string{tronAddr}, Chain: "tron"},
	}
	if diff := cmp.Diff(want, client.requests); diff != "" {
		t.Fatalf("mismatch (-want +got):\n%s", diff)
	}
}

// address_labels takes one address and an optional chain.
func TestPreprocessSingleAddressWithChain(t *testing.T) {
	client := &recordingClient{}
	p := NewPreprocessor(client, nil, "")
	p.Preprocess(context.Background(), map[string]any{"address": evmChecksum, "chain": "Matic"})
	want := []Request{{Addresses: []string{evmAddr}, Chain: "polygon"}}
	if diff := cmp.Diff(want, client.requests); diff != "" {
		t.Fatalf("mismatch (-want +got):\n%s", diff)
	}
}

// find_label_values / addresses_by_label carry no address: no call.
func TestPreprocessNoAddressNoCall(t *testing.T) {
	client := &recordingClient{}
	p := NewPreprocessor(client, nil, "")
	p.Preprocess(context.Background(), map[string]any{"query": "binance", "label_type": "", "chain": "ethereum"})
	p.Preprocess(context.Background(), map[string]any{"address": "   ", "chain": ""})
	if len(client.requests) != 0 {
		t.Fatalf("requests = %v, want none", client.requests)
	}
}

func TestPreprocessCustomParamNames(t *testing.T) {
	client := &recordingClient{}
	p := NewPreprocessor(client, []string{"wallet"}, "network")
	p.Preprocess(context.Background(), map[string]any{"wallet": tronAddr, "address": evmAddr, "network": "trx"})
	want := []Request{{Addresses: []string{tronAddr}, Chain: "tron"}}
	if diff := cmp.Diff(want, client.requests); diff != "" {
		t.Fatalf("mismatch (-want +got):\n%s", diff)
	}
}

func TestPreprocessNilSafe(t *testing.T) {
	var p *Preprocessor
	p.Preprocess(context.Background(), map[string]any{"address": evmAddr})
}

func TestNewDisabled(t *testing.T) {
	for _, cfg := range []*Config{nil, {}, {Service: "  ", TimeoutSec: 5}} {
		p, err := New(context.Background(), "src", cfg)
		if p != nil || err != nil {
			t.Errorf("New(%+v) = %v, %v; want nil, nil", cfg, p, err)
		}
	}
	p, err := New(context.Background(), "src", &Config{Host: "127.0.0.1:1", AddressParams: []string{"a"}, ChainParam: "c"})
	if err != nil || p == nil {
		t.Fatalf("New(enabled) = %v, %v", p, err)
	}
	if diff := cmp.Diff([]string{"a"}, p.addressParams); diff != "" || p.chainParam != "c" {
		t.Errorf("params not applied: %v %q", p.addressParams, p.chainParam)
	}
	if _, err := New(context.Background(), "src", &Config{Host: "h:1", SrvTtlSec: -1}); err == nil {
		t.Errorf("an invalid enabled config must be rejected")
	}
}
