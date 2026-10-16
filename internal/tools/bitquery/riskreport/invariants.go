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

// The invariants of view all. The defaults (max_hops, the deadline, the hub
// cut-off, branch_min_usd) decide how complete a report is; these decide that
// every number in it is true. A report that fails one is not returned.
//
//  1. Conservation: per direction, the attributed amounts plus every cut's
//     unattributed amount equal the analysed total, exactly, in raw units.
//  2. No over-attribution: no link carries more than its history shows.
//  3. History integrity: each replay keeps chain order and its parts add up to
//     the balance; a send above the balance is history_incomplete.
//  4. The two tables agree: measured per hop-1 link and reported, not enforced
//     (the receiver and sender tables are written apart).
//  5. The summary reconciles: the summary's window totals equal the
//     counterparty statement's, and the listed counterparties never exceed it.

import (
	"context"
	"fmt"
	"math/big"
	"time"
)

// searchContext ends the search early enough to assemble and check the report
// before the call's deadline.
func searchContext(ctx context.Context, deadlineSeconds int) (context.Context, context.CancelFunc) {
	reserve := max(time.Second, time.Duration(deadlineSeconds)*time.Second/10)
	if dl, ok := ctx.Deadline(); ok {
		return context.WithDeadline(ctx, dl.Add(-reserve))
	}
	return context.WithCancel(ctx)
}

// reconcile closes the ledger of view all: the counterparty-limit bucket, the
// five invariants, and the cuts in the output.
func reconcile(rep *reportOut, s *search, dirs []direction, edges edgesResult, row assetRow, listed, attributed map[direction]*big.Int, decimals int) error {
	// 5. The summary reconciles.
	for _, d := range []direction{received, sent} {
		sum := row.recv
		if d == sent {
			sum = row.sent
		}
		tot := edges.total[d].raw
		if tot == nil {
			tot = new(big.Int)
		}
		if sum == nil || sum.Cmp(tot) != 0 {
			return fmt.Errorf("invariant failed (summary reconciles): the %s total of the summary (%s) differs from the counterparties' (%s)",
				d, formatUnits(sum, decimals), formatUnits(tot, decimals))
		}
	}
	counts := map[direction]int64{}
	for _, d := range dirs {
		base := edges.total[d].rawAboveMin
		if base == nil {
			base = new(big.Int)
		}
		cpl := new(big.Int).Sub(base, listed[d])
		if cpl.Sign() < 0 {
			return fmt.Errorf("invariant failed (summary reconciles): the listed %s counterparties exceed the direction's total", d)
		}
		s.cuts[d]["counterparty_limit"].Set(cpl)
		counts[d] = max(0, edges.total[d].counterparties-int64(countDir(edges.rows, d)))

		// 1. Conservation.
		sum := new(big.Int).Set(attributed[d])
		for _, r := range cutReasons {
			if s.cuts[d][r].Sign() < 0 {
				return fmt.Errorf("invariant failed (conservation): a negative %s amount", r)
			}
			sum.Add(sum, s.cuts[d][r])
		}
		if sum.Cmp(base) != 0 {
			return fmt.Errorf("invariant failed (conservation): %s: attributed and unattributed add up to %s, the analysed total is %s",
				d, formatUnits(sum, decimals), formatUnits(base, decimals))
		}
	}
	// 2. No over-attribution.
	if err := s.checkLinks(); err != nil {
		return err
	}

	rep.UnattributedByCut = map[string]map[string]string{}
	for _, d := range dirs {
		by := map[string]string{}
		for _, r := range cutReasons {
			by[r] = formatUnits(s.cuts[d][r], decimals)
			n := int64(len(s.cutAddrs[d][r]))
			if r == "counterparty_limit" {
				n = counts[d]
			}
			note := cutNotes[r]
			if r == "hub" {
				unknown := 0
				for a := range s.cutAddrs[d][r] {
					if s.hubUnmeasured[a] {
						unknown++
					}
				}
				if unknown > 0 {
					note += fmt.Sprintf("; %d of them without a size estimate (EXPLAIN gave no granule count), treated as hubs", unknown)
				}
			}
			rep.Params.Cuts = append(rep.Params.Cuts, cutOut{Reason: r, Direction: d.String(), Counterparts: n,
				Amount: formatUnits(s.cuts[d][r], decimals), Note: note})
		}
		rep.UnattributedByCut[d.String()] = by
		if s.cuts[d]["budget"].Sign() > 0 || s.cuts[d]["counterparty_limit"].Sign() > 0 {
			rep.Params.Complete = false
		}
	}
	if s.stopped {
		rep.Params.Complete = false
	}
	rep.Params.Invariants = map[string]string{
		"conservation":        "ok",
		"no_over_attribution": "ok",
		"history_integrity":   "ok",
		"two_tables_agree":    "ok",
		"summary_reconciles":  "ok",
	}
	if n := len(s.incomplete); n > 0 {
		rep.Params.Invariants["history_integrity"] = fmt.Sprintf("ok; %d addresses sent more than their recorded balance, the excess is in history_incomplete", n)
	}
	if s.mismatch > 0 {
		rep.Params.Invariants["two_tables_agree"] = fmt.Sprintf("%d links differ between the receiver and sender tables; the amount is split in proportion, any excess is in history_incomplete", s.mismatch)
	}
	return nil
}
