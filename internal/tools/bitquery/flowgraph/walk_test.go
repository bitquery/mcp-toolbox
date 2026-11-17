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
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/googleapis/mcp-toolbox/internal/tools/bitquery/chainexec"
)

// --- test helpers -----------------------------------------------------------

// addr builds a valid lower-hex EVM address from a small integer, so a target
// passes the codec's outputAddr check (0x + 40 lower-hex).
func addr(n int) string { return fmt.Sprintf("0x%040x", n) }

func short(id string) string {
	if len(id) > 10 {
		return id[:10]
	}
	return id
}

// edge builds a priced native edge from -> to with the given USD.
func edge(from, to string, usd float64) edgeRow {
	u := usd
	return edgeRow{
		From: from, To: to, Token: "native", Symbol: "ETH",
		Amount: "1", AmountUsd: &u, UsdPricedShare: 1, Transfers: 1,
		FirstSeen: "2026-07-21T00:00:00Z", LastSeen: "2026-07-22T00:00:00Z",
	}
}

// mockFetcher serves canned outgoing edges per source, supplies each source's
// Counterparties count (counts, read POST-read for the hub decision — a source absent
// from it falls back to the distinct targets in its edge rows), and records every
// fetch with its chunkCapped flag. refuse, when set, decides a read's outcome before
// the edges are served: it returns errChunkTooBig to make the engine bisect, a stop
// error to make it stop, or nil. It models what the production execFetcher would have
// decided from the chunk/call budget, so the engine's bisect/stop/refusal-cap logic is
// exercised offline.
type mockFetcher struct {
	out    map[string][]edgeRow
	counts map[string]int
	errOn  map[string]error // a fetch containing one of these sources returns its error
	// refuse decides a read's outcome (errChunkTooBig / a stop error / nil) from its
	// sources and whether it was chunk-capped.
	refuse func(sources []string, chunkCapped bool) error
	calls  [][]string
	capped []bool // chunkCapped of each call, parallel to calls
}

func (m *mockFetcher) fetchEdges(_ context.Context, sources []string, chunkCapped bool) (fetchResult, error) {
	m.calls = append(m.calls, append([]string(nil), sources...))
	m.capped = append(m.capped, chunkCapped)
	for _, s := range sources {
		if err, ok := m.errOn[s]; ok {
			return fetchResult{}, err
		}
	}
	if m.refuse != nil {
		if err := m.refuse(sources, chunkCapped); err != nil {
			return fetchResult{}, err
		}
	}
	var edges []edgeRow
	for _, s := range sources {
		edges = append(edges, m.out[s]...)
	}
	return fetchResult{edges: edges, counts: m.counts}, nil
}

// countFetched is how many times a node appeared in a fetch, so a test can assert a
// node was expanded at most once (a hub is read once, then its edges discarded).
func (m *mockFetcher) countFetched(id string) int {
	n := 0
	for _, call := range m.calls {
		for _, s := range call {
			if s == id {
				n++
			}
		}
	}
	return n
}

// cappedFetched reports whether a node was ever fetched with the given chunkCapped
// flag (true = the chunk-capped path, false = the lone-seed call-budget path).
func (m *mockFetcher) cappedFetched(id string, capped bool) bool {
	for i, call := range m.calls {
		if m.capped[i] != capped {
			continue
		}
		for _, s := range call {
			if s == id {
				return true
			}
		}
	}
	return false
}

func emptyLabeler(_ context.Context, _ []string) (map[string][]labelRow, bool, error) {
	return map[string][]labelRow{}, false, nil
}

// baseInput wires a walkInput for a chain over a mock fetcher, with an empty
// labeler and the default walk bounds; a test overrides fields as needed.
func baseInput(t *testing.T, chain string, seeds []string, hops int, f hopFetcher) walkInput {
	t.Helper()
	codec, ok := codecFor(chain)
	if !ok {
		t.Fatalf("no codec for %q", chain)
	}
	return walkInput{
		chain: chain, codec: codec, seeds: seeds, hops: hops,
		params:      walkParamsOut{Address: seeds, Hops: hops, AfterTime: "2026-07-21T00:00:00Z"},
		dataAsOf:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		fetch:       f,
		label:       emptyLabeler,
		lm:          testLabelMethod(t),
		labelChains: []string{chain},
		noCoverage:  false,
		cfg:         walkConfig{hubN: 100, chunkSize: 100, edgeCap: defaultEdgeCap, chunkRowBudget: defaultWalkChunkRowBudget, maxRefusals: defaultWalkMaxRefusals, stopGroups: defaultStopGroups},
		stopTypes:   map[string]bool{},
		cost:        nil,
	}
}

func findNode(res *walkResult, id string) *walkNode {
	for i := range res.Nodes {
		if res.Nodes[i].ID == id {
			return &res.Nodes[i]
		}
	}
	return nil
}

// --- (a) cycle --------------------------------------------------------------

