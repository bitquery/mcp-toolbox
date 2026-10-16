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

// The multi-hop search (view all). Received funds are traced backward in time
// (where did what the target received come from), sent funds forward (where
// did what the target sent go). One level of the search, for every unlabelled
// address of the frontier at once:
//
//  1. labels for the whole frontier in one batch: an address with an entity
//     type ends its branch, its amount attributed to that entity, whatever the
//     amount;
//  2. an unlabelled branch whose amount is below branch_min_usd ends: cut small;
//  3. at the last hop the rest ends: cut max_hops;
//  4. the degree gate (key-only counts, both tables): over hub_transfers the
//     address is a hub and its branch ends: cut hub;
//  5. the history of the rest, one statement per table for the whole frontier,
//     a received-side address read up to the latest transfer that links it to
//     the path, a sent-side address over its whole history;
//  6. the balance-aware attribution (attrib.go) splits each branch's amount over
//     the address's sources (received side) or destinations (sent side): those
//     are the next frontier.
//
// A mint (a receipt from the zero address) is attributed to the entity "mint"
// of type Other, issued by the token; it has no label row. (A coinbase would
// be Miner: Tron has none.) A burn is a cut.
//
// Every amount ends in exactly one place: an entity, or an unattributed bucket
// named by its cut. A branch's amount is split in whole raw units, so the
// buckets and the entities add up to the analysed total exactly; that, and that
// no link carries more than the history shows, is checked before a report is
// returned.

import (
	"context"
	"fmt"
	"math/big"
	"slices"
	"sort"
	"strings"
	"time"
)

// cutReasons are the unattributed buckets of view all, in output order.
var cutReasons = []string{"counterparty_limit", "small", "hub", "max_hops", "budget", "history_incomplete", "cycle", "burn", "unspent"}

var cutNotes = map[string]string{
	"counterparty_limit": "1-hop counterparties past the counterparty limit were not looked up",
	"small":              "branches below branch_min_usd were not followed",
	"hub":                "an unlabelled address with more than hub_transfers transfers was not followed",
	"max_hops":           "an unlabelled address at the last hop searched",
	"budget":             "not reached before the deadline or the report's row budget",
	"history_incomplete": "an address sent more than its recorded balance; the excess has no known source",
	"cycle":              "funds that came back through an address already on the path, the target included",
	"burn":               "sent to the zero address",
	"unspent":            "still held by an unlabelled address",
}

// hubWithoutEstimate is the note of a hub whose size could not be estimated.
const hubWithoutEstimate = "no granule estimate from EXPLAIN: treated as a hub without a count"

// searchEntry is one branch: an amount that reached an address at a depth.
type searchEntry struct {
	dir    direction
	node   string
	amount *big.Int
	depth  int
	parent *searchEntry // nil at hop 1: the parent is the target
	// tx is the transfer that stands for the edge between node and its parent:
	// at hop 1 the counterparty's first transfer above dust (as in the direct
	// view), deeper the largest transfer on the link.
	tx string
	// bound: the received side reads the node's history up to this time.
	bound time.Time
}

func (e *searchEntry) onPath(addr, target string) bool {
	if addr == target {
		return true
	}
	for p := e; p != nil; p = p.parent {
		if p.node == addr {
			return true
		}
	}
	return false
}

// evidence is the path's transfers, hop 1 first.
func (e *searchEntry) evidence() []string {
	var out []string
	for p := e; p != nil; p = p.parent {
		out = append(out, p.tx)
	}
	slices.Reverse(out)
	return out
}

// capState is what one link can still carry: the parts the history gives it,
// less what branches already took. A branch never takes more than is left, so
// no source is attributed more than it sent along the link.
type capState struct {
	initial map[string]*big.Int
	used    map[string]*big.Int
}

// raise widens the link to a longer read of the same history (parts only grow).
func (c *capState) raise(d map[string]*big.Int) {
	for k, v := range d {
		if cur, ok := c.initial[k]; !ok || cur.Cmp(v) < 0 {
			c.initial[k] = new(big.Int).Set(v)
		}
	}
}

func (c *capState) remaining() (map[string]*big.Int, *big.Int) {
	rem := map[string]*big.Int{}
	tot := new(big.Int)
	for k, v := range c.initial {
		r := new(big.Int).Set(v)
		if u, ok := c.used[k]; ok {
			r.Sub(r, u)
		}
		if r.Sign() > 0 {
			rem[k] = r
			tot.Add(tot, r)
		}
	}
	return rem, tot
}

