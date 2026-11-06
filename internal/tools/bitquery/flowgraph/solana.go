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
)

// solanaCodec is the Solana chain codec. Solana is account-based like Tron: the
// transfer tables (solana.transfers_from / solana.transfers_to) carry the OWNER
// base58 addresses in transfer_from / transfer_to, so a counterparty is read
// verbatim — there is no decode (unlike EVM hex or Tron base58-check) and the SQL
// reads the address as a plain string. Go validates the base58 FORM with a regexp
// and NEVER lower-cases (base58 is case-sensitive). The token identity is the
// numeric currency_id, not a token account, so tokenKey refuses a token id: the
// token filter is not supported on Solana in v1.
type solanaCodec struct{}

// solAddrRe is a Solana base58 pubkey (32–44 base58 characters). It is both the
// input validator AND the Counterparty output form (the statement emits the owner
// address verbatim). The base58 alphabet has no quote or backslash, so the set
// literal is safe to quote.
var solAddrRe = regexp.MustCompile(`^[1-9A-HJ-NP-Za-km-z]{32,44}$`)

func (solanaCodec) chain() string                 { return "solana" }
func (solanaCodec) valid(a string) bool           { return solAddrRe.MatchString(a) }
func (solanaCodec) normalize(a string) string     { return a } // base58 is case-sensitive
func (solanaCodec) outputAddr(s string) bool      { return solAddrRe.MatchString(s) }
func (solanaCodec) supportsIncludeInternal() bool { return false }

// legacyCluster reports that ch-solana runs an old ClickHouse (CH 20.8) that does
// not know short_circuit_function_evaluation, so the engine omits that setting —
// the same reason the UTXO clusters are legacy. Solana is account-based, not UTXO,
// so it declares this rather than implementing utxoChain.
func (solanaCodec) legacyCluster() bool { return true }

// setLiteral writes the validated base58 set as a plain quoted comma list, e.g.
// `'5tz…'` (one address) or `'a', 'b'`. The statement wraps it as
// `transfer_from IN identity([{{.SetLiteral}}])` — the identity() barrier makes the
// constant array a pre-computed set on the initiator, so the distributed transfer
// tables are not re-scanned per shard (distributed_product_mode). Every member is
// re-checked here, so a value that slipped past parse cannot reach the SQL.
func (solanaCodec) setLiteral(addrs []string) (string, error) {
	if len(addrs) == 0 {
		return "", fmt.Errorf("the address set is empty")
	}
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if !solAddrRe.MatchString(a) {
			return "", fmt.Errorf("not a Solana address: %q", a)
		}
		parts = append(parts, "'"+a+"'")
	}
	return strings.Join(parts, ", "), nil
}

// tokenKey refuses every token id: on Solana the token identity is the numeric
// currency_id (not a base58 token account), and the per-token filter is not built
// in v1, so parse reports "token must be a solana token id".
func (solanaCodec) tokenKey(string) (string, bool) { return "", false }

func init() { registerCodec(solanaCodec{}) }
