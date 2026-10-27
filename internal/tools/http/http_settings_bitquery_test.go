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

package http

// Bitquery: a raw-SQL hatch tool (requestBody `{{.sql}}`, the whole body the
// caller's statement) must refuse a caller SETTINGS clause before anything is
// sent, while a templated tool whose server-authored body carries its own
// SETTINGS must still go through unchanged.

import (
	"strings"
	"testing"

	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

func TestBitqueryIsRawSQLPassthrough(t *testing.T) {
	passthrough := []string{
		"{{.sql}}",
		"{{.sql}}\n",
		"  {{ .sql }}  ",
		"{{.a}}{{.b}}",
	}
	for _, b := range passthrough {
		if !bitqueryIsRawSQLPassthrough(b) {
			t.Errorf("%q: not detected as a raw-SQL passthrough", b)
		}
	}
	templated := []string{
		"SELECT name FROM system.tables WHERE database = '{{.database}}'",
		"SELECT 1 FROM eth_api.events WHERE n = {{.n}} SETTINGS max_execution_time = 110",
		"SELECT 1",
		"",
	}
	for _, b := range templated {
		if bitqueryIsRawSQLPassthrough(b) {
			t.Errorf("%q: wrongly detected as a raw-SQL passthrough", b)
		}
	}
}

func bitqueryRawSQLTool(t *testing.T, f *bitqueryFixture) Tool {
	t.Helper()
	return f.tool(t, Config{
		ConfigBase:  tools.ConfigBase{Name: "eth_transfers_raw_sql", Description: "raw sql"},
		Type:        resourceType,
		Source:      "gw-eth",
		Path:        "/",
		Method:      "POST",
		RequestBody: "{{.sql}}\n",
		BodyParams:  parameters.Parameters{parameters.NewStringParameter("sql", "the statement")},
	})
}

func TestBitqueryRawSQLRefusesCallerSettings(t *testing.T) {
	f := newBitqueryFixture(t)
	tl := bitqueryRawSQLTool(t, f)

	refused := []string{
		"SELECT * FROM eth_api.transfers_sender LIMIT 10 SETTINGS max_threads = 32",
		"SELECT * FROM eth_api.transfers_sender SETTINGS max_rows_to_read = 0",
		"select 1 settings max_execution_time = 3600",
		"SELECT * FROM url('http://169.254.169.254/')", // the table-function guard still applies
	}
	for _, sql := range refused {
		_, err := tl.Invoke(f.ctx, f.sources["gw-eth"], parameters.ParamValues{{Name: "sql", Value: sql}}, "")
		if err == nil {
			t.Fatalf("%q: not refused", sql)
		}
		if err.Category() != util.CategoryAgent {
			t.Errorf("%q: error category %v, want agent", sql, err.Category())
		}
		if msg := err.Error(); strings.Contains(msg, "max_threads") || strings.Contains(msg, "169.254") || strings.Contains(msg, "SELECT") {
			t.Errorf("%q: message %q echoes the statement", sql, msg)
		}
	}
	// Nothing reached the backend: the refusal is before the request is built.
	if n := len(f.gateway.calls()); n != 0 {
		t.Fatalf("refused statements sent %d request(s)", n)
	}
}

func TestBitqueryRawSQLAllowsSettingsFreeStatement(t *testing.T) {
	f := newBitqueryFixture(t)
	tl := bitqueryRawSQLTool(t, f)

	sql := "SELECT count() FROM eth_api.transfers_sender WHERE Transfer_Success = 1 LIMIT 10"
	if _, err := tl.Invoke(f.ctx, f.sources["gw-eth"], parameters.ParamValues{{Name: "sql", Value: sql}}, ""); err != nil {
		t.Fatalf("a settings-free statement was refused: %v", err)
	}
	gw := f.gateway.calls()
	if len(gw) != 1 {
		t.Fatalf("gateway got %d calls, want 1", len(gw))
	}
	if !strings.Contains(gw[0].body, sql) {
		t.Errorf("backend body %q does not carry the caller SQL", gw[0].body)
	}
}

// A templated tool's body is server-authored SQL that carries its own SETTINGS.
// It is NOT a passthrough, so the caller-SETTINGS guard never runs on it and the
// statement reaches the backend with its SETTINGS intact (the AT4 regression the
// ticket forbids: server SETTINGS must keep working).
func TestBitqueryTemplatedToolServerSettingsUntouched(t *testing.T) {
	f := newBitqueryFixture(t)
	tl := f.tool(t, Config{
		ConfigBase:  tools.ConfigBase{Name: "eth_flows_between", Description: "templated"},
		Type:        resourceType,
		Source:      "gw-eth",
		Path:        "/",
		Method:      "POST",
		RequestBody: "SELECT count() FROM eth_api.transfers_sender WHERE Block_Date = '{{.day}}'\nSETTINGS max_rows_to_read = 100000000, read_overflow_mode = 'throw', max_execution_time = 110",
		BodyParams:  parameters.Parameters{parameters.NewStringParameter("day", "the UTC date")},
	})

	if _, err := tl.Invoke(f.ctx, f.sources["gw-eth"], parameters.ParamValues{{Name: "day", Value: "2026-09-21"}}, ""); err != nil {
		t.Fatalf("a templated tool with server SETTINGS was refused: %v", err)
	}
	gw := f.gateway.calls()
	if len(gw) != 1 {
		t.Fatalf("gateway got %d calls, want 1", len(gw))
	}
	if !strings.Contains(gw[0].body, "SETTINGS max_rows_to_read = 100000000") {
		t.Errorf("backend body %q lost the server SETTINGS", gw[0].body)
	}
}