func TestWalkCycle(t *testing.T) {
	S, A, B := addr(1), addr(2), addr(3)
	mock := &mockFetcher{out: map[string][]edgeRow{
		S: {edge(S, A, 100), edge(S, A, 100)}, // duplicate (from,to,token): deduped to one
		A: {edge(A, B, 50)},
		B: {edge(B, S, 25)}, // the cycle edge, back to the seed
	}}
	in := baseInput(t, "ethereum", []string{S}, 3, mock)

	res, err := runWalkGraph(context.Background(), in)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	// Three nodes: the seed is not duplicated when B points back to it.
	if len(res.Nodes) != 3 {
		t.Fatalf("nodes = %d, want 3", len(res.Nodes))
	}
	// Three edges, the repeated S->A counted once.
	if len(res.Edges) != 3 {
		t.Fatalf("edges = %d, want 3 (dedupe on from,to,token)", len(res.Edges))
	}
	seen := map[string]bool{}
	for _, e := range res.Edges {
		k := e.From + "|" + e.To + "|" + e.Token
		if seen[k] {
			t.Errorf("duplicate edge %s", k)
		}
		seen[k] = true
	}
	// Each node expanded exactly once (fetched once).
	for _, id := range []string{S, A, B} {
		if n := mock.countFetched(id); n != 1 {
			t.Errorf("%s fetched %d times, want 1 (expand each node once)", short(id), n)
		}
	}
	// The rediscovered seed keeps hop 0 and role seed.
	if sn := findNode(res, S); sn == nil || sn.Hop != 0 || sn.Role != "seed" {
		t.Errorf("seed node = %+v, want hop 0 / seed", sn)
	}
	// B was expanded (B->S), so it is a hop node at hop 2.
	if bn := findNode(res, B); bn == nil || bn.Hop != 2 || bn.Role != "hop" {
		t.Errorf("B node = %+v, want hop 2 / hop", bn)
	}
}

// --- (b) the 500-edge cap ---------------------------------------------------

func TestWalkEdgeCap(t *testing.T) {
	S := addr(1)
	var out []edgeRow
	for i := 1; i <= 600; i++ {
		out = append(out, edge(S, addr(1000+i), float64(i))) // USD 1..600
	}
	mock := &mockFetcher{out: map[string][]edgeRow{S: out}}
	in := baseInput(t, "ethereum", []string{S}, 1, mock)
	in.cfg.hubN = 1_000_000 // seeds bypass the hub stop anyway; keep it out of the way

	res, err := runWalkGraph(context.Background(), in)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(res.Edges) != defaultEdgeCap {
		t.Fatalf("edges = %d, want the cap %d", len(res.Edges), defaultEdgeCap)
	}
	if !res.Truncated {
		t.Error("truncated must be true once the cap drops an edge")
	}
	sn := findNode(res, S)
	wantDropped := 600 - defaultEdgeCap // 100
	if sn.EdgesDropped != wantDropped {
		t.Errorf("seed edges_dropped = %d, want %d", sn.EdgesDropped, wantDropped)
	}
	// Edges are kept highest-USD first, so the dropped are USD 1..100 -> sum 5050.
	if sn.UsdDropped != 5050 {
		t.Errorf("seed usd_dropped = %v, want 5050", sn.UsdDropped)
	}
}

// TestWalkCapAcrossFrontier: the cap fills across a hop-1 frontier. The
// higher-USD node is expanded first (and partially dropped); the lower-USD node
// is left unexpanded.
func TestWalkCapAcrossFrontier(t *testing.T) {
	S, A, B := addr(1), addr(2), addr(3)
	var aOut, bOut []edgeRow
	for i := 1; i <= 50; i++ {
		aOut = append(aOut, edge(A, addr(2000+i), float64(i)))
		bOut = append(bOut, edge(B, addr(3000+i), float64(i)))
	}
	mock := &mockFetcher{out: map[string][]edgeRow{
		S: {edge(S, A, 1000), edge(S, B, 10)}, // A has more inflow, so it ranks first
		A: aOut,
		B: bOut,
	}}
	in := baseInput(t, "ethereum", []string{S}, 2, mock)
	in.cfg.hubN = 1_000_000
	in.cfg.edgeCap = 10 // 2 seed edges + 8 of A's fills it

	res, err := runWalkGraph(context.Background(), in)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if !res.Truncated {
		t.Error("truncated must be true")
	}
	an, bn := findNode(res, A), findNode(res, B)
	if an.Role != "hop" {
		t.Errorf("A role = %s, want hop (expanded before the cap filled)", an.Role)
	}
	if an.EdgesDropped == 0 {
		t.Error("A should report dropped edges (the cap filled mid-expansion)")
	}
	if bn.Role != "unexpanded" {
		t.Errorf("B role = %s, want unexpanded (the cap was full before it)", bn.Role)
	}
}

// --- (c) hub stop on a chain with NO labels ---------------------------------

func TestWalkHubStopNoLabels(t *testing.T) {
	S, A := addr(1), addr(2)
	var aOut []edgeRow
	for i := 1; i <= 5; i++ {
		aOut = append(aOut, edge(A, addr(4000+i), float64(i))) // 5 distinct targets
	}
	mock := &mockFetcher{out: map[string][]edgeRow{
		S: {edge(S, A, 100)},
		A: aOut,
	}}
	in := baseInput(t, "arc", []string{S}, 2, mock) // arc: no label coverage
	in.noCoverage = true
	in.cfg.hubN = 3
	// On a no-coverage chain the labeler must never be called.
	in.label = func(_ context.Context, _ []string) (map[string][]labelRow, bool, error) {
		t.Fatal("labeler must not be called on a no-coverage chain")
		return nil, false, nil
	}

	res, err := runWalkGraph(context.Background(), in)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	an := findNode(res, A)
	if an.Role != "stop" || an.StopReason == nil || *an.StopReason != "hub" {
		t.Fatalf("A = role %s stop_reason %v, want stop / hub (5 counterparties > N=3)", an.Role, an.StopReason)
	}
	// Stopped AT the hub: its edges are not kept and its targets are not discovered.
	if len(res.Edges) != 1 {
		t.Errorf("edges = %d, want 1 (only S->A; A's edges discarded)", len(res.Edges))
	}
	for _, n := range res.Nodes {
		if n.Hop == 2 {
			t.Errorf("no hop-2 node expected behind a hub, got %s", short(n.ID))
		}
	}
	if len(an.Labels) != 0 || an.Label != nil {
		t.Errorf("A must carry no labels on a no-coverage chain: %+v", an.Labels)
	}
}