// search is the state of one report's search.
type search struct {
	m         *method
	ex        *executor
	req       request
	asset     tronAsset
	target    string
	minRaw    *big.Int
	branchMin *big.Int
	maxHops   int

	classOf    func(string) addressLabels
	loadLabels func(context.Context, []string) error
	attribute  func(at attribution, cl addressLabels, addr string, d direction, raw *big.Int, hops int, evidence []string)

	cuts     map[direction]map[string]*big.Int
	cutAddrs map[direction]map[string]map[string]bool
	unnamed  map[string]*unnamedOut

	degIn, degOut map[string]int64
	hub           map[string]bool
	hubNote       map[string]string
	events        map[string][]histEvent
	readTo        map[string]uint32
	flowMemo      map[string]*flows
	caps          map[string]*capState
	incomplete    map[string]bool
	mismatch      int
	stopped       bool
	maxReached    int
}

func newSearch() *search {
	s := &search{
		cuts: map[direction]map[string]*big.Int{}, cutAddrs: map[direction]map[string]map[string]bool{},
		unnamed: map[string]*unnamedOut{}, degIn: map[string]int64{}, degOut: map[string]int64{}, hub: map[string]bool{}, hubNote: map[string]string{},
		events: map[string][]histEvent{}, readTo: map[string]uint32{}, flowMemo: map[string]*flows{}, caps: map[string]*capState{},
		incomplete: map[string]bool{}, maxReached: 1,
	}
	for _, d := range []direction{received, sent} {
		s.cuts[d] = map[string]*big.Int{}
		s.cutAddrs[d] = map[string]map[string]bool{}
		for _, r := range cutReasons {
			s.cuts[d][r] = new(big.Int)
			s.cutAddrs[d][r] = map[string]bool{}
		}
	}
	return s
}

// cut puts an amount in an unattributed bucket; row lists the address with
// its amount among the unnamed connections.
func (s *search) cut(e *searchEntry, reason, addr string, amount *big.Int, row bool) {
	if amount.Sign() == 0 {
		return
	}
	s.cuts[e.dir][reason].Add(s.cuts[e.dir][reason], amount)
	s.cutAddrs[e.dir][reason][addr] = true
	if !row {
		return
	}
	k := e.dir.String() + "\x00" + addr + "\x00" + reason
	u, ok := s.unnamed[k]
	if !ok {
		u = &unnamedOut{Address: addr, Cut: reason, recvRaw: new(big.Int), sentRaw: new(big.Int), EvidencePath: e.evidence()}
		if reason == "hub" {
			u.Note = s.hubNote[addr]
		}
		s.unnamed[k] = u
	}
	if e.dir == received {
		u.recvRaw.Add(u.recvRaw, amount)
		if u.ReceivedHops == nil || *u.ReceivedHops > e.depth {
			u.ReceivedHops = intPtr(e.depth)
		}
	} else {
		u.sentRaw.Add(u.sentRaw, amount)
		if u.SentHops == nil || *u.SentHops > e.depth {
			u.SentHops = intPtr(e.depth)
		}
	}
}

// mintEntity is the entity a mint is attributed to.
const (
	mintEntity = "mint"
	mintType   = "Other"
	mintSource = "issuance"
)

// mint attributes an amount issued by the token (a receipt from the zero
// address) to the entity "mint" of type Other, from the source "issuance".
func (s *search) mint(amount *big.Int, d direction, hops int, evidence []string) {
	if amount.Sign() == 0 {
		return
	}
	at := attribution{Label: typedLabel{Type: mintType, Row: labelRow{Address: tronZero, LabelType: mintSource, Value: mintEntity, Source: mintSource}},
		Entity: mintEntity, SourceClass: mintSource}
	s.attribute(at, addressLabels{}, tronZero, d, amount, hops, evidence)
}

func (s *search) cutAll(entries []*searchEntry, reason string) {
	for _, e := range entries {
		s.cut(e, reason, e.node, e.amount, true)
	}
}

func sortEntries(es []*searchEntry) {
	sort.SliceStable(es, func(i, j int) bool {
		if c := es[i].amount.Cmp(es[j].amount); c != 0 {
			return c > 0
		}
		if es[i].dir != es[j].dir {
			return es[i].dir < es[j].dir
		}
		if es[i].node != es[j].node {
			return es[i].node < es[j].node
		}
		return strings.Join(es[i].evidence(), ",") < strings.Join(es[j].evidence(), ",")
	})
}

