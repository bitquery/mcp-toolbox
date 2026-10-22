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

func TestBitqueryForbiddenTableFunctionRefuses(t *testing.T) {
	cases := map[string]string{
		// the shapes seen in production
		"SELECT * FROM url('https://ifconfig.me/ip','RawBLOB','d String')":                                                                                      "url",
		"SELECT * FROM url('http://172.16.121.1:8123/ping','LineAsString','x String')\n":                                                                        "url",
		"SELECT * FROM redis('172.16.121.1:6379','k','k String')\n":                                                                                             "redis",
		"SELECT * FROM url('https://x.example/api', 'RawBLOB', 'd String', 'none', headers('Authorization' = 'Bearer t')) SETTINGS max_http_get_redirects = 10": "url",
		// variants and evasions
		"select * from URL('http://a')":                                "URL",
		"SELECT * FROM url ( 'http://a' )":                             "url",
		"SELECT * FROM url\n(\n'http://a')":                            "url",
		"SELECT * FROM url/* c */('http://a')":                         "url",
		"SELECT * FROM url--c\n('http://a')":                           "url",
		"SELECT * FROM `url`('http://a')":                              "url",
		"SELECT * FROM \"url\"('http://a')":                            "url",
		"SELECT * FROM urlCluster('c','http://a')":                     "urlCluster",
		"SELECT * FROM remote('ch-eth-api:9000', db.t)":                "remote",
		"SELECT * FROM remoteSecure('h', db.t)":                        "remoteSecure",
		"SELECT * FROM cluster('c', system.query_log)":                 "cluster",
		"SELECT * FROM clusterAllReplicas('c', system.one)":            "clusterAllReplicas",
		"SELECT * FROM mysql('h:3306','db','t','u','p')":               "mysql",
		"SELECT * FROM postgresql('h:5432','db','t','u','p')":          "postgresql",
		"SELECT * FROM mongodb('h:27017','db','c','u','p','')":         "mongodb",
		"SELECT * FROM sqlite('/tmp/x.db','t')":                        "sqlite",
		"SELECT * FROM s3('https://b.s3.amazonaws.com/k')":             "s3",
		"SELECT * FROM s3Cluster('c','https://b/k')":                   "s3Cluster",
		"SELECT * FROM gcs('https://storage.googleapis.com/b/k')":      "gcs",
		"SELECT * FROM azureBlobStorage('c','cont','blob')":            "azureBlobStorage",
		"SELECT * FROM hdfs('hdfs://h/p')":                             "hdfs",
		"SELECT * FROM file('/etc/passwd','LineAsString')":             "file",
		"SELECT * FROM executable('cat /etc/passwd','TSV','x String')": "executable",
		"SELECT * FROM iceberg('https://b/p')":                         "iceberg",
		"SELECT * FROM deltaLake('https://b/p')":                       "deltaLake",
		"SELECT * FROM hudi('https://b/p')":                            "hudi",
		"INSERT INTO FUNCTION url('http://a', 'JSON') SELECT 1":        "url",
		"WITH 1 AS x SELECT * FROM (SELECT * FROM url('http://a'))":    "url",
		"SELECT 1 UNION ALL SELECT count() FROM remote('h', db.t)":     "remote",
	}
	for sql, want := range cases {
		got, ok := BitqueryForbiddenTableFunction(sql)
		if !ok {
			t.Errorf("%q: not refused, want %s", sql, want)
			continue
		}
		if got != want {
			t.Errorf("%q: refused as %q, want %q", sql, got, want)
		}
	}
}

func TestBitqueryForbiddenTableFunctionAllows(t *testing.T) {
	cases := []string{
		"SELECT 1",
		"SELECT * FROM trading_rt.trades_by_token_address WHERE Pair_Token_Address = '0xabc' LIMIT 10",
		// forbidden names inside string literals and comments are data, not calls
		"SELECT 'url(' AS s",
		"SELECT 'SELECT * FROM url(''http://a'')' AS s",
		"SELECT 'it''s url(x)' AS s",
		`SELECT 'a\'url(' AS s`,
		"SELECT $$ url('http://a') $$ AS s",
		"SELECT $heredoc$ remote('h', t) $heredoc$ AS s",
		"-- url('http://a')\nSELECT 1",
		"# redis('h:6379','k','k String')\nSELECT 1",
		"/* s3('https://b/k') */ SELECT 1",
		// a column or alias named like a function is not a call
		"SELECT url, file FROM eth_api.events WHERE url != ''",
		"SELECT t.url AS u FROM db.t",
		"WITH url AS (SELECT 1 AS x) SELECT x FROM url",
		"SELECT count() AS s3 FROM db.t",
		// legitimate functions that contain a forbidden name as a prefix / suffix
		"SELECT domain(url), URLHash(url), cutURLParameter(url, 'x'), URLHierarchy(url) FROM db.t",
		"SELECT filesystemAvailable(), fileCount FROM db.t",
		"SELECT * FROM my_url_table",
		"SELECT * FROM remote_table_name LIMIT 1",
		"SELECT * FROM db.cluster_stats",
		// benign table functions
		"SELECT * FROM numbers(10)",
		"SELECT * FROM values('x UInt8', (1), (2))",
		"SELECT dictGetOrNull('marketdata.prices_eth_dict', 'price', tuple('a', 1))",
		"SELECT * FROM dictionary('marketdata.prices_eth_dict')",
		"SELECT * FROM generateRandom('x UInt8') LIMIT 1",
		"SELECT * FROM merge('db', '^t_')",
		"SELECT * FROM view(SELECT 1)",
		"SELECT * FROM system.one",
	}
	for _, sql := range cases {
		if got, ok := BitqueryForbiddenTableFunction(sql); ok {
			t.Errorf("%q: refused as %q, want allowed", sql, got)
		}
	}
}

func TestBitqueryForbiddenTableFunctionUnterminatedTokens(t *testing.T) {
	// A token that never closes swallows the rest of the text; the check must
	// still terminate and must not refuse what is inside it (ClickHouse itself
	// would reject the statement as a parse error).
	for _, sql := range []string{
		"SELECT 'url(http://a)",
		"SELECT `url(",
		"SELECT /* url('http://a')",
		"SELECT $$ url('http://a')",
	} {
		if got, ok := BitqueryForbiddenTableFunction(sql); ok {
			t.Errorf("%q: refused as %q, want allowed", sql, got)
		}
	}
	// ... unless the call sits before the unterminated token.
	if _, ok := BitqueryForbiddenTableFunction("SELECT * FROM url('http://a') WHERE x = 'open"); !ok {
		t.Errorf("call before an unterminated literal was not refused")
	}
}

func TestBitqueryTableFunctionRefusedMessage(t *testing.T) {
	err := BitqueryTableFunctionRefused("url")
	if err.Category() != CategoryAgent {
		t.Fatalf("refusal is %s, want %s", err.Category(), CategoryAgent)
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "invalid request: ") || !strings.Contains(msg, "url()") {
		t.Fatalf("unexpected message %q", msg)
	}
	for _, leak := range []string{"http", "172.", "SELECT"} {
		if strings.Contains(msg, leak) {
			t.Fatalf("message %q echoes the statement (%q)", msg, leak)
		}
	}
}