// TestWalkHubStopPostRead: the hub is detected POST-read from the edge statement's
// Counterparties count (the same value on every row of a source), so a source capped at
// per_node that still has many counterparties is stopped after the read and its edges
// discarded. A seed is never hub-stopped, even when its own count exceeds hubN.
func TestWalkHubStopPostRead(t *testing.T) {
	S, A := addr(1), addr(2)
	mock := &mockFetcher{
		out: map[string][]edgeRow{
			S: {edge(S, A, 100)},
			A: {edge(A, addr(5001), 1), edge(A, addr(5002), 2)}, // only 2 rows (capped at per_node)
		},
		counts: map[string]int{S: 500, A: 500}, // Counterparties says 500 for both
	}
	in := baseInput(t, "ethereum", []string{S}, 2, mock)
	in.cfg.hubN = 100

	res, err := runWalkGraph(context.Background(), in)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	an := findNode(res, A)
	if an.Role != "stop" || an.StopReason == nil || *an.StopReason != "hub" {
		t.Fatalf("A must hub-stop from the count 500>100, got role %s reason %v", an.Role, an.StopReason)
	}
	if len(res.Edges) != 1 {
		t.Errorf("edges = %d, want 1 (A's rows discarded as a hub; the seed's S->A kept)", len(res.Edges))
	}
	// A is read as part of the frontier chunk (the decision is POST-read), then its
	// edges discarded and its targets not discovered.
	if mock.countFetched(A) != 1 {
		t.Errorf("A fetched %d times, want 1 (read once, then discarded as a hub)", mock.countFetched(A))
	}
	if findNode(res, addr(5001)) != nil || findNode(res, addr(5002)) != nil {
		t.Error("a hub's targets must not be discovered")
	}
	// The seed's count also exceeds hubN, but a seed is never hub-stopped.
	sn := findNode(res, S)
	if sn.Role != "seed" || sn.StopReason != nil {
		t.Errorf("seed = role %s reason %v, want seed / no stop (a seed is never hub-stopped)", sn.Role, sn.StopReason)
	}
}

// TestWalkBisectContinues: a chunk the per-chunk budget refuses (errChunkTooBig) is
// split and each half re-read; the halves that fit expand. A 158 chunk-cap refusal is a
// bisect, never a walk stop.
func TestWalkBisectContinues(t *testing.T) {
	S, A, B := addr(1), addr(2), addr(3)
	mock := &mockFetcher{out: map[string][]edgeRow{
		S: {edge(S, A, 100), edge(S, B, 90)},
		A: {edge(A, addr(9001), 10)},
		B: {edge(B, addr(9002), 10)},
	}}
	// A read carrying BOTH A and B is too big; either alone fits.
	mock.refuse = func(sources []string, chunkCapped bool) error {
		if chunkCapped && contains(sources, A) && contains(sources, B) {
			return errChunkTooBig
		}
		return nil
	}
	in := baseInput(t, "ethereum", []string{S}, 2, mock)

	res, err := runWalkGraph(context.Background(), in)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if res.budgetStopped {
		t.Error("a chunk-cap refusal must bisect, not budget-stop the walk")
	}
	for _, id := range []string{A, B} {
		if n := findNode(res, id); n == nil || n.Role != "hop" {
			t.Errorf("%s = %+v, want hop (expanded after the bisect)", short(id), n)
		}
	}
	if findNode(res, addr(9001)) == nil || findNode(res, addr(9002)) == nil {
		t.Error("both halves' targets must be discovered after the bisect")
	}
}

// TestWalkStopNotBisectOn159: a timeout (Code 159) or memory kill (241) already ran, so
// it is a walk stop, never a bisect — even though the engine would bisect on a 158. The
// engine must test errChunkTooBig before isStopError.
func TestWalkStopNotBisectOn159(t *testing.T) {
	S, A := addr(1), addr(2)
	mock := &mockFetcher{out: map[string][]edgeRow{
		S: {edge(S, A, 100)},
		A: {edge(A, addr(9001), 10)},
	}}
	mock.refuse = func(sources []string, _ bool) error {
		if contains(sources, A) {
			return fmt.Errorf("unexpected status code: 500, response body: Code: 159. DB::Exception: Timeout exceeded (TIMEOUT_EXCEEDED)")
		}
		return nil
	}
	in := baseInput(t, "ethereum", []string{S}, 2, mock)

	res, err := runWalkGraph(context.Background(), in)
	if err != nil {
		t.Fatalf("a timeout stop must not be an error: %v", err)
	}
	if !res.Partial || !res.budgetStopped {
		t.Errorf("a 159 timeout must stop the walk, got partial=%v budgetStopped=%v", res.Partial, res.budgetStopped)
	}
	if an := findNode(res, A); an == nil || an.Role != "unexpanded" {
		t.Errorf("A = %+v, want unexpanded after the timeout stop", an)
	}
	// A was read once and NOT bisected (no second, split read of A).
	if mock.countFetched(A) != 1 {
		t.Errorf("A fetched %d times, want 1 (a timeout is not bisected)", mock.countFetched(A))
	}
}

