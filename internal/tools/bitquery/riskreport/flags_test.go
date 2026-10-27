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

import (
	"math/big"
	"testing"
)

func usdt(units float64) *big.Int {
	return usdRaw(units, 1, 6)
}

// The test wallet's USDT counterparties (AT3): three look-alikes paid it small
// amounts right after a genuine payment; one of them (TB8hy...) paid 5.00009
// USDT twice, just above the 5 USD dust line, and must still be found.
func TestFindLookalikesTestWallet(t *testing.T) {
	const target = "TDDaCrpQueZmPZBVouNbZqyZFS3Cnx4QL9"
	asset := []counterparty{
		{Dir: received, Address: "TUrdaveFV4bnTgLxSDkttBMvKSTt36e1A8", Transfers: 1, Raw: usdt(8319100), RawMax: usdt(8319100)},
		{Dir: received, Address: "TB8hyXhttYSok8ARGcTuAzMroYvw7r1QfB", Transfers: 8, Raw: usdt(16.000234), RawMax: usdt(5.00009), Tx: []string{"aa"}},
		{Dir: received, Address: "TRfwwTEJL8LkKCVX8iX2UqNdnbfGVke1uM", Transfers: 2, Raw: usdt(2.44516), RawMax: usdt(1.3), Tx: []string{"bb"}},
		{Dir: received, Address: "TMoA4yBcdTbrBsrH3RoqvVFxigyZdiBttw", Transfers: 1, Raw: usdt(1.21), RawMax: usdt(1.21), Tx: []string{"cc"}},
		{Dir: sent, Address: "TB8hKVScKC9w8QnDnNKJyDwjAPf37rJQfB", Transfers: 3, Raw: usdt(7000000), RawMax: usdt(5000000)},
		{Dir: sent, Address: "TRfwrEAM7X7zGVEj6z6kNQo6a954Vkc1uM", Transfers: 1, Raw: usdt(216149), RawMax: usdt(216149)},
		{Dir: sent, Address: "TPr6F6XitL2UH2NY1motzuSvhn7YdiBttw", Transfers: 1, Raw: usdt(20), RawMax: usdt(20)},
	}
	other := []contact{
		// A native-dust sender matching a genuine counterparty on the last 3.
		{Dir: received, Address: "TXyz1111111111111111111111111rJQfB", Transfers: 1, NativeDust: 1, Tx: []string{"dd"}},
		// A native-dust sender that matches nobody.
		{Dir: received, Address: "TXyz11111111111111111111111111zzzz", Transfers: 1, NativeDust: 1},
	}
	// Gas funding: the wallet received native coin above dust from this
	// address, which makes it genuine, so the four dust senders that copy its
	// last characters are look-alikes of it.
	other = append(other,
		contact{Dir: received, Address: "THWDZvDbRrM27j7k1S224pdaeUZP21j1yK", Transfers: 2, NativeAboveDust: 2},
		contact{Dir: received, Address: "TSvJFfcoYwxgALaJ1FXktN8h26BQh2D1yK", Transfers: 1, NativeDust: 1},
		contact{Dir: received, Address: "TUD8rkxQvVePE5pB97GAX3jnojiCnpq1yK", Transfers: 1, NativeDust: 1},
		contact{Dir: received, Address: "TVLWhvuwaZSDuBBGmbZcikqArX92VKG1yK", Transfers: 1, NativeDust: 1},
		contact{Dir: received, Address: "TGA4rRxJv7HATwXbWYeYevia2ucfQQF1yK", Transfers: 1, NativeDust: 1},
	)
	// These are Tron addresses, so PrefixSkip is 1: the matching first characters
	// are counted after the leading 'T' every address shares.
	got := findLookalikes(poisoningInput{Target: target, Asset: asset, Other: other, DustRaw: usdt(5), CapRaw: usdt(100), MinMatch: 3, PrefixSkip: 1, Decimals: 6})
	want := map[string]struct {
		imitates    string
		kind        string
		first, last int
	}{
		"TB8hyXhttYSok8ARGcTuAzMroYvw7r1QfB": {"TB8hKVScKC9w8QnDnNKJyDwjAPf37rJQfB", "asset_small_transfer", 3, 3},
		"TRfwwTEJL8LkKCVX8iX2UqNdnbfGVke1uM": {"TRfwrEAM7X7zGVEj6z6kNQo6a954Vkc1uM", "asset_small_transfer", 3, 3},
		"TMoA4yBcdTbrBsrH3RoqvVFxigyZdiBttw": {"TPr6F6XitL2UH2NY1motzuSvhn7YdiBttw", "asset_small_transfer", 0, 6},
		"TXyz1111111111111111111111111rJQfB": {"TB8hKVScKC9w8QnDnNKJyDwjAPf37rJQfB", "native_dust", 0, 5},
		"TSvJFfcoYwxgALaJ1FXktN8h26BQh2D1yK": {"THWDZvDbRrM27j7k1S224pdaeUZP21j1yK", "native_dust", 0, 3},
		"TUD8rkxQvVePE5pB97GAX3jnojiCnpq1yK": {"THWDZvDbRrM27j7k1S224pdaeUZP21j1yK", "native_dust", 0, 3},
		"TVLWhvuwaZSDuBBGmbZcikqArX92VKG1yK": {"THWDZvDbRrM27j7k1S224pdaeUZP21j1yK", "native_dust", 0, 3},
		"TGA4rRxJv7HATwXbWYeYevia2ucfQQF1yK": {"THWDZvDbRrM27j7k1S224pdaeUZP21j1yK", "native_dust", 0, 3},
	}
	if len(got) != len(want) {
		t.Fatalf("found %d look-alikes, want %d: %+v", len(got), len(want), got)
	}
	for _, l := range got {
		w, ok := want[l.Address]
		if !ok {
			t.Errorf("unexpected look-alike %s", l.Address)
			continue
		}
		if l.Imitates != w.imitates || l.Kind != w.kind || l.FirstChars != w.first || l.LastChars != w.last {
			t.Errorf("%s: %+v, want %+v", l.Address, l, w)
		}
	}
}

