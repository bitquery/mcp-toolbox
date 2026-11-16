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

// The multi-hop WALK (flow_graph), an ADDITIVE path beside the hop-1
// counterparties run() in engine.go. Nothing here is reached unless the tool is
// configured walk: true; counterparties is untouched.
//
// The walk is breadth-first from 1-10 seeds, up to 3 hops, following OUTGOING
// money. Each node is expanded at most once, at its shortest distance from a
// seed; edges are unique on (from, to, token). Before a frontier is expanded its
// labels are read, and a node is stopped at a stop_at label group, at a hub
// (more than N counterparties), or at the global 500-edge cap (which sets
// truncated). A budget or deadline stop sets partial. The result is a
// flow_graph/1 document (the shape the in-chat app reads), the same shape as
// internal/tools/bitquery/flowgraph/ui/fixture-eth.json.
//
// The per-hop edge fetch and the label read are injected (hopFetcher, labelFetch)
// so the engine is exercised offline with mock rows; the production wiring (the
// batched flow_edges statement through chainexec, and t.fetchLabels) lives in
// walk_exec.go.

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/googleapis/mcp-toolbox/internal/tools/bitquery/chainexec"
)

const walkSchema = "flow_graph/1"

// edgeRow is one directed edge of the graph, from -> to in one token, already
// decoded and valued. It marshals to the edge object of a flow_graph/1 document.
// amount_usd is null (never 0) when the token has no priced transfer on the edge.
type edgeRow struct {
	From           string   `json:"from"`
	To             string   `json:"to"`
	Token          string   `json:"token"`
	Symbol         string   `json:"symbol"`
	SymbolNonascii int      `json:"symbol_nonascii"`
	Amount         string   `json:"amount"`
	AmountUsd      *float64 `json:"amount_usd"`
	UsdPricedShare float64  `json:"usd_priced_share"`
	Transfers      int64    `json:"transfers"`
	FirstSeen      string   `json:"first_seen"`
	LastSeen       string   `json:"last_seen"`
}

// labelEntry is one label type a node carries, with its chosen value, how many
// distinct sources back it, and whether its most-trusted source is heuristic.
type labelEntry struct {
	Type      string `json:"type"`
	Value     string `json:"value"`
	Sources   int    `json:"sources"`
	Heuristic bool   `json:"heuristic"`
}

// walkNode is one node of the graph. label is the picked primary label's value
// (null when none); labels lists every distinct type the node carries. role is
// seed | hop | stop | unexpanded; stop_reason is label | hub (null otherwise).
// in_usd / out_usd are the summed priced USD of the node's kept incoming /
// outgoing edges (null when none priced). edges_dropped / usd_dropped are what
// the edge cap dropped at this node.
type walkNode struct {
	ID           string       `json:"id"`
	Label        *string      `json:"label"`
	Labels       []labelEntry `json:"labels"`
	Role         string       `json:"role"`
	StopReason   *string      `json:"stop_reason"`
	Hop          int          `json:"hop"`
	InUsd        *float64     `json:"in_usd"`
	OutUsd       *float64     `json:"out_usd"`
	Status       string       `json:"status"`
	EdgesDropped int          `json:"edges_dropped"`
	UsdDropped   float64      `json:"usd_dropped"`
}

// walkParamsOut is the echoed request: the params the fixture carries and the app's
// Expand re-sends (Expand re-sends every params field except address and hops).
// after_time / before_time are RFC3339 so the window is not shifted by date
// truncation. per_node, stop_at and token echo the EFFECTIVE (post-default,
// post-clamp) request, so a caller sees what actually ran rather than what it asked.
type walkParamsOut struct {
	Address         []string `json:"address"`
	Hops            int      `json:"hops"`
	AfterTime       string   `json:"after_time"`
	BeforeTime      string   `json:"before_time"`
	MinUsd          float64  `json:"min_usd"`
	IncludeInternal int      `json:"include_internal"`
	PerNode         int      `json:"per_node"`
	StopAt          string   `json:"stop_at"`
	Token           string   `json:"token"`
}