func distinctNodes(es []*searchEntry) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range es {
		if !seen[e.node] {
			seen[e.node] = true
			out = append(out, e.node)
		}
	}
	sort.Strings(out)
	return out
}

// run searches level by level until the frontier is empty.
func (s *search) run(ctx context.Context, first []*searchEntry) error {
	frontier := first
	for depth := 1; len(frontier) > 0; depth++ {
		s.maxReached = depth
		next, err := s.level(ctx, depth, frontier)
		if err != nil {
			return err
		}
		frontier = next
	}
	return nil
}

func (s *search) level(ctx context.Context, depth int, frontier []*searchEntry) ([]*searchEntry, error) {
	sortEntries(frontier)
	if s.stopped {
		s.cutAll(frontier, "budget")
		return nil, nil
	}
	// 1. The target and the zero address are not followed: a cycle, and a mint
	// (received) or a burn (sent).
	var live []*searchEntry
	for _, e := range frontier {
		switch {
		case e.node == s.target:
			s.cut(e, "cycle", e.node, e.amount, false)
		case e.node == tronZero && e.dir == received:
			s.mint(e.amount, e.dir, depth, e.evidence())
		case e.node == tronZero:
			s.cut(e, "burn", e.node, e.amount, false)
		default:
			live = append(live, e)
		}
	}
	// 2. Labels for the whole frontier (hop 1's are already known): an address
	// with an entity label is attributed whatever the amount that reached it.
	if depth > 1 && len(live) > 0 {
		if err := s.loadLabels(ctx, distinctNodes(live)); err != nil {
			if classifyLimit(ctx, err) == notLimit {
				return nil, err
			}
			s.stopped = true
			s.cutAll(live, "budget")
			return nil, nil
		}
	}
	var unlabelled []*searchEntry
	for _, e := range live {
		cl := s.classOf(e.node)
		if at, ok := s.m.attribute(cl, s.req.weights, e.dir); ok {
			s.attribute(at, cl, e.node, e.dir, e.amount, depth, e.evidence())
			continue
		}
		unlabelled = append(unlabelled, e)
	}
	// 3. Small unlabelled branches, by the address's total at this level.
	agg := map[string]*big.Int{}
	for _, e := range unlabelled {
		addTo(agg, e.dir.String()+"\x00"+e.node, e.amount)
	}
	var open []*searchEntry
	for _, e := range unlabelled {
		if s.branchMin.Sign() > 0 && agg[e.dir.String()+"\x00"+e.node].Cmp(s.branchMin) < 0 {
			s.cut(e, "small", e.node, e.amount, false)
			continue
		}
		open = append(open, e)
	}
	// 3. The last hop.
	if depth >= s.maxHops {
		s.cutAll(open, "max_hops")
		return nil, nil
	}
	// 4. The degree gate.
	var need []string
	for _, n := range distinctNodes(open) {
		_, in := s.degIn[n]
		_, out := s.degOut[n]
		if !s.hub[n] && !(in && out) {
			need = append(need, n)
		}
	}
	if len(need) > 0 {
		res, err := s.ex.degrees(ctx, need)
		if err != nil {
			if classifyLimit(ctx, err) == notLimit {
				return nil, err
			}
			s.stopped = true
			s.cutAll(open, "budget")
			return nil, nil
		}
		for _, n := range need {
			s.degIn[n] = res.in[n]
			s.degOut[n] = res.out[n]
			if res.over[n] {
				s.hub[n] = true
			}
			if res.unknown[n] {
				s.hubNote[n] = hubWithoutEstimate
			}
		}
	}
	var keep []*searchEntry
	for _, e := range open {
		if s.hub[e.node] || s.degIn[e.node]+s.degOut[e.node] > int64(s.ex.th.HubTransfers) {
			s.hub[e.node] = true
			s.cut(e, "hub", e.node, e.amount, true)
			continue
		}
		keep = append(keep, e)
	}
	// 5. The histories.
	if err := s.read(ctx, keep); err != nil {
		return nil, err
	}
	// 6. Attribution: the next frontier.
	var next []*searchEntry
	for _, e := range keep {
		if !s.hasHistory(e) {
			s.cut(e, "budget", e.node, e.amount, true)
			continue
		}
		ch, err := s.expand(e)
		if err != nil {
			return nil, err
		}
		next = append(next, ch...)
	}
	return next, nil
}

