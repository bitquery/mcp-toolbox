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
	"fmt"
	"regexp"
	"strings"
	"time"
)

// utxoCodec is the codec of a single-coin UTXO chain (Bitcoin, Zcash transparent).
// Unlike the account chains (EVM, Tron) the statement works ONE address at a time:
// its pro-rata attribution (Own_Share = the address's inputs / the transaction's
// inputs) is per address, so a set of N addresses is N statements merged in Go —
// not built here yet, so the engine refuses a set of more than one on a UTXO chain
// (singleAddressOnly). The address is a plain base58/bech32 string that the SQL
// reads verbatim (no decode, unlike Tron base58 or EVM hex), so the codec validates
// its FORM with a regexp, never lower-cases (base58 is case-sensitive; bech32 is
// already lower), and writes the set as a plain quoted IN-list. There is no token:
// the chain has one native coin, so tokenKey refuses every token id.
type utxoCodec struct {
	name    string
	re      *regexp.Regexp
	genesis time.Time
}

var (
	// btcAddrRe is a Bitcoin address FORM: P2PKH/P2SH base58 (1.../3...) or bech32
	// (bc1...). It is the input validator AND the Counterparty output form (the
	// statement emits the address verbatim).
	btcAddrRe = regexp.MustCompile(`^([13][1-9A-HJ-NP-Za-km-z]{25,34}|bc1[0-9ac-hj-np-z]{11,71})$`)
	// zecTAddrRe is a Zcash TRANSPARENT address (t1.../t3...). Shielded (zs.../z...),
	// unified (u1...), TEX (tex1...) and the zcash_flow `d-<hex>` shielded-output
	// placeholder are all refused: the tool covers transparent counterparties only.
	zecTAddrRe = regexp.MustCompile(`^t[13][1-9A-HJ-NP-Za-km-z]{33}$`)
)

func (c utxoCodec) chain() string               { return c.name }
func (c utxoCodec) valid(a string) bool         { return c.re.MatchString(a) }
func (utxoCodec) normalize(a string) string     { return a } // base58/bech32: case-sensitive, never lower
func (c utxoCodec) outputAddr(s string) bool    { return c.re.MatchString(s) }
func (utxoCodec) supportsIncludeInternal() bool { return false }

// setLiteral writes the validated address set as a plain quoted IN-list, e.g.
// `'34xp4v…'` (one address) or `'a','b'`. The statement uses it as
// `address IN ({{.SetLiteral}})` (the set's own spends/receipts) and
// `address NOT IN ({{.SetLiteral}})` (excluding the set from its counterparties).
// The regexp charset is base58/bech32 only — no quote, no backslash — so quoting is
// safe; every member is re-checked here, so a value that slipped past parse cannot
// reach the SQL. The engine refuses a set of more than one on a UTXO chain today
// (the per-address fan-out is not built), so this renders one element in practice.
func (c utxoCodec) setLiteral(addrs []string) (string, error) {
	if len(addrs) == 0 {
		return "", fmt.Errorf("the address set is empty")
	}
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if !c.re.MatchString(a) {
			return "", fmt.Errorf("not a %s address: %q", c.name, a)
		}
		parts = append(parts, "'"+a+"'")
	}
	return strings.Join(parts, ", "), nil
}

// tokenKey refuses every token id: a UTXO chain has one native coin, so the token
// parameter does not apply (parse then reports "token must be a <chain> token id").
func (utxoCodec) tokenKey(string) (string, bool) { return "", false }

// genesisFloor is the default window start for a UTXO chain: its spends and receipts
// predate the 90-day default by years, so a call with no after_time reads the whole
// history (bounded by max_txs), not an empty last-90-days window. Only utxoCodec
// implements utxoChain, so the engine detects a UTXO chain by this interface.
func (c utxoCodec) genesisFloor() time.Time { return c.genesis }

// utxoChain is implemented only by utxoCodec. The engine uses it to key the two UTXO
// behaviours on the codec (not a chain-name compare): the genesis window floor, and
// the one-address-per-call limit until the per-address fan-out lands.
type utxoChain interface {
	genesisFloor() time.Time
}

func init() {
	// Bitcoin genesis 2009-01-03; Zcash launch 2016-10-28. The floor is a safe lower
	// bound (a transfer cannot predate it), used only when no after_time is given.
	registerCodec(utxoCodec{name: "bitcoin", re: btcAddrRe, genesis: time.Date(2009, 1, 1, 0, 0, 0, 0, time.UTC)})
	registerCodec(utxoCodec{name: "zcash", re: zecTAddrRe, genesis: time.Date(2016, 10, 1, 0, 0, 0, 0, time.UTC)})
}
