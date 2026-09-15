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
	"fmt"
	"regexp"
	"strings"
)

// HookSource is implemented by a source that may carry the labels pre-process
// hook. The returned preprocessor is nil when the source has none configured.
type HookSource interface {
	BitqueryLabelsPreprocessor() *Preprocessor
}

var (
	defaultAddressParams = []string{"address", "addresses"}
	defaultChainParam    = "chain"
)

var evmAddressRe = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)

// addressSeparatorRe splits an address-list parameter exactly the way the
// labels_for_addresses statement does (splitByRegexp('[^0-9A-Za-z]+', …)), so the
// refresh covers the same addresses the SELECT then reads.
var addressSeparatorRe = regexp.MustCompile(`[^0-9A-Za-z]+`)

// minAddressLength drops tokens no supported chain uses as an address (EVM 42,
// Tron 34, Solana 32-44, Bitcoin 26+). A pasted list can carry words such as
// "and"; those should not cost a service lookup.
const minAddressLength = 25

// chainTagToSlug maps the chain names a caller may pass — graphql_gateway's
// network tags, the labels-query-service slugs, and the aliases the MCP label
// tools' chain transform() accepts — to the slug the service expects. An unknown
// value is passed through lowercased, letting the service resolve it best-effort.
var chainTagToSlug = map[string]string{
	"eth":                 "ethereum",
	"ethereum":            "ethereum",
	"bsc":                 "bsc",
	"bnb":                 "bsc",
	"binance smart chain": "bsc",
	"matic":               "polygon",
	"polygon":             "polygon",
	"arbitrum":            "arbitrum",
	"arb":                 "arbitrum",
	"optimism":            "optimism",
	"op":                  "optimism",
	"base":                "base",
	"avalanche":           "avalanche-c",
	"solana":              "solana",
	"sol":                 "solana",
	"tron":                "tron",
	"trx":                 "tron",
	"bitcoin":             "bitcoin",
	"btc":                 "bitcoin",
}

// supportedEvmSlugs is the set of EVM chains an EVM address with no explicit
// chain is looked up on. Ordered for deterministic requests/tests.
var supportedEvmSlugs = []string{"ethereum", "bsc", "polygon", "arbitrum", "optimism", "base"}

// Preprocessor is the query pre-process hook of a label tool: before the SQL
// runs, it refreshes the addresses named in the tool's parameters in the
// labels-query-service, so a label found there is already in directory.labels
// when the SELECT reads it.
type Preprocessor struct {
	client        Client
	addressParams []string
	chainParam    string
}

// NewPreprocessor builds a hook around a client. Empty addressParams/chainParam
// fall back to the defaults (address, addresses / chain).
func NewPreprocessor(client Client, addressParams []string, chainParam string) *Preprocessor {
	if len(addressParams) == 0 {
		addressParams = defaultAddressParams
	}
	if strings.TrimSpace(chainParam) == "" {
		chainParam = defaultChainParam
	}
	return &Preprocessor{client: client, addressParams: addressParams, chainParam: chainParam}
}

// New builds the hook for a source from its `bitqueryLabelsQueryService` block.
// It returns (nil, nil) when the block is absent or names no service, which
// leaves the source's tools reading ClickHouse directly.
func New(ctx context.Context, sourceName string, cfg *Config) (*Preprocessor, error) {
	logger := loggerFrom(ctx)
	if !cfg.Enabled() {
		if cfg != nil && logger != nil {
			logger.InfoContext(ctx, fmt.Sprintf("source %q: labels-query-service not configured, label tools read ClickHouse directly", sourceName))
		}
		return nil, nil
	}
	svc, err := NewService(*cfg)
	if err != nil {
		return nil, fmt.Errorf("source %q: %w", sourceName, err)
	}
	p := NewPreprocessor(svc, cfg.AddressParams, cfg.ChainParam)
	if logger != nil {
		logger.InfoContext(ctx, fmt.Sprintf("source %q: labels-query-service pre-process hook enabled", sourceName),
			"service", svc.service, "host", svc.host, "timeout", svc.timeout.String(),
			"addressParams", p.addressParams, "chainParam", p.chainParam)
	}
	return p, nil
}