// A report on the intermediary finds the look-alikes of the test wallet
// itself, which the intermediary paid 8.3M.
func TestFindLookalikesOfAPaidAddress(t *testing.T) {
	got := findLookalikes(poisoningInput{
		Target: "TUrdaveFV4bnTgLxSDkttBMvKSTt36e1A8",
		Asset: []counterparty{
			{Dir: sent, Address: "TDDaCrpQueZmPZBVouNbZqyZFS3Cnx4QL9", Transfers: 1, Raw: usdt(8319100), RawMax: usdt(8319100)},
			{Dir: received, Address: "TDDaZJju6iLySe32yV2hCYZzhzZNnx4QL9", Transfers: 1, Raw: usdt(0.800001), RawMax: usdt(0.800001)},
			{Dir: received, Address: "TDDaG2jEyRwrWnumB1ZBD1oSZMFinx4QL9", Transfers: 2, Raw: usdt(1.200002), RawMax: usdt(0.700001)},
			{Dir: received, Address: "TBWaRvoHaPz59sQ9EHCSgxeeV8Nrnx4QL9", Transfers: 1, Raw: usdt(1.01), RawMax: usdt(1.01)},
		},
		DustRaw: usdt(5), CapRaw: usdt(100), MinMatch: 3, Decimals: 6,
	})
	if len(got) != 3 {
		t.Fatalf("found %d, want 3: %+v", len(got), got)
	}
	for _, l := range got {
		if l.Imitates != "TDDaCrpQueZmPZBVouNbZqyZFS3Cnx4QL9" {
			t.Errorf("%s imitates %s", l.Address, l.Imitates)
		}
	}
}

func TestPassThrough(t *testing.T) {
	cps := []counterparty{
		{Dir: received, Raw: usdt(1000)},
		{Dir: sent, Raw: usdt(1000), RawEarly: usdt(996)},
	}
	p := passThroughRule(cps, "2026-07-28 12:22:15", 0.9, 0.01)
	if !p.Fires || p.OutEarlyPct != 99.6 || p.EndBalance.Sign() != 0 {
		t.Errorf("%+v", p)
	}
	cps[1].RawEarly = usdt(500)
	if p := passThroughRule(cps, "", 0.9, 0.01); p.Fires {
		t.Errorf("fired with half out early")
	}
	cps[1] = counterparty{Dir: sent, Raw: usdt(900), RawEarly: usdt(900)}
	if p := passThroughRule(cps, "", 0.9, 0.01); p.Fires {
		t.Errorf("fired with a 10%% end balance")
	}
	// A net spender over a window (sent far exceeds received, a negative end
	// balance): pass-through must NOT fire, and out_within_pct is capped at 100.
	net := []counterparty{
		{Dir: received, Raw: usdt(1552)},
		{Dir: sent, Raw: usdt(6210), RawEarly: usdt(6210)},
	}
	if p := passThroughRule(net, "", 0.9, 0.01); p.Fires || p.OutEarlyPct != 100 || p.EndBalance.Sign() >= 0 {
		t.Errorf("net-spender window: fired or pct not capped: %+v", p)
	}
}

func TestFormatAndConvertUnits(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		dec  int
		want string
	}{{"8319119655394", 6, "8319119.655394"}, {"1", 6, "0.000001"}, {"0", 6, "0.000000"}, {"123", 0, "123"}, {"-5", 2, "-0.05"}} {
		b, _ := new(big.Int).SetString(tc.raw, 10)
		if got := formatUnits(b, tc.dec); got != tc.want {
			t.Errorf("formatUnits(%s, %d) = %s, want %s", tc.raw, tc.dec, got, tc.want)
		}
	}
	if got := usdRaw(100, 1, 6).String(); got != "100000000" {
		t.Errorf("usdRaw(100 USDT) = %s", got)
	}
	if got := usdRaw(5, 0.25, 6).String(); got != "20000000" {
		t.Errorf("usdRaw at 0.25 = %s", got)
	}
	if got := usdRaw(5, 0, 6).Sign(); got != 0 {
		t.Errorf("usdRaw without a price = %d", got)
	}
}

// The largest unnamed counterparties are listed, the rest summed, and nothing
// is lost: listed + summed = every unnamed amount.
func TestUnnamedRowsConserve(t *testing.T) {
	u := map[string]*unnamedOut{
		"TA": {Address: "TA", recvRaw: usdt(300), sentRaw: usdt(0)},
		"TB": {Address: "TB", recvRaw: usdt(100), sentRaw: usdt(50)},
		"TC": {Address: "TC", recvRaw: usdt(0), sentRaw: usdt(7)},
	}
	rows, more := unnamedRows(u, 1, 6)
	if len(rows) != 1 || rows[0].Address != "TA" || rows[0].Received != "300.000000" {
		t.Fatalf("rows: %+v", rows)
	}
	if more == nil || more.Addresses != 2 || more.Received != "100.000000" || more.Sent != "57.000000" {
		t.Fatalf("more: %+v", more)
	}
	if rows, more := unnamedRows(u, 50, 6); len(rows) != 3 || more != nil {
		t.Fatalf("under the limit: %d rows, more %+v", len(rows), more)
	}
}