// walkResult is a flow_graph/1 document. cost is a NUMBER (the rows read): the
// in-chat app coerces it with num() and sums it across Expand calls, so it must
// not be an object. view_url is empty in v1 (the hosted page is a later step).
type walkResult struct {
	Schema    string        `json:"schema"`
	Chain     string        `json:"chain"`
	DataAsOf  string        `json:"data_as_of"`
	Params    walkParamsOut `json:"params"`
	Nodes     []walkNode    `json:"nodes"`
	Edges     []edgeRow     `json:"edges"`
	Truncated bool          `json:"truncated"`
	Partial   bool          `json:"partial"`
	Cost      int64         `json:"cost"`
	ViewURL   string        `json:"view_url"`
	// budgetStopped is engine state, not part of the document (json:"-"): it says
	// the walk stopped on its budget, so runWalk can distinguish a graceful partial
	// (some edges) from a hard budget_exceeded refusal (no edges). It is never
	// serialized, so the flow_graph/1 shape is unchanged.
	budgetStopped bool `json:"-"`
	// hopsClampedFrom is the raw hops the caller asked for, set by runWalk only when it
	// differed from the effective (clamped) hops (0 otherwise). Text-only (json:"-"), so
	// summary() can say "clamped from N"; it is never part of the flow_graph/1 document.
	hopsClampedFrom int `json:"-"`
	// includeZero echoes the effective include_zero_value (0/1) for the empty-graph hint
	// text only. Text-only (json:"-"), never part of the document.
	includeZero int `json:"-"`
}

// fetchResult is one batched hop fetch: the outgoing edges of the requested sources,
// and counts — each source's distinct-counterparty count read POST-read from the edge
// statement's Counterparties column (the same value on every row of a source). The hub
// decision reads counts: a non-seed whose Counterparties exceeds hubN is a hub (its
// edges are discarded), decided AFTER the read, not before. A source absent from counts
// (a fetcher that supplies none, e.g. an offline mock) falls back to the distinct
// address targets in its edge rows.
type fetchResult struct {
	edges  []edgeRow
	counts map[string]int
}

// errChunkTooBig is fetchEdges's signal that the CHUNK row budget — not the whole
// call's budget — refused the read before it started (ClickHouse Code 158,
// TOO_MANY_ROWS, raised by the statement's max_rows_to_read): the chunk's estimated
// scan exceeds the per-chunk row budget, reading 0 rows. The engine BISECTS on it
// (splits the chunk and re-reads each half), rather than stopping the walk. A refusal
// the whole call's budget caused (the chunk budget was not the binding limit) is a
// normal stop error instead (isStopError), never this — so the engine MUST test
// errors.Is(err, errChunkTooBig) BEFORE isStopError, because a 158 satisfies both.
var errChunkTooBig = errors.New("flow_edges chunk exceeds the per-chunk row budget")

// hopFetcher reads a frontier chunk's outgoing edges in one billed statement, with its
// per-source Counterparties count, and returns them as a fetchResult. chunkCapped
// bounds the read by the per-chunk row budget (min with the remaining call budget):
// when it is the binding limit and the statement refuses before start, fetchEdges
// returns errChunkTooBig so the engine bisects. chunkCapped=false reads under the whole
// call budget only (the lone-seed path: a seed is read even if heavy, so one Code-158
// there is a genuine call-budget stop, not a bisect). The production impl renders the
// chain's statement and runs it through chainexec; tests supply a mock.
type hopFetcher interface {
	fetchEdges(ctx context.Context, sources []string, chunkCapped bool) (fetchResult, error)
}

// labelFetch reads directory.labels for a frontier chunk, keyed by the chain's
// directory address form. The production impl is a closure over t.fetchLabels;
// the second return is whether the read was cut short (a budget stop), which
// gates the frontier's expansion.
type labelFetch func(ctx context.Context, addrs []string) (byAddr map[string][]labelRow, partial bool, err error)

// walkConfig bounds a walk. hubN: more than this many counterparties stops a node as a
// hub. chunkSize: senders batched per statement. edgeCap: the global edge cap.
// chunkRowBudget: the per-chunk max_rows_to_read; a chunk whose scan exceeds it is
// refused before start (Code 158) and bisected. maxRefusals: the cap on such refusals
// per call, past which the remaining senders are left unexpanded (a graceful partial).
// stopGroups: a stop-group name -> its label types.
type walkConfig struct {
	hubN           int
	chunkSize      int
	edgeCap        int
	chunkRowBudget int64
	maxRefusals    int
	stopGroups     map[string][]string
}

// walkInput is one parsed, wired call for runWalkGraph. The sources (fetch,
// label) and the cost reader are injected so the engine runs offline in tests.
type walkInput struct {
	chain       string
	codec       chainCodec
	seeds       []string
	hops        int
	params      walkParamsOut
	dataAsOf    time.Time
	fetch       hopFetcher
	label       labelFetch
	lm          *labelMethod
	labelChains []string
	noCoverage  bool
	cfg         walkConfig
	// stopTypes is the set of label types that stop a node (the selected stop_at
	// groups' types, flattened); built by the caller.
	stopTypes map[string]bool
	// cost reads the running chainexec ledger; nil in tests (cost 0).
	cost func() chainexec.CostOut
}