// TestWalkBisectLoneHeavySender: a single non-seed the chunk budget refuses even alone
// is left unexpanded + partial (not a hub, not a walk stop); the rest of the frontier
// still expands.
func TestWalkBisectLoneHeavySender(t *testing.T) {
	S, H, L := addr(1), addr(2), addr(3)
	mock := &mockFetcher{out: map[string][]edgeRow{
		S: {edge(S, H, 100), edge(S, L, 90)},
		H: {edge(H, addr(9001), 10)},
		L: {edge(L, addr(9002), 10)},
	}}
	// Any chunk-capped read containing H is too big — even H alone.
	mock.refuse = func(sources []string, chunkCapped bool) error {
		if chunkCapped && contains(sources, H) {
			return errChunkTooBig
		}
		return nil
	}
	in := baseInput(t, "ethereum", []string{S}, 2, mock)

	res, err := runWalkGraph(context.Background(), in)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if res.budgetStopped {
		t.Error("a too-heavy lone sender must not budget-stop the walk")
	}
	if !res.Partial {
		t.Error("a too-heavy lone sender must mark the result partial")
	}
	hn := findNode(res, H)
	if hn.Role != "unexpanded" || hn.StopReason != nil {
		t.Errorf("H = role %s reason %v, want unexpanded / no stop (too heavy, not a hub)", hn.Role, hn.StopReason)
	}
	if ln := findNode(res, L); ln == nil || ln.Role != "hop" {
		t.Errorf("L = %+v, want hop (the rest of the frontier still expands)", ln)
	}
	if findNode(res, addr(9001)) != nil {
		t.Error("the heavy sender's target must not be discovered")
	}
	if findNode(res, addr(9002)) == nil {
		t.Error("the light sender's target must be discovered")
	}
}

// TestWalkRefusalCap: once the per-call refusal cap is reached the engine stops
// bisecting and leaves the remaining senders unexpanded (a graceful partial), rather
// than splitting without end.
func TestWalkRefusalCap(t *testing.T) {
	S := addr(1)
	A, B, C, D := addr(2), addr(3), addr(4), addr(5)
	mock := &mockFetcher{out: map[string][]edgeRow{
		S: {edge(S, A, 100), edge(S, B, 99), edge(S, C, 98), edge(S, D, 97)},
		A: {edge(A, addr(9001), 1)},
		B: {edge(B, addr(9002), 1)},
		C: {edge(C, addr(9003), 1)},
		D: {edge(D, addr(9004), 1)},
	}}
	// Every chunk-capped read of a hop-1 sender refuses.
	mock.refuse = func(sources []string, chunkCapped bool) error {
		if chunkCapped && (contains(sources, A) || contains(sources, B) || contains(sources, C) || contains(sources, D)) {
			return errChunkTooBig
		}
		return nil
	}
	in := baseInput(t, "ethereum", []string{S}, 2, mock)
	in.cfg.maxRefusals = 2 // a low cap

	res, err := runWalkGraph(context.Background(), in)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if !res.Partial {
		t.Error("hitting the refusal cap must mark the result partial")
	}
	if res.budgetStopped {
		t.Error("the refusal cap is a graceful partial, not a budget stop")
	}
	for _, id := range []string{A, B, C, D} {
		if n := findNode(res, id); n == nil || n.Role != "unexpanded" {
			t.Errorf("%s = %+v, want unexpanded under the refusal cap", short(id), n)
		}
	}
	for _, n := range res.Nodes {
		if n.Hop == 2 {
			t.Errorf("no hop-2 node expected under the refusal cap, got %s", short(n.ID))
		}
	}
}

// TestWalkLoneSeedOverBudget: a lone seed the per-chunk budget refuses is re-read under
// the whole call budget (chunkCapped=false) — a seed is read even if heavy — so its
// edges are kept rather than the seed left empty.
func TestWalkLoneSeedOverBudget(t *testing.T) {
	S, A := addr(1), addr(2)
	mock := &mockFetcher{out: map[string][]edgeRow{
		S: {edge(S, A, 100)},
	}}
	// The seed refuses only on the chunk-capped read; the uncapped re-read fits.
	mock.refuse = func(sources []string, chunkCapped bool) error {
		if chunkCapped && contains(sources, S) {
			return errChunkTooBig
		}
		return nil
	}
	in := baseInput(t, "ethereum", []string{S}, 1, mock)

	res, err := runWalkGraph(context.Background(), in)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if res.budgetStopped {
		t.Error("a lone seed re-read under the call budget must not budget-stop")
	}
	if len(res.Edges) != 1 {
		t.Fatalf("edges = %d, want 1 (the seed read under the call budget)", len(res.Edges))
	}
	if !mock.cappedFetched(S, false) {
		t.Error("the lone seed must be re-read uncapped (chunkCapped=false) under the call budget")
	}
	if findNode(res, A) == nil {
		t.Error("the seed's target must be discovered")
	}
}

func contains(ss []string, x string) bool {
	for _, s := range ss {
		if s == x {
			return true
		}
	}
	return false
}

// --- partial on a budget / deadline stop ------------------------------------

// TestWalkPartialOnFetchStop: a hop-1 fetch runs out of time. The walk keeps the
// graph so far, marks partial, returns no error, and leaves the unfetched node
// unexpanded.
func TestWalkPartialOnFetchStop(t *testing.T) {
	S, A := addr(1), addr(2)
	mock := &mockFetcher{
		out:   map[string][]edgeRow{S: {edge(S, A, 100)}, A: {edge(A, addr(3), 10)}},
		errOn: map[string]error{A: chainexec.ErrNoTime}, // A's expansion fetch runs out of time
	}
	in := baseInput(t, "ethereum", []string{S}, 2, mock)

	res, err := runWalkGraph(context.Background(), in)
	if err != nil {
		t.Fatalf("a budget stop must not be an error: %v", err)
	}
	if !res.Partial {
		t.Error("partial must be true after a budget stop")
	}
	if an := findNode(res, A); an == nil || an.Role != "unexpanded" {
		t.Errorf("A = %+v, want unexpanded after the fetch stop", an)
	}
	if len(res.Edges) != 1 {
		t.Errorf("edges = %d, want 1 (S->A kept)", len(res.Edges))
	}
}