// boundOf is the history an entry needs, in unix seconds.
func (s *search) boundOf(e *searchEntry) uint32 {
	if e.dir == sent || e.bound.IsZero() {
		return unbounded
	}
	u := e.bound.Unix()
	if u < 0 {
		return 0
	}
	if u >= int64(unbounded) {
		return unbounded - 1
	}
	return uint32(u)
}

func (s *search) hasHistory(e *searchEntry) bool {
	r, ok := s.readTo[e.node]
	return ok && r >= s.boundOf(e)
}

// read loads the histories the entries need that are not loaded yet. A
// statement stopped by a limit stops the search; the addresses it was for stay
// unread (their branches become cut budget).
func (s *search) read(ctx context.Context, entries []*searchEntry) error {
	need := map[string]uint32{}
	for _, e := range entries {
		need[e.node] = max(need[e.node], s.boundOf(e))
	}
	var todo []string
	for n, b := range need {
		if r, ok := s.readTo[n]; !ok || r < b {
			todo = append(todo, n)
		}
	}
	sort.Strings(todo)
	for i := 0; i < len(todo) && !s.stopped; i += historyBatch {
		chunk := todo[i:min(i+historyBatch, len(todo))]
		bounds := make([]uint32, len(chunk))
		var nIn, nOut int64
		for j, n := range chunk {
			bounds[j] = need[n]
			nIn += s.degIn[n]
			nOut += s.degOut[n]
		}
		var rows []histRow
		ok := true
		for _, side := range []hopSide{receiptsSide, sendsSide} {
			capRows := nIn
			if !side.In {
				capRows = nOut
			}
			got, err := s.ex.history(ctx, side, chunk, bounds, s.asset, int(capRows)+10)
			if err != nil {
				if classifyLimit(ctx, err) == notLimit {
					return err
				}
				s.stopped = true
				ok = false
				break
			}
			rows = append(rows, got...)
		}
		if !ok {
			break
		}
		byNode := map[string][]histEvent{}
		for _, r := range rows {
			// A transfer to itself changes neither the balance nor its mix.
			if r.ev.Peer == r.addr {
				continue
			}
			byNode[r.addr] = append(byNode[r.addr], r.ev)
		}
		for j, n := range chunk {
			ev := byNode[n]
			sortEvents(ev)
			s.events[n] = ev
			s.readTo[n] = bounds[j]
			for k := range s.flowMemo {
				if strings.HasPrefix(k, n+"\x00") {
					delete(s.flowMemo, k)
				}
			}
		}
	}
	return nil
}

// keyOf names an event's peer: the zero address is a mint or a burn, and a
// transfer with the target outside the analysed amount (out of the window, or
// at or below min_amount_usd) is told apart from one inside it.
func (s *search) keyOf(e histEvent) string {
	switch {
	case e.Peer == tronZero && e.In:
		return keyMint
	case e.Peer == tronZero:
		return keyBurn
	case e.Peer == s.target:
		w := s.req.w
		if e.Raw.Cmp(s.minRaw) <= 0 || (!w.After.IsZero() && e.Time.Before(w.After)) || (!w.Before.IsZero() && !e.Time.Before(w.Before)) {
			return keyTargetOther
		}
		return s.target
	}
	return e.Peer
}

// flowsOf replays a node's history up to a bound, once per (node, bound).
func (s *search) flowsOf(node string, bound uint32) (*flows, error) {
	k := fmt.Sprintf("%s\x00%d", node, bound)
	if f, ok := s.flowMemo[k]; ok {
		return f, nil
	}
	var ev []histEvent
	for _, e := range s.events[node] {
		if bound == unbounded || e.Time.Unix() <= int64(bound) {
			ev = append(ev, e)
		}
	}
	f, err := replay(ev, s.keyOf)
	if err != nil {
		return nil, fmt.Errorf("%w (address %s)", err, node)
	}
	if f.incompleteSends > 0 {
		s.incomplete[node] = true
	}
	s.flowMemo[k] = f
	return f, nil
}