// wnode is a node's mutable walk state. The output walkNode is derived from it.
type wnode struct {
	id           string
	hop          int
	isSeed       bool
	expanded     bool
	stopReason   string // "" | "label" | "hub"
	labels       []labelEntry
	label        *string
	edgesDropped int
	usdDropped   float64
}

// walkEngine holds the running graph. The maps are read on ordering/output paths
// only through the ordered slices (order, edges), so the output is deterministic.
type walkEngine struct {
	in        walkInput
	nodes     map[string]*wnode
	order     []*wnode
	edges     []edgeRow
	edgeSeen  map[string]bool
	truncated bool
	partial   bool
	stopped   bool // a fetch hit a budget stop: stop walking, keep what we have
	// budgetStop records that the walk ended on a budget/deadline stop (as opposed
	// to a clean finish). runWalk reads it to tell a hard refusal (budget_exceeded,
	// when nothing was gathered) from a graceful partial. It is engine state, never
	// inferred from the cost ledger: a statement that overflows and throws reads no
	// rows into the ledger, so "rows == budget" would miss a first-statement stop.
	budgetStop bool
	// refusals counts the Code-158 chunk-cap refusals (errChunkTooBig) seen on this
	// call. Each one bisects; once it reaches cfg.maxRefusals the engine stops
	// bisecting and leaves the remaining senders unexpanded (partial). Refusals read 0
	// rows, so they are not in the cost ledger — the engine counts them itself.
	refusals int
}

// runWalkGraph runs the breadth-first walk and returns the flow_graph/1 result.
// A real (non-budget) fetch or label error aborts with that error; a budget stop
// returns the finished-so-far graph with partial set.
func runWalkGraph(ctx context.Context, in walkInput) (*walkResult, error) {
	e := &walkEngine{in: in, nodes: map[string]*wnode{}, edgeSeen: map[string]bool{}}

	seedNodes := make([]*wnode, 0, len(in.seeds))
	for _, s := range in.seeds {
		n := e.ensure(s, 0)
		n.isSeed = true
		seedNodes = append(seedNodes, n)
	}
	// Seeds are labelled for display but never stopped, and are expanded whether or
	// not their labels completed (they are the subject of the walk).
	if _, err := e.labelFrontier(ctx, seedNodes, true); err != nil {
		return nil, err
	}

	frontier := seedNodes
	for h := 0; h < in.hops; h++ {
		discovered, err := e.expandFrontier(ctx, h, frontier)
		if err != nil {
			return nil, err
		}
		if e.stopped || len(discovered) == 0 {
			break
		}
		// Label the new frontier BEFORE the next hop expands it. If the labels did
		// not complete, do not expand it — walking through a node whose labels are
		// unknown could pass through an unflagged exchange.
		complete, lerr := e.labelFrontier(ctx, discovered, false)
		if lerr != nil {
			return nil, lerr
		}
		if !complete {
			e.partial = true
			break
		}
		frontier = discovered
	}
	return e.result(), nil
}

// ensure returns the node for id, creating it at hop when new (an existing node
// keeps its earlier, shortest hop).
func (e *walkEngine) ensure(id string, hop int) *wnode {
	if n, ok := e.nodes[id]; ok {
		return n
	}
	n := &wnode{id: id, hop: hop}
	e.nodes[id] = n
	e.order = append(e.order, n)
	return n
}

// labelFrontier reads the labels of a frontier, records each node's label entries
// and primary label, and (for non-seed nodes) sets a stop_reason of "label" when
// a label type is in a selected stop group. It returns whether the read
// completed; an incomplete read sets partial and leaves expansion to the caller.
func (e *walkEngine) labelFrontier(ctx context.Context, nodes []*wnode, seed bool) (complete bool, err error) {
	if e.in.noCoverage || len(nodes) == 0 {
		// No label data for the chain: nothing to read and nothing to stop on.
		return true, nil
	}
	addrs := make([]string, 0, len(nodes))
	for _, n := range nodes {
		addrs = append(addrs, n.id)
	}
	byAddr, partial, ferr := e.in.label(ctx, addrs)
	if ferr != nil {
		if isStopError(ctx, ferr) {
			e.partial = true
			e.budgetStop = true
			return false, nil
		}
		return false, ferr
	}
	if partial {
		// A label read cut short (no time left to start its next batch) is a budget stop.
		e.partial = true
		e.budgetStop = true
	}
	toolChain := e.in.labelChains[0]
	for _, n := range nodes {
		rows := restrictForContract(byAddr[e.in.codec.normalize(n.id)], toolChain)
		n.labels = labelEntries(e.in.lm, rows)
		if lt, lv, _, _ := e.in.lm.pick(rows); lt != "" {
			v := lv
			n.label = &v
		}
		if !seed && e.nodeStops(rows) {
			n.stopReason = "label"
		}
	}
	return !partial, nil
}