// TestWalkPartialOnLabelStop: a hop-1 frontier's label read runs out of time. The
// frontier is NOT expanded (walking through a node whose labels are unknown could
// pass an unflagged exchange); partial is set and the node is never fetched.
func TestWalkPartialOnLabelStop(t *testing.T) {
	S, A := addr(1), addr(2)
	mock := &mockFetcher{out: map[string][]edgeRow{
		S: {edge(S, A, 100)},
		A: {edge(A, addr(3), 10)},
	}}
	in := baseInput(t, "ethereum", []string{S}, 2, mock)
	in.label = func(_ context.Context, addrs []string) (map[string][]labelRow, bool, error) {
		for _, a := range addrs {
			if a == A {
				return nil, false, chainexec.ErrNoTime
			}
		}
		return map[string][]labelRow{}, false, nil
	}

	res, err := runWalkGraph(context.Background(), in)
	if err != nil {
		t.Fatalf("a label budget stop must not be an error: %v", err)
	}
	if !res.Partial {
		t.Error("partial must be true when a frontier's labels did not complete")
	}
	if an := findNode(res, A); an == nil || an.Role != "unexpanded" {
		t.Errorf("A = %+v, want unexpanded (not expanded without labels)", an)
	}
	if n := mock.countFetched(A); n != 0 {
		t.Errorf("A fetched %d times, want 0 (never expanded without its labels)", n)
	}
}

// --- a label stop + label entries -------------------------------------------

func TestWalkLabelStopAndEntries(t *testing.T) {
	S, A, B := addr(1), addr(2), addr(3)
	mock := &mockFetcher{out: map[string][]edgeRow{
		S: {edge(S, A, 100), edge(S, B, 50)},
		A: {edge(A, addr(9001), 10)}, // A would expand, but it is an exchange
		B: {edge(B, addr(9002), 10)},
	}}
	in := baseInput(t, "ethereum", []string{S}, 2, mock)
	// A is a CEX hot wallet (exchange group); B is a plain dex-router (not a stop).
	in.label = func(_ context.Context, _ []string) (map[string][]labelRow, bool, error) {
		return map[string][]labelRow{
			A: {{Address: A, Chain: "ethereum", LabelType: "cex-hot-wallet", Value: "binance", Source: "v"}},
			B: {{Address: B, Chain: "ethereum", LabelType: "dex-router", Value: "uniswap", Source: "v"}},
		}, false, nil
	}
	in.stopTypes = map[string]bool{"cex-hot-wallet": true} // the "exchange" group, selected

	res, err := runWalkGraph(context.Background(), in)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	an, bn := findNode(res, A), findNode(res, B)
	if an.Role != "stop" || an.StopReason == nil || *an.StopReason != "label" {
		t.Fatalf("A = role %s reason %v, want stop / label", an.Role, an.StopReason)
	}
	if an.Label == nil || *an.Label != "binance" {
		t.Errorf("A label = %v, want the picked value binance", an.Label)
	}
	if len(an.Labels) != 1 || an.Labels[0].Type != "cex-hot-wallet" || an.Labels[0].Sources != 1 || an.Labels[0].Heuristic {
		t.Errorf("A labels = %+v, want one cex-hot-wallet entry, 1 source, not heuristic", an.Labels)
	}
	// B is a dex-router, not a stop group: it is expanded (role hop at hop 1 < hops).
	if bn.Role != "hop" {
		t.Errorf("B role = %s, want hop (dex-router is not a stop group)", bn.Role)
	}
	// The exchange A is stopped, so its outgoing edge is not kept; B's is.
	if findNode(res, addr(9001)) != nil {
		t.Error("A's target must not be discovered behind a label stop")
	}
	if findNode(res, addr(9002)) == nil {
		t.Error("B's target should be discovered (B is not stopped)")
	}
}

// --- the production row decoder ---------------------------------------------

