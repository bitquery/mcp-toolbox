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

// Flags computed from the target's own transfers: address poisoning and
// pass-through. The label-driven flags (direct sanctions, risky dust, risk in
// another asset) are built where the labels are attributed (report.go).

import (
	"math/big"
	"sort"
)

// counterparty is one 1-hop counterparty in the report asset, for one
// direction. Amounts are raw integer units of the asset.
type counterparty struct {
	Dir               direction
	Address           string
	Transfers         int64
	Raw               *big.Int
	TransfersAboveMin int64
	RawAboveMin       *big.Int
	DustTransfers     int64
	RawMax            *big.Int
	// Sends inside the pass-through window after the first receipt.
	RawEarly    *big.Int
	First       string
	Last        string
	TxAboveDust []string
	Tx          []string
}

// contact is one 1-hop counterparty in the other assets, for one direction.
type contact struct {
	Dir        direction
	Address    string
	Transfers  int64
	NativeDust int64
	// Native-coin transfers above native dust.
	NativeAboveDust int64
	// Contracts (or asset ids) of the tokens it moved that carry the report
	// asset's symbol, or a look-alike of it, on another contract.
	Imitations []string
	First      string
	Tx         []string
}

// lookalike is one address-poisoning finding.
type lookalike struct {
	Address     string   `json:"address"`
	Imitates    string   `json:"imitates"`
	Kind        string   `json:"kind"`
	FirstChars  int      `json:"matching_first_chars"`
	LastChars   int      `json:"matching_last_chars"`
	Transfers   int64    `json:"transfers"`
	Amount      string   `json:"amount,omitempty"`
	FirstSeen   string   `json:"first_seen,omitempty"`
	EvidenceTxs []string `json:"evidence_txs"`
}

func commonSuffix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[len(a)-1-n] == b[len(b)-1-n] {
		n++
	}
	return n
}

// commonPrefixAfter counts the matching leading characters of a and b after a
// fixed prefix of skip characters. On a chain whose addresses all begin with
// the same network prefix (every Tron address starts with 'T'), that prefix is
// not a resemblance, so it is skipped before counting.
func commonPrefixAfter(a, b string, skip int) int {
	if skip < 0 {
		skip = 0
	}
	n := 0
	for skip+n < len(a) && skip+n < len(b) && a[skip+n] == b[skip+n] {
		n++
	}
	return n
}

// poisoningInput is what the poisoning rule reads.
type poisoningInput struct {
	Target   string
	Asset    []counterparty
	Other    []contact
	DustRaw  *big.Int // report asset: at or below = dust
	CapRaw   *big.Int // report asset: a receipt above it makes its sender genuine
	MinMatch int      // matching last characters
	// PrefixSkip is the chain's fixed network-prefix length, skipped before the
	// matching first characters are counted (1 on Tron: the leading 'T').
	PrefixSkip int
	Decimals   int
}