// nodeStops reports whether any of a node's label types is in a selected stop
// group (the stop fires on any type, not only the picked primary).
func (e *walkEngine) nodeStops(rows []labelRow) bool {
	for _, r := range rows {
		if e.in.stopTypes[r.LabelType] {
			return true
		}
	}
	return false
}

// expandFrontier expands the non-stopped nodes of a frontier at distance h,
// discovering the hop h+1 nodes. It ranks the frontier by USD received (so the edge
// cap keeps the higher-value flows), then reads it in chunks. Each chunk is read in one
// billed flow_edges statement bounded by the per-chunk row budget; a chunk the
// statement refuses before start (Code 158, errChunkTooBig) is bisected (readChunk /
// bisect). The hub decision is POST-read: a non-seed whose Counterparties count exceeds
// hubN is stopped and its edges discarded (fold). A budget/deadline stop ends the walk
// with partial set.
func (e *walkEngine) expandFrontier(ctx context.Context, h int, frontier []*wnode) ([]*wnode, error) {
	expandable := make([]*wnode, 0, len(frontier))
	for _, n := range frontier {
		if n.stopReason == "" {
			expandable = append(expandable, n)
		}
	}
	inc := map[string]float64{}
	for _, ed := range e.edges {
		if ed.AmountUsd != nil {
			inc[ed.To] += *ed.AmountUsd
		}
	}
	sort.SliceStable(expandable, func(i, j int) bool {
		ui, uj := inc[expandable[i].id], inc[expandable[j].id]
		if ui != uj {
			return ui > uj
		}
		return expandable[i].id < expandable[j].id
	})

	var discovered []*wnode
	seenDisc := map[string]bool{}
	cs := e.in.cfg.chunkSize
	if cs < 1 {
		cs = 1
	}
	for start := 0; start < len(expandable); start += cs {
		// The cap is already full: the rest of the frontier is left unexpanded.
		if len(e.edges) >= e.in.cfg.edgeCap {
			e.truncated = true
			break
		}
		end := min(start+cs, len(expandable))
		disc, stop, err := e.readChunk(ctx, h, expandable[start:end], seenDisc)
		discovered = append(discovered, disc...)
		if err != nil {
			return discovered, err
		}
		if stop {
			return discovered, nil
		}
	}
	return discovered, nil
}

// readChunk reads one chunk's outgoing edges in a single chunk-capped flow_edges
// statement and folds them into the graph. A Code-158 chunk-cap refusal
// (errChunkTooBig) bisects (split and re-read each half); a real budget/deadline stop
// ends the walk (stop=true); any other error is returned. The errors.Is check for
// errChunkTooBig MUST come before isStopError, because a 158 satisfies both.
func (e *walkEngine) readChunk(ctx context.Context, h int, chunk []*wnode, seenDisc map[string]bool) (discovered []*wnode, stop bool, err error) {
	if len(chunk) == 0 {
		return nil, false, nil
	}
	if len(e.edges) >= e.in.cfg.edgeCap {
		e.truncated = true
		return nil, false, nil
	}
	res, ferr := e.in.fetch.fetchEdges(ctx, idsOf(chunk), true)
	if ferr != nil {
		if errors.Is(ferr, errChunkTooBig) {
			return e.bisect(ctx, h, chunk, seenDisc)
		}
		if isStopError(ctx, ferr) {
			e.partial = true
			e.stopped = true
			e.budgetStop = true
			return nil, true, nil
		}
		return nil, false, ferr
	}
	return e.fold(h, chunk, res, seenDisc), false, nil
}