func TestParseEdgeRowsDecoder(t *testing.T) {
	doc := map[string]any{"data": []any{
		map[string]any{
			"From": "0xaa", "To": "0xbb", "Token": "native", "Symbol": "ETH",
			"Amount": "1.5", "Amount_Usd": "123.45", "Usd_Priced_Share": 1.0,
			"Transfers": "3", "First_Seen": "2026-07-21 00:00:00", "Last_Seen": "2026-07-22 07:00:00",
			"Counterparties": "7",
		},
		map[string]any{ // null USD, a look-alike symbol (Cyrillic Т), empty/epoch times
			"From": "0xaa", "To": "0xcc", "Token": "0xdead", "Symbol": "USDТ",
			"Amount": "1", "Amount_Usd": nil, "Usd_Priced_Share": 0.0,
			"Transfers": "1", "First_Seen": "", "Last_Seen": "1970-01-01 00:00:00",
			"Counterparties": 7.0, // same source -> same count
		},
		map[string]any{ // non-finite USD must decode to nil, or json.Marshal fails
			"From": "0xaa", "To": "0xdd", "Token": "native", "Symbol": "ETH",
			"Amount": "2", "Amount_Usd": math.NaN(), "Usd_Priced_Share": 0.0,
			"Transfers": "1", "First_Seen": "2026-01-01 00:00:00", "Last_Seen": "2026-01-01 00:00:00",
			"Counterparties": 7.0,
		},
	}}
	rows, counts, derr := parseEdgeRows(doc)
	if derr != nil {
		t.Fatalf("decode: %v", derr)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	if counts["0xaa"] != 7 {
		t.Errorf("Counterparties for 0xaa = %d, want 7 (per-source count)", counts["0xaa"])
	}
	if rows[0].AmountUsd == nil || *rows[0].AmountUsd != 123.45 {
		t.Errorf("row0 amount_usd = %v, want 123.45", rows[0].AmountUsd)
	}
	if rows[0].SymbolNonascii != 0 {
		t.Errorf("row0 symbol_nonascii = %d, want 0", rows[0].SymbolNonascii)
	}
	if rows[0].FirstSeen != "2026-07-21T00:00:00Z" {
		t.Errorf("row0 first_seen = %q, want RFC3339 Z", rows[0].FirstSeen)
	}
	if rows[0].Transfers != 3 {
		t.Errorf("row0 transfers = %d, want 3", rows[0].Transfers)
	}
	if rows[1].AmountUsd != nil {
		t.Error("row1 null amount_usd must decode to nil")
	}
	if rows[1].SymbolNonascii != 1 {
		t.Errorf("row1 symbol_nonascii = %d, want 1 (non-ASCII symbol)", rows[1].SymbolNonascii)
	}
	if rows[1].FirstSeen != "" || rows[1].LastSeen != "" {
		t.Errorf("row1 empty/epoch times must be \"\": %q / %q", rows[1].FirstSeen, rows[1].LastSeen)
	}
	if rows[2].AmountUsd != nil {
		t.Error("row2 NaN amount_usd must decode to nil")
	}
	// The whole document must marshal (a NaN would have killed this).
	if _, err := json.Marshal(rows); err != nil {
		t.Fatalf("decoded rows must marshal: %v", err)
	}
	// A row whose from/to did not decode (e.g. the statement's column names do not
	// match) is a loud error, not a silently dropped row that yields a seeds-only graph.
	bad := map[string]any{"data": []any{map[string]any{"To": "0xbb", "Token": "native", "Counterparties": "1"}}}
	if _, _, err := parseEdgeRows(bad); err == nil {
		t.Error("a row with an empty from must be a decode error, not a silent skip")
	}
	// A row with no Counterparties column is a loud error too: a missing count would
	// silently disable the hub stop (AsInt(nil)=0, never > hubN).
	noCount := map[string]any{"data": []any{map[string]any{"From": "0xaa", "To": "0xbb", "Token": "native"}}}
	if _, _, err := parseEdgeRows(noCount); err == nil {
		t.Error("a row with no Counterparties must be a decode error (the hub stop needs it)")
	}
}

// --- the output shape equals the fixture ------------------------------------

func TestWalkResultKeysMatchFixture(t *testing.T) {
	raw, err := os.ReadFile("ui/fixture-eth.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fix map[string]any
	if err := json.Unmarshal(raw, &fix); err != nil {
		t.Fatalf("fixture json: %v", err)
	}

	S, A := addr(1), addr(2)
	unpriced := edgeRow{From: A, To: addr(3), Token: "0xdead", Symbol: "X", Amount: "9", UsdPricedShare: 0, Transfers: 1}
	mock := &mockFetcher{out: map[string][]edgeRow{
		S: {edge(S, A, 100)},
		A: {unpriced},
	}}
	in := baseInput(t, "ethereum", []string{S}, 2, mock)
	// Give A a label so a label entry is present to compare.
	in.label = func(_ context.Context, _ []string) (map[string][]labelRow, bool, error) {
		return map[string][]labelRow{
			A: {{Address: A, Chain: "ethereum", LabelType: "dex-router", Value: "uniswap", Source: "v"}},
		}, false, nil
	}

	res, err := runWalkGraph(context.Background(), in)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	var got map[string]any
	b, _ := json.Marshal(res)
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("marshal result: %v", err)
	}

	assertSameKeys(t, "top-level", fix, got)
	// cost: the fixture carries a synthetic 0 and production reports rows_read; both
	// are JSON numbers, so the kinds agree. Asserted deliberately (the one key whose
	// value type we chose over the fixture's literal).
	if kf, kg := kind(fix["cost"]), kind(got["cost"]); kf != kg {
		t.Errorf("cost kind: fixture %s, got %s", kf, kg)
	}
	// The on-disk fixture now carries all nine echoed params (F6 added before_time /
	// per_node / stop_at / token), so params keys must match the walk output exactly.
	assertSameKeys(t, "params", fix["params"].(map[string]any), got["params"].(map[string]any))
	assertSameKeys(t, "node", fix["nodes"].([]any)[0].(map[string]any), got["nodes"].([]any)[0].(map[string]any))
	assertSameKeys(t, "edge", fix["edges"].([]any)[0].(map[string]any), got["edges"].([]any)[0].(map[string]any))
	assertSameKeys(t, "label entry", firstLabel(t, fix), firstLabel(t, got))
}

func keysOf(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func assertSameKeys(t *testing.T, where string, want, got map[string]any) {
	t.Helper()
	kw, kg := keysOf(want), keysOf(got)
	if fmt.Sprint(kw) != fmt.Sprint(kg) {
		t.Errorf("%s keys differ:\n fixture %v\n got     %v", where, kw, kg)
	}
}

func kind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case float64:
		return "number"
	case string:
		return "string"
	case bool:
		return "bool"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

func firstLabel(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	for _, n := range doc["nodes"].([]any) {
		labels := n.(map[string]any)["labels"].([]any)
		if len(labels) > 0 {
			return labels[0].(map[string]any)
		}
	}
	t.Fatal("no node with a label found")
	return nil
}

// --- F1: stop line names the ADDRESS, plus the seed/stop node block --------

// parseNodeBlock parses the "nodes (seeds and stops):" CSV block of a summary,
// returning each data row keyed by its address (column 0).
func parseNodeBlock(t *testing.T, summary string) map[string][]string {
	t.Helper()
	const hdr = "address,role,label,stop_reason,in_usd,out_usd"
	i := strings.Index(summary, hdr)
	if i < 0 {
		t.Fatalf("node block header not found in summary:\n%s", summary)
	}
	r := csv.NewReader(strings.NewReader(summary[i:]))
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	if err != nil {
		t.Fatalf("node block CSV must parse: %v", err)
	}
	out := map[string][]string{}
	for _, rec := range recs {
		if len(rec) == 6 && rec[0] != "address" {
			out[rec[0]] = rec
		}
	}
	return out
}

func assertNodeRow(t *testing.T, rows map[string][]string, id, role, label, reason, inUsd, outUsd string) {
	t.Helper()
	rec, ok := rows[id]
	if !ok {
		t.Errorf("node block missing row for %s", short(id))
		return
	}
	got := []string{rec[1], rec[2], rec[3], rec[4], rec[5]}
	want := []string{role, label, reason, inUsd, outUsd}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("node %s row = %v, want %v", short(id), got, want)
	}
}

func TestWalkSummaryStopAndNodeBlock(t *testing.T) {
	S, A, B, C := addr(1), addr(2), addr(3), addr(4)
	mock := &mockFetcher{
		out: map[string][]edgeRow{
			S: {edge(S, A, 100), edge(S, C, 80), edge(S, B, 50)},
			A: {edge(A, addr(201), 10)},
			C: {edge(C, addr(202), 10)},
			B: {edge(B, addr(203), 10)},
		},
		counts: map[string]int{B: 500}, // B is a hub
	}
	in := baseInput(t, "ethereum", []string{S}, 2, mock)
	// A and C are both exchanges labelled "binance" (a shared label); B is a hub.
	in.label = func(_ context.Context, _ []string) (map[string][]labelRow, bool, error) {
		return map[string][]labelRow{
			A: {{Address: A, Chain: "ethereum", LabelType: "cex-hot-wallet", Value: "binance", Source: "v"}},
			C: {{Address: C, Chain: "ethereum", LabelType: "cex-hot-wallet", Value: "binance", Source: "v"}},
		}, false, nil
	}
	in.stopTypes = map[string]bool{"cex-hot-wallet": true}

	res, err := runWalkGraph(context.Background(), in)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	s := res.summary()

	// Each stop is named by ADDRESS (+ [label] when known); two distinct nodes that
	// share a label are both listed (no dedup).
	for _, want := range []string{
		A + " [binance] (label)",
		C + " [binance] (label)",
		B + " (hub)",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("stop line missing %q\nsummary:\n%s", want, s)
		}
	}
	if strings.Contains(s, "binance (label), binance (label)") {
		t.Error("the stop line must not print the bare label (the old apparent-dup bug)")
	}

	// The node block lists the seed and every stop, by address, with correct USD.
	rows := parseNodeBlock(t, s)
	assertNodeRow(t, rows, S, "seed", "", "", "", "230") // seed out = 100+80+50
	assertNodeRow(t, rows, A, "stop", "binance", "label", "100", "")
	assertNodeRow(t, rows, C, "stop", "binance", "label", "80", "")
	assertNodeRow(t, rows, B, "stop", "", "hub", "50", "")
	if _, ok := rows[addr(201)]; ok {
		t.Error("the node block must list only seeds and stops, not hop/unexpanded targets")
	}
}

