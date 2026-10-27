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

// The raw-SQL hatch tools run as a readonly=2 ClickHouse user, which lets a
// SETTINGS clause in the CALLER's statement raise or (with `= 0`) remove the
// limits the server relies on. BitqueryRefuseCallerSQL refuses the clause's
// presence — a value-based allowlist is a trap because `= 0` means "no limit".

func TestBitqueryRefuseCallerSQLRefusesSettings(t *testing.T) {
	cases := []string{
		// the measured abuses: a raised limit and the `= 0` removal
		"SELECT * FROM eth_api.transfers_sender LIMIT 10 SETTINGS max_threads = 32",
		"SELECT count() FROM eth_api.transfers_sender SETTINGS max_execution_time = 3600",
		"SELECT * FROM eth_api.transfers_sender SETTINGS max_result_rows = 1000000",
		"SELECT * FROM eth_api.transfers_sender SETTINGS max_memory_usage = 50000000000",
		// `= 0` is the trap a value cap would miss: it means "no limit"
		"SELECT * FROM eth_api.transfers_sender SETTINGS max_rows_to_read = 0",
		"SELECT * FROM eth_api.transfers_sender SETTINGS max_execution_time = 0, max_result_rows = 0",
		// lowercase, extra whitespace forms (ClickHouse is case-insensitive)
		"select 1 settings max_threads = 32",
		"SELECT 1\nSETTINGS\tmax_threads = 32",
		"SELECT 1 SETTINGS  max_threads=32",
		// inside a subquery, not only at the top level
		"SELECT * FROM (SELECT * FROM eth_api.transfers_sender SETTINGS max_rows_to_read = 0)",
		// a quoted identifier cannot be the SETTINGS clause keyword; dropping the
		// quotes leaves the bare keyword, and the guard errs toward refusing it
		// (a column that must be named SETTINGS can be aliased).
		"SELECT `SETTINGS` FROM eth_api.transfers_sender",
	}
	for _, sql := range cases {
		refusal := BitqueryRefuseCallerSQL(sql)
		if refusal == nil {
			t.Errorf("%q: not refused, want a SETTINGS refusal", sql)
			continue
		}
		if refusal.Category() != CategoryAgent {
			t.Errorf("%q: refusal is %s, want %s", sql, refusal.Category(), CategoryAgent)
		}
		msg := refusal.Error()
		if !strings.HasPrefix(msg, "invalid request: ") || !strings.Contains(msg, "SETTINGS") {
			t.Errorf("%q: unexpected message %q", sql, msg)
		}
		// The message must not echo the caller's statement.
		for _, leak := range []string{"max_threads", "max_execution_time", "32", "3600", "transfers_sender"} {
			if strings.Contains(msg, leak) {
				t.Errorf("%q: message %q echoes the statement (%q)", sql, msg, leak)
			}
		}
	}
}

func TestBitqueryRefuseCallerSQLAllows(t *testing.T) {
	cases := []string{
		"SELECT 1",
		"SELECT * FROM eth_api.transfers_sender WHERE Transfer_Success = 1 LIMIT 10",
		// the word lives inside a string literal / comment / heredoc — data, not a clause
		"SELECT 'SETTINGS' AS s",
		"SELECT 'x SETTINGS max_threads = 32' AS note",
		"-- SETTINGS max_threads = 32\nSELECT 1",
		"# SETTINGS max_threads = 32\nSELECT 1",
		"/* SETTINGS max_threads = 32 */ SELECT 1",
		"SELECT $$ SETTINGS max_threads = 32 $$ AS s",
		// the SETTINGS substring sits inside a longer identifier, so \b does not fire
		"SELECT my_settings, settings_v2 FROM eth_api.events",
		"SELECT count() AS settings_count FROM eth_api.events",
	}
	for _, sql := range cases {
		if refusal := BitqueryRefuseCallerSQL(sql); refusal != nil {
			t.Errorf("%q: refused (%q), want allowed", sql, refusal.Error())
		}
	}
}