// allocate splits a branch's amount over what its link can still carry. More
// than the link carries (the two tables disagree) is returned as excess.
func (s *search) allocate(capKey string, d map[string]*big.Int, amount *big.Int) (map[string]*big.Int, *big.Int) {
	cs, ok := s.caps[capKey]
	if !ok {
		cs = &capState{initial: map[string]*big.Int{}, used: map[string]*big.Int{}}
		s.caps[capKey] = cs
	}
	cs.raise(d)
	rem, tot := cs.remaining()
	amt := new(big.Int).Set(amount)
	excess := new(big.Int)
	if amt.Cmp(tot) > 0 {
		excess.Sub(amt, tot)
		amt.Set(tot)
	}
	parts := split(amt, rem, tot)
	for k, p := range parts {
		addTo(cs.used, k, p)
	}
	return parts, excess
}

func sortedParts(parts map[string]*big.Int) []string {
	keys := make([]string, 0, len(parts))
	for k := range parts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if c := parts[keys[i]].Cmp(parts[keys[j]]); c != 0 {
			return c > 0
		}
		return keys[i] < keys[j]
	})
	return keys
}

// expand splits one branch over the next hop.
func (s *search) expand(e *searchEntry) ([]*searchEntry, error) {
	fl, err := s.flowsOf(e.node, s.boundOf(e))
	if err != nil {
		return nil, err
	}
	link := s.target
	if e.parent != nil {
		link = e.parent.node
	}
	var d map[string]*big.Int
	var carried *big.Int
	if e.dir == received {
		d = fl.out[link]
		carried = fl.sentTo[link]
	} else {
		d = map[string]*big.Int{}
		for dst, srcs := range fl.out {
			if v, ok := srcs[link]; ok && v.Sign() > 0 {
				d[dst] = v
			}
		}
		if h, ok := fl.held[link]; ok && h.Sign() > 0 {
			d[keyHeld] = h
		}
		carried = fl.recvFrom[link]
	}
	if carried == nil {
		carried = new(big.Int)
	}
	// The two tables agree when the link's total in this address's history
	// equals what the other side recorded: at hop 1 the analysed amount, deeper
	// the parent's own history of the link.
	if e.parent == nil {
		if carried.Cmp(e.amount) != 0 {
			s.mismatch++
		}
	} else if pf, ok := s.flowMemo[fmt.Sprintf("%s\x00%d", e.parent.node, s.boundOf(e.parent))]; ok {
		other := pf.recvFrom[e.node]
		if e.dir == sent {
			other = pf.sentTo[e.node]
		}
		if other == nil || other.Cmp(carried) != 0 {
			s.mismatch++
		}
	}
	parts, excess := s.allocate(e.dir.String()+"\x00"+e.node+"\x00"+link, d, e.amount)
	if excess.Sign() > 0 {
		s.incomplete[e.node] = true
		s.cut(e, "history_incomplete", e.node, excess, true)
	}
	var next []*searchEntry
	for _, k := range sortedParts(parts) {
		p := parts[k]
		switch {
		case k == keyIncomplete:
			s.cut(e, "history_incomplete", e.node, p, true)
		case k == keyMint:
			ev := append(e.evidence(), fl.bigRecv[keyMint].Tx)
			s.mint(p, e.dir, e.depth+1, ev)
		case k == keyBurn:
			s.cut(e, "burn", e.node, p, false)
		case k == keyHeld:
			s.cut(e, "unspent", e.node, p, true)
		case k == keyTargetOther || e.onPath(k, s.target):
			addr := k
			if k == keyTargetOther {
				addr = s.target
			}
			s.cut(e, "cycle", addr, p, false)
		default:
			ch := &searchEntry{dir: e.dir, node: k, amount: p, depth: e.depth + 1, parent: e}
			if e.dir == received {
				ch.tx = fl.bigRecv[k].Tx
				ch.bound = fl.lastRecv[k]
			} else {
				ch.tx = fl.bigSend[k].Tx
			}
			next = append(next, ch)
		}
	}
	return next, nil
}

// checkLinks is invariant 2: no link gave more than its history carries.
func (s *search) checkLinks() error {
	for k, cs := range s.caps {
		for src, u := range cs.used {
			if in, ok := cs.initial[src]; !ok || u.Cmp(in) > 0 {
				return fmt.Errorf("invariant failed (no over-attribution): a source is attributed more than it sent along a link (%s)", strings.ReplaceAll(k, "\x00", " "))
			}
		}
	}
	return nil
}