// A node's in_usd / out_usd is a Go sum of the per-edge amount_usd (each already
// 2dp from SQL); summing those floats re-introduces binary-float noise, so the
// aggregate is rounded to cents before it reaches the structured document and the
// node CSV. Three 0.1 edges sum to 0.30000000000000004 in float64; the node must
// read 0.3, not the noisy value.
func TestWalkNodeUSDRoundedToCents(t *testing.T) {
	S, A, B, C := addr(1), addr(2), addr(3), addr(4)
	mock := &mockFetcher{
		out: map[string][]edgeRow{
			S: {edge(S, A, 0.1), edge(S, B, 0.1), edge(S, C, 0.1)},
		},
	}
	in := baseInput(t, "ethereum", []string{S}, 1, mock)
	res, err := runWalkGraph(context.Background(), in)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	// Structured: the seed's out_usd is rounded to cents.
	var seed *walkNode
	for i := range res.Nodes {
		if res.Nodes[i].ID == S {
			seed = &res.Nodes[i]
		}
	}
	if seed == nil || seed.OutUsd == nil {
		t.Fatalf("seed node with out_usd not found: %+v", res.Nodes)
	}
	if got := usdCell(seed.OutUsd); got != "0.3" {
		t.Errorf("seed out_usd = %q, want %q (rounded to cents)", got, "0.3")
	}

	// Text: the node CSV carries the rounded value, and the summary never shows
	// the full-precision float.
	s := res.summary()
	if strings.Contains(s, "0.30000000000000004") {
		t.Errorf("summary shows the un-rounded float:\n%s", s)
	}
	rows := parseNodeBlock(t, s)
	assertNodeRow(t, rows, S, "seed", "", "", "", "0.3")
}

// --- F4: the edge CSV shows the TOP-N by USD, r.Edges unchanged ------------

func parseEdgeCSV(t *testing.T, s string) [][]string {
	t.Helper()
	const hdr = "from,to,token,symbol"
	i := strings.Index(s, hdr)
	if i < 0 {
		t.Fatalf("edge CSV header not found:\n%s", s)
	}
	r := csv.NewReader(strings.NewReader(s[i:]))
	r.FieldsPerRecord = -1
	recs, err := r.ReadAll()
	if err != nil {
		t.Fatalf("edge CSV must parse: %v", err)
	}
	var out [][]string
	for _, rec := range recs {
		if len(rec) >= 6 && rec[0] != "from" {
			out = append(out, rec)
		}
	}
	return out
}