// BitqueryRefuseCallerSQL still refuses what BitqueryRefuseSQL does, so a raw-SQL
// hatch keeps the table-function and escaped-identifier guards too.
func TestBitqueryRefuseCallerSQLStillRefusesTableFunctions(t *testing.T) {
	for _, sql := range []string{
		"SELECT * FROM url('http://169.254.169.254/')",
		"SELECT * FROM remote('ch-eth-api:9000', db.t)",
		"SELECT `a\\tb` FROM eth_api.transfers_sender",
	} {
		if refusal := BitqueryRefuseCallerSQL(sql); refusal == nil {
			t.Errorf("%q: not refused, want a refusal", sql)
		}
	}
}

// The three fixtures the ticket calls for. Each is a SERVER-authored statement
// that carries a legitimate SETTINGS clause: it MUST pass the source-level check
// (BitqueryRefuseSQL, which has no SETTINGS rule), proving the fix does not break
// the server's own per-query settings. The same text with a caller SETTINGS
// appended MUST be refused by the caller-only check.
func TestBitqueryServerSettingsStatementsPassTheSourceCheck(t *testing.T) {
	serverStatements := map[string]string{
		// 26.8-style, lifted from a templated transfer tool (transfers_eth.yaml):
		// modern settings, no in-SQL date formatting.
		"26.8 templated transfer tool": "SELECT concat('0x', lower(hex(Transfer_Sender))) AS s\n" +
			"FROM eth_api.transfers_sender\n" +
			"WHERE Transfer_Receiver = unhex('abcd')\n" +
			"SETTINGS max_rows_to_read = 100000000, read_overflow_mode = 'throw', max_execution_time = 110",
		// 20.8-style UTXO statement (transfers_btc.yaml shape): dates formatted in
		// SQL, FORMAT JSONEachRow, no modern per-query settings at all.
		"20.8 UTXO statement (no settings)": "SELECT address, formatDateTime(tx_time, '%FT%TZ') AS t, toString(value) AS v\n" +
			"FROM bitcoin.outputs_by_address\n" +
			"WHERE address = 'bc1qexampleexampleexampleexampleexample'\n" +
			"LIMIT 100\n" +
			"FORMAT JSONEachRow",
		// address_risk_report engine shape: the exact clause settings() emits in
		// internal/tools/bitquery/riskreport/tron.go (func settings + stopSettings).
		"risk-report engine statement": "SELECT Native, Symbol, toString(recv_w) AS Received\n" +
			"FROM tron_api.transfers_sender\n" +
			"WHERE Transfer_Sender = unhex('41abcd')\n" +
			"SETTINGS max_rows_to_read = 1000000, read_overflow_mode = 'throw', max_result_rows = 200, " +
			"result_overflow_mode = 'throw', max_execution_time = 110, " +
			"timeout_overflow_mode = 'throw', cancel_http_readonly_queries_on_client_close = 1\n" +
			"FORMAT JSON",
	}
	for name, stmt := range serverStatements {
		// The source runs BitqueryRefuseSQL on every statement, server-built ones
		// included; it must not refuse a legitimate SETTINGS clause.
		if refusal := BitqueryRefuseSQL(stmt); refusal != nil {
			t.Errorf("%s: BitqueryRefuseSQL refused a server statement (%q); server SETTINGS would be broken", name, refusal.Error())
		}
		// The 20.8 statement carries no SETTINGS, so even the caller-only check
		// passes it. The two with a SETTINGS clause must be refused once the SAME
		// text is sent by a caller through a raw-SQL hatch — then it IS caller text,
		// whatever its resemblance to a statement the server builds for itself.
		if !strings.Contains(strings.ToUpper(stmt), "SETTINGS") {
			if refusal := BitqueryRefuseCallerSQL(stmt); refusal != nil {
				t.Errorf("%s: a settings-free statement was refused as caller SQL (%q)", name, refusal.Error())
			}
			continue
		}
		if refusal := BitqueryRefuseCallerSQL(stmt); refusal == nil {
			t.Errorf("%s: the same text would NOT be refused if a caller sent it as raw SQL", name)
		}
	}
}