// findLookalikes applies the poisoning rule. The genuine counterparties C are
// the addresses the target paid above dust in the report asset, the senders of
// a report-asset receipt above the cap, and every address that moved the
// native coin above native dust to or from the target. A look-alike is an incoming sender that is not in C and shares
// at least MinMatch last characters with a member of C: a report-asset sender
// whose every transfer is at or below the cap (poisoners pay just over any
// fixed dust line), a native-dust sender, or a sender of an imitation token.
func findLookalikes(in poisoningInput) []lookalike {
	genuine := map[string]bool{}
	for _, c := range in.Asset {
		if c.Dir == sent && c.RawMax != nil && c.RawMax.Cmp(in.DustRaw) > 0 {
			genuine[c.Address] = true
		}
		if c.Dir == received && c.RawMax != nil && c.RawMax.Cmp(in.CapRaw) > 0 {
			genuine[c.Address] = true
		}
	}
	for _, c := range in.Other {
		// The native coin has no USD price here, so any native transfer above
		// native dust, in either direction, makes a genuine counterparty (gas
		// funding is the common case). A poisoner that pays just above native
		// dust becomes "genuine" too: it can then be named as the imitated
		// address instead of the real one, but it never suppresses a flag.
		if c.NativeAboveDust > 0 {
			genuine[c.Address] = true
		}
	}
	delete(genuine, in.Target)
	members := make([]string, 0, len(genuine))
	for a := range genuine {
		members = append(members, a)
	}
	sort.Strings(members)

	best := func(x string) (string, int, int) {
		var who string
		bs, bp := 0, 0
		for _, c := range members {
			s, p := commonSuffix(x, c), commonPrefixAfter(x, c, in.PrefixSkip)
			if s > bs || (s == bs && p > bp) {
				who, bs, bp = c, s, p
			}
		}
		return who, bs, bp
	}

	var out []lookalike
	seen := map[string]bool{}
	add := func(l lookalike) {
		key := l.Address + "|" + l.Kind
		if !seen[key] {
			seen[key] = true
			out = append(out, l)
		}
	}
	for _, c := range in.Asset {
		if c.Dir != received || genuine[c.Address] || c.Address == in.Target {
			continue
		}
		if c.RawMax == nil || c.RawMax.Cmp(in.CapRaw) > 0 {
			continue
		}
		if who, s, p := best(c.Address); s >= in.MinMatch {
			add(lookalike{Address: c.Address, Imitates: who, Kind: "asset_small_transfer", FirstChars: p, LastChars: s,
				Transfers: c.Transfers, Amount: formatUnits(c.Raw, in.Decimals), FirstSeen: c.First, EvidenceTxs: c.Tx})
		}
	}
	for _, c := range in.Other {
		if c.Dir != received || genuine[c.Address] || c.Address == in.Target {
			continue
		}
		if c.NativeDust > 0 {
			if who, s, p := best(c.Address); s >= in.MinMatch {
				add(lookalike{Address: c.Address, Imitates: who, Kind: "native_dust", FirstChars: p, LastChars: s,
					Transfers: c.NativeDust, FirstSeen: c.First, EvidenceTxs: c.Tx})
			}
		}
		if len(c.Imitations) > 0 {
			if who, s, p := best(c.Address); s >= in.MinMatch {
				add(lookalike{Address: c.Address, Imitates: who, Kind: "imitation_token", FirstChars: p, LastChars: s,
					Transfers: c.Transfers, FirstSeen: c.First, EvidenceTxs: c.Tx})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].LastChars != out[j].LastChars {
			return out[i].LastChars > out[j].LastChars
		}
		return out[i].Address < out[j].Address
	})
	return out
}

// passThrough is the pass-through rule's measurement.
type passThrough struct {
	Fires        bool
	Received     *big.Int
	Sent         *big.Int
	OutEarly     *big.Int
	OutEarlyPct  float64
	EndBalance   *big.Int
	FirstReceipt string
}

// passThroughRule: at least outShare of the received amount left within the
// window after the first receipt, and the end balance is under endShare of the
// received amount.
func passThroughRule(cps []counterparty, firstReceipt string, outShare, endShare float64) passThrough {
	p := passThrough{Received: new(big.Int), Sent: new(big.Int), OutEarly: new(big.Int), FirstReceipt: firstReceipt}
	for _, c := range cps {
		if c.Dir == received {
			p.Received.Add(p.Received, c.Raw)
		} else {
			p.Sent.Add(p.Sent, c.Raw)
			if c.RawEarly != nil {
				p.OutEarly.Add(p.OutEarly, c.RawEarly)
			}
		}
	}
	p.EndBalance = new(big.Int).Sub(p.Received, p.Sent)
	if p.Received.Sign() <= 0 {
		return p
	}
	rec, _ := new(big.Float).SetInt(p.Received).Float64()
	early, _ := new(big.Float).SetInt(p.OutEarly).Float64()
	end, _ := new(big.Float).SetInt(p.EndBalance).Float64()
	p.OutEarlyPct = round(100*early/rec, 2)
	p.Fires = early >= outShare*rec && end < endShare*rec
	return p
}