// Preprocess runs the hook for one tool call. It is safe on a nil receiver and
// never fails: a tool whose parameters name no address makes no call, and every
// service error is swallowed by the client.
func (p *Preprocessor) Preprocess(ctx context.Context, params map[string]any) {
	if p == nil || p.client == nil {
		return
	}
	addresses := p.addresses(params)
	if len(addresses) == 0 {
		return
	}

	var chainFilters []string
	if chain, ok := params[p.chainParam].(string); ok {
		chainFilters = []string{chain}
	}
	requests := buildLabelsRequests(addresses, chainFilters)
	if logger := loggerFrom(ctx); logger != nil {
		logger.DebugContext(ctx, "labels preprocessor: refreshing addresses before ClickHouse read",
			"addresses", len(addresses), "chainFilters", chainFilters, "requests", len(requests))
	}
	for _, req := range requests {
		p.client.Refresh(ctx, req)
	}
}

// addresses collects the addresses from the configured parameters: split on
// non-alphanumerics, short tokens dropped, 0x-hex lowercased (EVM addresses are
// stored lowercase), de-duplicated in first-seen order.
func (p *Preprocessor) addresses(params map[string]any) []string {
	var raw []string
	for _, name := range p.addressParams {
		switch v := params[name].(type) {
		case string:
			raw = append(raw, v)
		case []string:
			raw = append(raw, v...)
		case []any:
			for _, item := range v {
				if s, ok := item.(string); ok {
					raw = append(raw, s)
				}
			}
		}
	}

	var out []string
	seen := make(map[string]bool)
	for _, value := range raw {
		for _, token := range addressSeparatorRe.Split(value, -1) {
			if len(token) < minAddressLength {
				continue
			}
			if strings.HasPrefix(strings.ToLower(token), "0x") {
				token = strings.ToLower(token)
			}
			if seen[token] {
				continue
			}
			seen[token] = true
			out = append(out, token)
		}
	}
	return out
}

// buildLabelsRequests turns the queried addresses and any explicit chain filters
// into service requests. With explicit chains, one request carries every address
// against those chains. Without, addresses are partitioned by format so each goes
// to its real chain(s) — Tron and bech32 Bitcoin to their single chain, EVM to
// all supported EVM chains, and any other base58 address (ambiguous between
// Solana and a legacy Bitcoin address) to both.
func buildLabelsRequests(addresses, chainFilters []string) []Request {
	if slugs := mapChains(chainFilters); len(slugs) > 0 {
		if len(slugs) == 1 {
			return []Request{{Addresses: addresses, Chain: slugs[0]}}
		}
		return []Request{{Addresses: addresses, Chains: slugs}}
	}

	var evm, tron, bitcoin, base58 []string
	for _, addr := range addresses {
		switch detectChainKind(addr) {
		case "evm":
			evm = append(evm, addr)
		case "tron":
			tron = append(tron, addr)
		case "bitcoin":
			bitcoin = append(bitcoin, addr)
		default:
			base58 = append(base58, addr)
		}
	}

	var reqs []Request
	if len(evm) > 0 {
		reqs = append(reqs, Request{Addresses: evm, Chains: supportedEvmSlugs})
	}
	if len(base58) > 0 {
		reqs = append(reqs, Request{Addresses: base58, Chains: []string{"solana", "bitcoin"}})
	}
	if len(tron) > 0 {
		reqs = append(reqs, Request{Addresses: tron, Chain: "tron"})
	}
	if len(bitcoin) > 0 {
		reqs = append(reqs, Request{Addresses: bitcoin, Chain: "bitcoin"})
	}
	return reqs
}

// mapChains maps chain filter values to service slugs, dropping empties and
// de-duplicating while keeping first-seen order.
func mapChains(chains []string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, c := range chains {
		key := strings.ToLower(strings.TrimSpace(c))
		if key == "" {
			continue
		}
		slug, ok := chainTagToSlug[key]
		if !ok {
			slug = key
		}
		if seen[slug] {
			continue
		}
		seen[slug] = true
		out = append(out, slug)
	}
	return out
}

// detectChainKind classifies an address by format: "evm" (0x + 40 hex), "tron"
// (base58, 'T' prefix, 34 chars), "bitcoin" (bech32, "bc1" prefix) or "solana"
// (any other base58 address, which may also be a legacy Bitcoin address).
func detectChainKind(addr string) string {
	switch {
	case evmAddressRe.MatchString(addr):
		return "evm"
	case len(addr) == 34 && strings.HasPrefix(addr, "T"):
		return "tron"
	case strings.HasPrefix(addr, "bc1"):
		return "bitcoin"
	default:
		return "solana"
	}
}