// bisect handles a chunk the per-chunk row budget refused (errChunkTooBig). It counts
// the refusal; once the count reaches cfg.maxRefusals it stops bisecting and leaves the
// chunk's senders unexpanded (a graceful partial). A chunk of one SEED is re-read under
// the whole call budget (a seed is the subject of the walk, read even if heavy); a
// chunk of one non-seed still too heavy is left unexpanded + partial (not a hub, not a
// walk stop). Otherwise the chunk is split in half — in the frontier's USD order, so
// the ranking still decides the edge cap — and each half re-read.
func (e *walkEngine) bisect(ctx context.Context, h int, chunk []*wnode, seenDisc map[string]bool) (discovered []*wnode, stop bool, err error) {
	e.refusals++
	if e.refusals >= e.in.cfg.maxRefusals {
		// The refusal cap is spent: leave the rest unexpanded (role() reports it) and
		// mark the result partial. Stop bisecting, but do not stop the walk.
		e.partial = true
		return nil, false, nil
	}
	if len(chunk) == 1 {
		n := chunk[0]
		if n.isSeed {
			return e.readSeedAlone(ctx, h, n, seenDisc)
		}
		// A single non-seed too heavy even alone: leave it unexpanded (role() reports
		// it), mark partial, and keep walking the rest of the frontier.
		e.partial = true
		return nil, false, nil
	}
	mid := len(chunk) / 2
	d1, stop1, err1 := e.readChunk(ctx, h, chunk[:mid], seenDisc)
	discovered = append(discovered, d1...)
	if err1 != nil {
		return discovered, stop1, err1
	}
	if stop1 {
		return discovered, true, nil
	}
	d2, stop2, err2 := e.readChunk(ctx, h, chunk[mid:], seenDisc)
	discovered = append(discovered, d2...)
	return discovered, stop2, err2
}

// readSeedAlone re-reads a lone seed that the per-chunk budget refused, this time under
// the whole call budget (chunkCapped=false): a seed is read even when heavy. It then
// either completes or hits the real call budget — a genuine budget stop that ends the
// walk.
func (e *walkEngine) readSeedAlone(ctx context.Context, h int, n *wnode, seenDisc map[string]bool) (discovered []*wnode, stop bool, err error) {
	if len(e.edges) >= e.in.cfg.edgeCap {
		e.truncated = true
		return nil, false, nil
	}
	res, ferr := e.in.fetch.fetchEdges(ctx, []string{n.id}, false)
	if ferr != nil {
		if isStopError(ctx, ferr) {
			e.partial = true
			e.stopped = true
			e.budgetStop = true
			return nil, true, nil
		}
		return nil, false, ferr
	}
	return e.fold(h, []*wnode{n}, res, seenDisc), false, nil
}

// fold folds a group's fetched edges into the graph: it decides each node's hub stop
// from the POST-read Counterparties count (a seed is never hub-stopped; a hub's edges
// are discarded), ranks a node's edges by USD, dedupes on (from,to,token), skips
// non-address targets, and records per-node drops once the edge cap is reached.
func (e *walkEngine) fold(h int, group []*wnode, res fetchResult, seenDisc map[string]bool) []*wnode {
	var discovered []*wnode
	bySource := map[string][]edgeRow{}
	for _, ed := range res.edges {
		bySource[ed.From] = append(bySource[ed.From], ed)
	}
	for _, n := range group {
		rows := bySource[n.id]
		if !n.isSeed && e.counterpartyCount(n.id, rows, res.counts) > e.in.cfg.hubN {
			// A hub (more counterparties than hubN), decided after the read: its edges are
			// discarded and its targets are not discovered.
			n.stopReason = "hub"
			continue
		}
		if len(e.edges) >= e.in.cfg.edgeCap {
			// The cap filled before this node: leave it unexpanded.
			e.truncated = true
			continue
		}
		n.expanded = true
		sortEdges(rows)
		for _, ed := range rows {
			if !e.isAddressTarget(ed.To) {
				continue // skip d-... placeholders and non-address targets
			}
			key := ed.From + "\x00" + ed.To + "\x00" + ed.Token
			if e.edgeSeen[key] {
				continue
			}
			if len(e.edges) >= e.in.cfg.edgeCap {
				n.edgesDropped++
				if ed.AmountUsd != nil {
					n.usdDropped += *ed.AmountUsd
				}
				e.truncated = true
				continue
			}
			e.edges = append(e.edges, ed)
			e.edgeSeen[key] = true
			tn := e.ensure(ed.To, h+1)
			if tn.hop == h+1 && !seenDisc[tn.id] {
				seenDisc[tn.id] = true
				discovered = append(discovered, tn)
			}
		}
	}
	return discovered
}

// idsOf is the id slice of a node group (the sources of a fetch).
func idsOf(group []*wnode) []string {
	srcs := make([]string, len(group))
	for i, n := range group {
		srcs[i] = n.id
	}
	return srcs
}