func TestWalkEdgesCSVTopByUSD(t *testing.T) {
	a1 := addr(1)
	u10, u100, u50 := 10.0, 100.0, 50.0
	mk := func(to string, usd *float64) edgeRow {
		return edgeRow{From: a1, To: to, Token: "native", Symbol: "ETH", Amount: "1", AmountUsd: usd, UsdPricedShare: 1, Transfers: 1, FirstSeen: "2026-07-21T00:00:00Z", LastSeen: "2026-07-22T00:00:00Z"}
	}
	// Traversal order is deliberately NOT USD order, with the nil-USD edge FIRST.
	res := &walkResult{
		Schema: walkSchema, Chain: "eth",
		Params: walkParamsOut{Address: []string{a1}, Hops: 1},
		Edges:  []edgeRow{mk(addr(2), nil), mk(addr(3), &u10), mk(addr(4), &u100), mk(addr(5), &u50)},
	}
	wantOrder := []string{addr(2), addr(3), addr(4), addr(5)}

	out := res.edgesCSV(2) // top 2 by USD
	if !strings.Contains(out, "Showing the top 2 of 4 edges by USD") {
		t.Errorf("missing top-by-USD notice:\n%s", out)
	}
	recs := parseEdgeCSV(t, out)
	if len(recs) != 2 {
		t.Fatalf("edge CSV data rows = %d, want 2", len(recs))
	}
	if recs[0][5] != "100" || recs[1][5] != "50" {
		t.Errorf("CSV USD order = %q,%q, want 100,50 (USD desc, nulls last)", recs[0][5], recs[1][5])
	}
	for _, r := range recs {
		if r[1] == addr(2) {
			t.Error("the nil-USD edge must drop last, not appear in the top 2")
		}
	}
	// r.Edges (the structured document order) must be untouched by the text CSV.
	var gotOrder []string
	for _, e := range res.Edges {
		gotOrder = append(gotOrder, e.To)
	}
	if fmt.Sprint(gotOrder) != fmt.Sprint(wantOrder) {
		t.Errorf("edgesCSV reordered r.Edges: %v, want %v", gotOrder, wantOrder)
	}
}

// --- F6: the Params line echoes the effective request (clamp noted) --------

func TestWalkParamsLineClamped(t *testing.T) {
	res := &walkResult{
		Chain:           "eth",
		Params:          walkParamsOut{Address: []string{addr(1)}, Hops: 3, AfterTime: "2026-07-01T00:00:00Z", BeforeTime: "2026-07-21T00:00:00Z", PerNode: 10, MinUsd: 1000000, StopAt: "exchange", Token: "dac17f958d2ee523a2206206994597c13d831ec7"},
		hopsClampedFrom: 5,
		Nodes:           []walkNode{{ID: addr(1), Role: "seed"}},
	}
	s := res.summary()
	for _, want := range []string{
		"Params: hops=3 (clamped from 5)",
		"window 2026-07-01T00:00:00Z..2026-07-21T00:00:00Z",
		"per_node=10",
		"min_usd=1000000", // 'f' format, not 1e+06
		"stop_at=exchange",
		"token=dac17f958d2ee523a2206206994597c13d831ec7",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("params line missing %q\nsummary:\n%s", want, s)
		}
	}
	// No clamp -> no clamp note.
	res.hopsClampedFrom = 0
	if strings.Contains(res.summary(), "clamped from") {
		t.Error("an unclamped hops must not print a clamp note")
	}
	// Empty stop_at / token read as all / any.
	res.Params.StopAt, res.Params.Token = "", ""
	if s2 := res.summary(); !strings.Contains(s2, "stop_at=all") || !strings.Contains(s2, "token=any") {
		t.Errorf("empty stop_at/token must read all/any:\n%s", s2)
	}
}

// --- F7: an empty graph explains itself from the active filters ------------

func TestWalkEmptyGraphHint(t *testing.T) {
	base := walkResult{
		Chain:       "eth",
		Params:      walkParamsOut{Address: []string{addr(1)}, Hops: 2, AfterTime: "2026-07-01T00:00:00Z", BeforeTime: "2026-07-21T00:00:00Z", MinUsd: 1000},
		includeZero: 1,
		Nodes:       []walkNode{{ID: addr(1), Role: "seed"}},
	}
	s := base.summary()
	if !strings.Contains(s, "No outgoing transfers matched the filters") {
		t.Errorf("an empty graph must explain itself:\n%s", s)
	}
	for _, want := range []string{"window 2026-07-01T00:00:00Z..2026-07-21T00:00:00Z", "min_usd=1000", "token=any", "include_zero_value=1"} {
		if !strings.Contains(s, want) {
			t.Errorf("empty-graph hint missing %q\n%s", want, s)
		}
	}
	// A budget partial already explains itself; the hint must NOT also fire.
	p := base
	p.Partial = true
	if strings.Contains(p.summary(), "No outgoing transfers matched") {
		t.Error("a partial (budget-stopped) empty graph must not add the no-match hint")
	}
	// A non-empty graph never shows the hint.
	ne := base
	ne.Edges = []edgeRow{{From: addr(1), To: addr(2), Token: "native"}}
	if strings.Contains(ne.summary(), "No outgoing transfers matched") {
		t.Error("a non-empty graph must not show the no-match hint")
	}
}

// --- F12: a budget stop WITH edges is a partial that carries the marker ----

func TestWalkPartialMarker(t *testing.T) {
	S, A := addr(1), addr(2)
	mock := &mockFetcher{
		out:   map[string][]edgeRow{S: {edge(S, A, 100)}, A: {edge(A, addr(3), 10)}},
		errOn: map[string]error{A: chainexec.ErrNoTime}, // A's expansion runs out of time
	}
	in := baseInput(t, "ethereum", []string{S}, 2, mock)
	res, err := runWalkGraph(context.Background(), in)
	if err != nil {
		t.Fatalf("a budget stop must not be an error: %v", err)
	}
	if !res.Partial || !res.budgetStopped {
		t.Fatalf("want a budget-stopped partial, got partial=%v budgetStopped=%v", res.Partial, res.budgetStopped)
	}
	if len(res.Edges) == 0 {
		t.Fatal("this case must keep the S->A edge (a partial WITH edges)")
	}
	s := res.summary()
	if !strings.Contains(s, "Partial: stopped on the budget.") {
		t.Errorf("a budget-stopped walk's summary must carry the Partial marker:\n%s", s)
	}
	if strings.Contains(s, "No outgoing transfers matched") {
		t.Error("a partial WITH edges is not an empty graph")
	}
}
