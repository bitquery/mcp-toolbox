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

// The balance-aware attribution of one address in one asset. Over the address's
// history in chain order, a receipt of v from s adds v to s's part of the
// balance; a send of v takes from every part in proportion to it, so the send
// carries the balance's mix and the mix does not change. That is the rule
// m = (B·m + v·[s]) / (B + v), B += v on a receipt and B -= v on a send, kept as
// the parts themselves (raw integer units) instead of the fractions m: each
// send is split exactly, with the units left over by the integer division given
// to the largest remainders, so every send's parts add up to the send and no
// part ever goes below zero. A send larger than the balance takes the whole
// balance and the excess is history_incomplete: a hole in the data.

import (
	"fmt"
	"math/big"
	"sort"
	"time"
)

// Keys of the parts that are not an address.
const (
	keyMint       = "!mint"               // a receipt from the zero address
	keyBurn       = "!burn"               // a send to the zero address
	keyIncomplete = "!history_incomplete" // the part of a send above the balance
	keyHeld       = "!held"               // what is still in the balance at the end
	// A transfer with the target that is not part of the analysed amount (in the
	// window and above min_amount_usd): it is the target's, but not traced.
	keyTargetOther = "!target_other"
)

// tronZero is the zero address: a receipt from it is a mint, a send to it a burn.
const tronZero = "T9yD14Nj9j7xAB4dbGeiX9h8unkKHxuWwb"

// histEvent is one transfer of an address in the report asset.
type histEvent struct {
	In       bool   // a receipt of the address (else a send)
	Peer     string // the other side, base58
	Raw      *big.Int
	Block    uint64
	TxIndex  uint64
	Call     uint64
	Log      uint64
	Transfer uint64
	Time     time.Time
	Tx       string
}

// before is the chain order: (block, tx index, call, log, transfer index), a
// receipt before a send on an exact tie (a send never waits for a receipt in
// the same frame), then the transaction hash; rows equal in all of that keep
// their order (the sort is stable).
func (a histEvent) before(b histEvent) bool {
	for _, p := range [][2]uint64{{a.Block, b.Block}, {a.TxIndex, b.TxIndex}, {a.Call, b.Call}, {a.Log, b.Log}, {a.Transfer, b.Transfer}} {
		if p[0] != p[1] {
			return p[0] < p[1]
		}
	}
	if a.In != b.In {
		return a.In
	}
	return a.Tx < b.Tx
}

func sortEvents(ev []histEvent) {
	sort.SliceStable(ev, func(i, j int) bool { return ev[i].before(ev[j]) })
}

// edgeRef is the transfer that stands for an edge in an evidence path: the
// largest one, the earliest on a tie.
type edgeRef struct {
	Tx   string
	Raw  *big.Int
	Time time.Time
}

func noteEdge(m map[string]edgeRef, k string, e histEvent) {
	if cur, ok := m[k]; ok && cur.Raw.Cmp(e.Raw) >= 0 {
		return
	}
	m[k] = edgeRef{Tx: e.Tx, Raw: e.Raw, Time: e.Time}
}

// flows is one replay of an address's history: where every source's funds went.
type flows struct {
	// out[dst][src] is the part of src's funds carried by the sends to dst.
	out      map[string]map[string]*big.Int
	held     map[string]*big.Int // src -> still in the balance at the end
	recvFrom map[string]*big.Int
	sentTo   map[string]*big.Int
	bigRecv  map[string]edgeRef // largest receipt per source
	bigSend  map[string]edgeRef // largest send per destination
	lastRecv map[string]time.Time
	balance  *big.Int
	// Sends larger than the balance, and their excess over it.
	incompleteSends int
	incomplete      *big.Int
}

// keyFn names the peer of an event as a part key.
type keyFn func(histEvent) string