// counterpartyCount is a node's distinct-counterparty count for the hub test: the
// statement's per-source count when the fetch provided one, else the distinct
// valid-address targets in the returned rows.
func (e *walkEngine) counterpartyCount(id string, rows []edgeRow, counts map[string]int) int {
	if counts != nil {
		if c, ok := counts[id]; ok {
			return c
		}
	}
	seen := map[string]bool{}
	for _, ed := range rows {
		if e.isAddressTarget(ed.To) {
			seen[ed.To] = true
		}
	}
	return len(seen)
}

// isAddressTarget reports whether a target is a real address of the chain (not a
// d-... shielded-output placeholder and not empty).
func (e *walkEngine) isAddressTarget(to string) bool {
	if to == "" || strings.HasPrefix(to, "d-") {
		return false
	}
	return e.in.codec.outputAddr(to)
}

// sortEdges orders a node's edges by priced USD descending (so the edge cap drops
// the smallest), then by target and token for a stable order.
func sortEdges(rows []edgeRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		ui, uj := ptrOr0(rows[i].AmountUsd), ptrOr0(rows[j].AmountUsd)
		if ui != uj {
			return ui > uj
		}
		if rows[i].To != rows[j].To {
			return rows[i].To < rows[j].To
		}
		return rows[i].Token < rows[j].Token
	})
}

// result builds the flow_graph/1 document from the finished walk: the per-node
// in/out USD summed from the kept edges, the role of each node, and the flags.
func (e *walkEngine) result() *walkResult {
	inUsd := map[string]float64{}
	inHas := map[string]bool{}
	outUsd := map[string]float64{}
	outHas := map[string]bool{}
	for _, ed := range e.edges {
		if ed.AmountUsd != nil {
			inUsd[ed.To] += *ed.AmountUsd
			inHas[ed.To] = true
			outUsd[ed.From] += *ed.AmountUsd
			outHas[ed.From] = true
		}
	}
	nodes := make([]walkNode, 0, len(e.order))
	for _, n := range e.order {
		wn := walkNode{
			ID:           n.id,
			Label:        n.label,
			Labels:       n.labels,
			Role:         e.role(n),
			Hop:          n.hop,
			Status:       "ok",
			EdgesDropped: n.edgesDropped,
			UsdDropped:   n.usdDropped,
		}
		if wn.Labels == nil {
			wn.Labels = []labelEntry{}
		}
		if n.stopReason != "" {
			sr := n.stopReason
			wn.StopReason = &sr
		}
		if inHas[n.id] {
			v := inUsd[n.id]
			wn.InUsd = &v
		}
		if outHas[n.id] {
			v := outUsd[n.id]
			wn.OutUsd = &v
		}
		nodes = append(nodes, wn)
	}
	edges := e.edges
	if edges == nil {
		edges = []edgeRow{}
	}
	var cost int64
	if e.in.cost != nil {
		cost = e.in.cost().RowsRead
	}
	return &walkResult{
		Schema:        walkSchema,
		Chain:         e.in.chain,
		DataAsOf:      e.in.dataAsOf.UTC().Format(time.RFC3339),
		Params:        e.in.params,
		Nodes:         nodes,
		Edges:         edges,
		Truncated:     e.truncated,
		Partial:       e.partial,
		Cost:          cost,
		ViewURL:       "",
		budgetStopped: e.budgetStop,
	}
}

// role is a node's output role. A stopped node is "stop"; an expanded node, and a
// node at the maximum hop (a terminal frontier node), are "hop"; a non-stopped
// node below the maximum hop that was never expanded (the edge cap or the budget
// cut it) is "unexpanded".
func (e *walkEngine) role(n *wnode) string {
	switch {
	case n.isSeed:
		return "seed"
	case n.stopReason != "":
		return "stop"
	case n.expanded:
		return "hop"
	case n.hop == e.in.hops:
		return "hop"
	default:
		return "unexpanded"
	}
}

// labelEntries builds a node's labels array: one entry per distinct label type,
// with the best value for that type (same within-type rule as the pick), the
// distinct source count, and whether that value's most-trusted source class is
// heuristic. Types are sorted for a stable order and include every type present.
func labelEntries(m *labelMethod, rows []labelRow) []labelEntry {
	byType := map[string][]labelRow{}
	var types []string
	for _, r := range rows {
		if _, ok := byType[r.LabelType]; !ok {
			types = append(types, r.LabelType)
		}
		byType[r.LabelType] = append(byType[r.LabelType], r)
	}
	sort.Strings(types)
	out := make([]labelEntry, 0, len(types))
	for _, ty := range types {
		val, srcs, heur := bestValue(m, byType[ty])
		out = append(out, labelEntry{Type: ty, Value: val, Sources: srcs, Heuristic: heur})
	}
	return out
}

