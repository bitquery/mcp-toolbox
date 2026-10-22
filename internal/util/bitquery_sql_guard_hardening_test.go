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

package util

import (
	"strings"
	"testing"
)

// BitqueryRefuseSQL closes the evasions a name-and-paren check alone would let
// through, and keeps catching plain forbidden calls.
func TestBitqueryRefuseSQLRefuses(t *testing.T) {
	cases := map[string]string{
		// plain forbidden calls still refused through the full entry point
		"SELECT * FROM url('http://169.254.169.254/latest/meta-data/')": "url()",
		"SELECT * FROM redis('172.16.1.1:6379','k','k String')":         "redis()",
		// a vertical tab is whitespace to ClickHouse but not to Go's \s
		"SELECT * FROM url\v('http://a')":    "url()",
		"SELECT * FROM url\v\v ('http://a')": "url()",
		// a backslash escape in a quoted identifier can hide the name from a
		// textual check; such an identifier used as a call is refused outright
		"SELECT * FROM `\\x75rl`('http://a')":     "escaped identifier",
		"SELECT * FROM \"\\x72emote\"('h', db.t)": "escaped identifier",
		// a per-query setting whose value is parsed as SQL can carry a forbidden
		// call inside a string literal this check strips
		"SELECT 1 SETTINGS additional_table_filters = {'db.t': '1 IN (SELECT 1 FROM url(''http://a''))'}": "additional_table_filters",
		"SELECT 1 SETTINGS additional_result_filter = '1 IN (SELECT 1 FROM remote(''h'', db.t))'":         "additional_result_filter",
		// the growing lake / object-store families are matched by prefix
		"SELECT * FROM s3Cluster('c','https://b/k')":               "s3Cluster()",
		"SELECT * FROM icebergLocalCluster('https://b/p')":         "icebergLocalCluster()",
		"SELECT * FROM deltaLakeS3Cluster('c','https://b/p')":      "deltaLakeS3Cluster()",
		"SELECT * FROM azureBlobStorageCluster('c','cont','blob')": "azureBlobStorageCluster()",
		"SELECT * FROM hive('thrift://h:9083','db','t')":           "hive()",
		"SELECT * FROM hudiCluster('c','https://b/p')":             "hudiCluster()",
	}
	for sql, want := range cases {
		refusal := BitqueryRefuseSQL(sql)
		if refusal == nil {
			t.Errorf("%q: not refused, want %s", sql, want)
			continue
		}
		if !strings.Contains(refusal.Error(), want) {
			t.Errorf("%q: message %q does not name %s", sql, refusal.Error(), want)
		}
		for _, leak := range []string{"169.254", "172.16", "http", "db.t", "SELECT", "thrift"} {
			if strings.Contains(refusal.Error(), leak) {
				t.Errorf("%q: message %q echoes the statement (%q)", sql, refusal.Error(), leak)
			}
		}
	}
}

func TestBitqueryRefuseSQLAllows(t *testing.T) {
	cases := []string{
		"SELECT 1",
		"SELECT * FROM trading_rt.trades_by_token_address LIMIT 10",
		// a scalar whose name starts with a forbidden prefix but is not that call
		"SELECT filesystemAvailable(), filesystemCapacity() FROM db.t",
		"SELECT domain(url), URLHash(url) FROM eth_api.events",
		// a forbidden name inside a literal, comment or heredoc is data
		"SELECT 'url(' AS s",
		"SELECT $$ url('http://a') $$ AS s",
		"-- additional_table_filters\nSELECT 1",
		"/* s3Cluster('x') */ SELECT 1",
		// a column or alias named like a function
		"SELECT url, s3 FROM db.t WHERE url != ''",
		"SELECT count() AS s3 FROM db.t",
	}
	for _, sql := range cases {
		if refusal := BitqueryRefuseSQL(sql); refusal != nil {
			t.Errorf("%q: refused (%q), want allowed", sql, refusal.Error())
		}
	}
}

// A closed quoted identifier with a backslash is refused whatever follows (it
// could decode to a forbidden name); an UNterminated one is not a closed token,
// so the strip pass drops it and the statement — which ClickHouse rejects as a
// parse error anyway — is allowed through to the database.
func TestBitqueryRefuseSQLEscapedIdentNotACall(t *testing.T) {
	if refusal := BitqueryRefuseSQL("SELECT `a\\tb` FROM db.t"); refusal == nil {
		t.Errorf("a backslash in a quoted identifier was allowed, want refused")
	}
	for _, sql := range []string{
		"SELECT * FROM `\\x75rl",
		"SELECT * FROM \"\\x75rl('http://a')",
	} {
		if refusal := BitqueryRefuseSQL(sql); refusal != nil {
			t.Errorf("%q: refused (%q), want allowed", sql, refusal.Error())
		}
	}
}
