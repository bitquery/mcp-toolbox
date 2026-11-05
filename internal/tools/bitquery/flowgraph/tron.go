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

// tronCodec is the Tron chain codec. A Tron address is base58check ('T' + 33
// base58 characters); the raw 20-byte key, the base58 decode/encode and the
// checksum all live in the SQL (ClickHouse base58Decode / base58Encode /
// tryBase58Decode), matching the risk report — there is no Go base58 decoder
// here. Go validates the base58 FORM with a regexp and NEVER lower-cases, because
// base58 is case-sensitive (unlike EVM hex).
type tronCodec struct{}

var (
	// tronAddrRe is the base58check form of a Tron address, and also its output
	// form (the Counterparty the statement base58-encodes).
	tronAddrRe  = regexp.MustCompile(`^T[1-9A-HJ-NP-Za-km-z]{33}$`)
	tronTRC10Re = regexp.MustCompile(`^[0-9]{1,12}$`)
)

func (tronCodec) chain() string                 { return "tron" }
func (tronCodec) valid(a string) bool           { return tronAddrRe.MatchString(a) }
func (tronCodec) normalize(a string) string     { return a } // base58 is case-sensitive
func (tronCodec) outputAddr(s string) bool      { return tronAddrRe.MatchString(s) }
func (tronCodec) supportsIncludeInternal() bool { return false }

// setLiteral turns the validated base58 set into an Array(FixedString(20)) scalar
// the statement reads exactly as it reads the EVM array: each address is
// base58-decoded IN SQL, its 4-byte checksum verified, and a bad one dropped
// (filtered to length 20). A checksum-invalid address therefore leaves the set
// silently; Go has already counted it as used (it passed the form regexp), so the
// used count can exceed the set — reported, never fatal. The result shape matches
// evmCodec.setLiteral so the statement's IN (SELECT arrayJoin(set_a)) is identical.
func (tronCodec) setLiteral(addrs []string) (string, error) {
	if len(addrs) == 0 {
		return "", fmt.Errorf("the address set is empty")
	}
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if !tronAddrRe.MatchString(a) {
			return "", fmt.Errorf("not a Tron address: %q", a)
		}
		parts = append(parts, "'"+a+"'")
	}
	list := "[" + strings.Join(parts, ", ") + "]"
	return "(SELECT arrayDistinct(arrayMap(x -> toFixedString(x, 20), arrayFilter(x -> length(x) = 20, " +
		"arrayMap(x -> if(length(tryBase58Decode(x)) = 25 AND substring(tryBase58Decode(x), 22, 4) = " +
		"substring(SHA256(SHA256(substring(tryBase58Decode(x), 1, 21))), 1, 4), substring(tryBase58Decode(x), 2, 20), ''), " +
		list + ")))))", nil
}

// tokenKey accepts a base58 T-contract (TRC-20) or a TRC-10 numeric id and returns
// the price-store key string unchanged (the base58 contract, or the numeric id) —
// NOT hex. The statement matches it against the Tron token forms by shape (a
// 34-character base58 contract vs a numeric asset id).
func (tronCodec) tokenKey(a string) (string, bool) {
	a = strings.TrimSpace(a)
	switch {
	case tronAddrRe.MatchString(a):
		return a, true
	case tronTRC10Re.MatchString(a):
		return a, true
	}
	return "", false
}

func init() { registerCodec(tronCodec{}) }