// bestValue chooses, within one label type, the value with the most-trusted
// source class, then the most distinct sources, then the lowest value; it returns
// that value, its distinct source count, and whether its best source class is
// heuristic.
func bestValue(m *labelMethod, rows []labelRow) (value string, sources int, heuristic bool) {
	type agg struct {
		value     string
		bestClass int
		bestName  string
		srcs      map[string]bool
	}
	aggs := map[string]*agg{}
	var order []string
	for _, r := range rows {
		a := aggs[r.Value]
		if a == nil {
			a = &agg{value: r.Value, bestClass: len(m.classRank) + 1, srcs: map[string]bool{}}
			aggs[r.Value] = a
			order = append(order, r.Value)
		}
		cls := m.sourceClass(r.Source)
		if cr := m.classRankOf(cls); cr < a.bestClass {
			a.bestClass = cr
			a.bestName = cls
		}
		a.srcs[r.Source] = true
	}
	if len(order) == 0 {
		return "", 0, false
	}
	sort.Strings(order)
	best := aggs[order[0]]
	for _, v := range order[1:] {
		a := aggs[v]
		switch {
		case a.bestClass != best.bestClass:
			if a.bestClass < best.bestClass {
				best = a
			}
		case len(a.srcs) != len(best.srcs):
			if len(a.srcs) > len(best.srcs) {
				best = a
			}
		case a.value < best.value:
			best = a
		}
	}
	return best.value, len(best.srcs), best.bestName == "heuristic"
}

// toMap renders the document as a generic map (what StructuredResult carries as
// structuredContent). The round-trip keeps the json tags, so the keys match the
// struct exactly.
func (r *walkResult) toMap() map[string]any {
	b, _ := json.Marshal(r)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// summary is the short text the model reads (content): the counts, the effective
// params, the stop nodes (by address) and a seed/stop node block. The full graph is
// the structured document.
func (r *walkResult) summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Flow graph — %s. %d nodes, %d edges, up to %d hops from %d seed(s).",
		r.Chain, len(r.Nodes), len(r.Edges), r.Params.Hops, len(r.Params.Address))
	if r.Truncated {
		b.WriteString(" Truncated at the edge cap.")
	}
	if r.Partial {
		b.WriteString(" Partial: stopped on the budget.")
	}
	b.WriteString("\n")
	b.WriteString(r.paramsLine())
	// Stops print the ADDRESS (a node id is a unique address), the label in brackets
	// when known, and the reason — never deduped, since distinct stop nodes are distinct
	// even when they share a label.
	var stops []string
	for _, n := range r.Nodes {
		if n.Role == "stop" && n.StopReason != nil {
			s := n.ID
			if n.Label != nil {
				s += " [" + *n.Label + "]"
			}
			stops = append(stops, fmt.Sprintf("%s (%s)", s, *n.StopReason))
		}
	}
	if len(stops) > 0 {
		fmt.Fprintf(&b, "Stopped at: %s.\n", strings.Join(stops, ", "))
	}
	// An empty graph that is NOT a budget partial: name the active filters that could
	// have emptied it, from the request alone — no extra query (that would bill a
	// statement). A budget partial already explains itself with the Partial marker.
	if len(r.Edges) == 0 && !r.Partial {
		token := r.Params.Token
		if token == "" {
			token = "any"
		}
		fmt.Fprintf(&b, "No outgoing transfers matched the filters (window %s..%s; min_usd=%s; token=%s; include_zero_value=%d).\n",
			r.Params.AfterTime, r.Params.BeforeTime, fmtUsd(r.Params.MinUsd), token, r.includeZero)
	}
	b.WriteString(r.nodesCSV())
	return b.String()
}