// replay applies the balance-aware rule to events in chain order. It checks its
// own arithmetic: the parts left add up to the balance.
func replay(events []histEvent, key keyFn) (*flows, error) {
	f := &flows{
		out: map[string]map[string]*big.Int{}, held: map[string]*big.Int{},
		recvFrom: map[string]*big.Int{}, sentTo: map[string]*big.Int{},
		bigRecv: map[string]edgeRef{}, bigSend: map[string]edgeRef{}, lastRecv: map[string]time.Time{},
		balance: new(big.Int), incomplete: new(big.Int),
	}
	comp := f.held
	bal := f.balance
	for i, e := range events {
		if i > 0 && e.before(events[i-1]) {
			return nil, fmt.Errorf("history integrity: events out of chain order")
		}
		if e.Raw == nil || e.Raw.Sign() < 0 {
			return nil, fmt.Errorf("history integrity: a transfer without an amount")
		}
		k := key(e)
		if e.In {
			addTo(comp, k, e.Raw)
			bal.Add(bal, e.Raw)
			addTo(f.recvFrom, k, e.Raw)
			noteEdge(f.bigRecv, k, e)
			if e.Time.After(f.lastRecv[k]) {
				f.lastRecv[k] = e.Time
			}
			continue
		}
		addTo(f.sentTo, k, e.Raw)
		noteEdge(f.bigSend, k, e)
		out := f.out[k]
		if out == nil {
			out = map[string]*big.Int{}
			f.out[k] = out
		}
		if e.Raw.Cmp(bal) >= 0 {
			for s, c := range comp {
				addTo(out, s, c)
				delete(comp, s)
			}
			if excess := new(big.Int).Sub(e.Raw, bal); excess.Sign() > 0 {
				addTo(out, keyIncomplete, excess)
				f.incomplete.Add(f.incomplete, excess)
				f.incompleteSends++
			}
			bal.SetInt64(0)
			continue
		}
		for s, p := range split(e.Raw, comp, bal) {
			addTo(out, s, p)
			c := comp[s]
			c.Sub(c, p)
			if c.Sign() == 0 {
				delete(comp, s)
			}
		}
		bal.Sub(bal, e.Raw)
	}
	sum := new(big.Int)
	for _, c := range comp {
		if c.Sign() < 0 {
			return nil, fmt.Errorf("history integrity: a source's part went below zero")
		}
		sum.Add(sum, c)
	}
	if sum.Cmp(bal) != 0 {
		return nil, fmt.Errorf("history integrity: the parts of the balance do not add up to it")
	}
	return f, nil
}

func addTo(m map[string]*big.Int, k string, v *big.Int) {
	if v.Sign() == 0 {
		return
	}
	if c, ok := m[k]; ok {
		c.Add(c, v)
		return
	}
	m[k] = new(big.Int).Set(v)
}

// split divides amount over the weights w (which add up to total > 0) in
// proportion, in whole units: each key gets floor(amount·w/total), and the
// units that leaves go one each to the largest remainders (the key breaking a
// tie). The parts add up to amount exactly; when amount <= total no part
// exceeds its weight.
func split(amount *big.Int, w map[string]*big.Int, total *big.Int) map[string]*big.Int {
	parts := make(map[string]*big.Int, len(w))
	if amount.Sign() == 0 || total.Sign() <= 0 {
		return parts
	}
	type rem struct {
		k string
		r *big.Int
	}
	var rems []rem
	sum := new(big.Int)
	prod := new(big.Int)
	for k, wk := range w {
		if wk.Sign() <= 0 {
			continue
		}
		prod.Mul(amount, wk)
		q, r := new(big.Int).QuoRem(prod, total, new(big.Int))
		if q.Sign() > 0 {
			parts[k] = q
			sum.Add(sum, q)
		}
		if r.Sign() > 0 {
			rems = append(rems, rem{k, r})
		}
	}
	left := new(big.Int).Sub(amount, sum)
	if left.Sign() == 0 {
		return parts
	}
	sort.Slice(rems, func(i, j int) bool {
		if c := rems[i].r.Cmp(rems[j].r); c != 0 {
			return c > 0
		}
		return rems[i].k < rems[j].k
	})
	one := big.NewInt(1)
	for i := 0; left.Sign() > 0 && i < len(rems); i++ {
		if p, ok := parts[rems[i].k]; ok {
			p.Add(p, one)
		} else {
			parts[rems[i].k] = big.NewInt(1)
		}
		left.Sub(left, one)
	}
	return parts
}

// sumOf adds up a map's values.
func sumOf(m map[string]*big.Int) *big.Int {
	s := new(big.Int)
	for _, v := range m {
		s.Add(s, v)
	}
	return s
}