// paramsLine echoes the EFFECTIVE request (post-default, post-clamp), including
// whether hops was clamped, so a caller sees what actually ran.
func (r *walkResult) paramsLine() string {
	stopAt := r.Params.StopAt
	if strings.TrimSpace(stopAt) == "" {
		stopAt = "all"
	}
	token := r.Params.Token
	if token == "" {
		token = "any"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Params: hops=%d", r.Params.Hops)
	if r.hopsClampedFrom != 0 {
		fmt.Fprintf(&b, " (clamped from %d)", r.hopsClampedFrom)
	}
	fmt.Fprintf(&b, ", window %s..%s, per_node=%d, min_usd=%s, stop_at=%s, token=%s\n",
		r.Params.AfterTime, r.Params.BeforeTime, r.Params.PerNode, fmtUsd(r.Params.MinUsd), stopAt, token)
	return b.String()
}

// nodesCSV is the node identity block: the seed and stop nodes as CSV (the nodes a
// reader acts on), so the text names each by ADDRESS, not only by a shared label. It
// is written with encoding/csv — a chain-supplied label is attacker-controlled, like
// the edge symbol. A nil label or stop_reason is "", and in/out USD follow usdCell
// ("" — never 0 — when unpriced).
func (r *walkResult) nodesCSV() string {
	var rows []walkNode
	for _, n := range r.Nodes {
		if n.Role == "seed" || n.Role == "stop" {
			rows = append(rows, n)
		}
	}
	if len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("nodes (seeds and stops):\n")
	w := csv.NewWriter(&b)
	_ = w.Write([]string{"address", "role", "label", "stop_reason", "in_usd", "out_usd"})
	for _, n := range rows {
		label, reason := "", ""
		if n.Label != nil {
			label = *n.Label
		}
		if n.StopReason != nil {
			reason = *n.StopReason
		}
		_ = w.Write([]string{n.ID, n.Role, label, reason, usdCell(n.InUsd), usdCell(n.OutUsd)})
	}
	w.Flush()
	return b.String()
}

// fmtUsd formats a USD threshold for the text summary ('f', -1 so 1_000_000 prints as
// 1000000, not the 1e+06 that %v would give).
func fmtUsd(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// maxContentEdges is how many edges the text content carries as CSV; the full graph
// is always in structuredContent.
const maxContentEdges = 100

// content is the text the model and a text-only host read (the content item): the
// summary, up to maxContentEdges edges as CSV, and — when share is on — the view_url
// line. runWalk returns it as a tools.TextResult so it reaches the client verbatim,
// not JSON-quoted. AT10: a client with no structuredContent still gets the edges and
// the view_url line from here.
func (r *walkResult) content(share bool) string {
	var b strings.Builder
	b.WriteString(r.summary())
	b.WriteString(r.edgesCSV(maxContentEdges))
	if share {
		url := r.ViewURL
		if url == "" {
			url = "(not available in this version)"
		}
		fmt.Fprintf(&b, "view_url: %s\n", url)
	}
	return b.String()
}

// edgesCSV renders the top n edges BY USD as CSV (so truncation drops the smallest
// flows, not the last-traversed), ranked on a copy that leaves r.Edges untouched. It
// is written with encoding/csv, so a chain-supplied symbol that contains a comma, a
// quote or a newline is quoted and cannot forge a new row or column (Symbol is
// attacker-controlled on-chain data).
func (r *walkResult) edgesCSV(n int) string {
	var b strings.Builder
	// Rank a COPY by USD so the text keeps the HIGHEST-value edges when it truncates,
	// rather than the first-traversed. r.Edges — the structured document's order — is
	// never reordered (runWalk renders content() before toMap()).
	ranked := append([]edgeRow(nil), r.Edges...)
	sort.SliceStable(ranked, func(i, j int) bool {
		pi, pj := ranked[i].AmountUsd != nil, ranked[j].AmountUsd != nil
		if pi != pj {
			return pi // priced before unpriced (nulls last)
		}
		if pi && pj && *ranked[i].AmountUsd != *ranked[j].AmountUsd {
			return *ranked[i].AmountUsd > *ranked[j].AmountUsd
		}
		if ranked[i].Transfers != ranked[j].Transfers {
			return ranked[i].Transfers > ranked[j].Transfers
		}
		if ranked[i].To != ranked[j].To {
			return ranked[i].To < ranked[j].To
		}
		return ranked[i].Token < ranked[j].Token
	})
	shown := len(ranked)
	if shown > n {
		shown = n
		fmt.Fprintf(&b, "Showing the top %d of %d edges by USD as CSV; the full graph is in structuredContent.\n", n, len(ranked))
	}
	b.WriteString("\n")
	w := csv.NewWriter(&b)
	_ = w.Write([]string{"from", "to", "token", "symbol", "amount", "amount_usd", "usd_priced_share", "transfers", "first_seen", "last_seen"})
	for _, ed := range ranked[:shown] {
		_ = w.Write([]string{
			ed.From, ed.To, ed.Token, ed.Symbol, ed.Amount,
			usdCell(ed.AmountUsd), strconv.FormatFloat(ed.UsdPricedShare, 'f', -1, 64),
			strconv.FormatInt(ed.Transfers, 10), ed.FirstSeen, ed.LastSeen,
		})
	}
	w.Flush()
	return b.String()
}

// usdCell renders a nullable USD value for the CSV: "" (never 0) when the edge has
// no priced transfer, matching amount_usd being null in the structured document.
func usdCell(p *float64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatFloat(*p, 'f', -1, 64)
}
