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
	"strings"
	"testing"
	"text/template"

	"github.com/googleapis/mcp-toolbox/internal/tools"
)

func TestEvmSetLiteral(t *testing.T) {
	c, ok := codecFor("ethereum")
	if !ok {
		t.Fatal("no ethereum codec registered")
	}
	// A mixed-case address is lower-cased into the literal; unhex is
	// case-insensitive but the statement text must be stable.
	got, err := c.setLiteral([]string{"0x9008D19f58AAbD9eD0D60971565AA8510560ab41"})
	if err != nil {
		t.Fatalf("setLiteral: %v", err)
	}
	want := "[toFixedString(unhex('9008d19f58aabd9ed0d60971565aa8510560ab41'),20)]"
	if got != want {
		t.Fatalf("setLiteral = %q, want %q", got, want)
	}
	// Two addresses → two array elements.
	got2, err := c.setLiteral([]string{"0x" + strings.Repeat("a", 40), "0x" + strings.Repeat("B", 40)})
	if err != nil {
		t.Fatalf("setLiteral two: %v", err)
	}
	if n := strings.Count(got2, "toFixedString"); n != 2 {
		t.Fatalf("two-address literal has %d elements: %q", n, got2)
	}
	// Invalid and empty are refused — a bad address never reaches a statement.
	if _, err := c.setLiteral([]string{"not-an-address"}); err == nil {
		t.Fatal("expected an error on an invalid address")
	}
	if _, err := c.setLiteral(nil); err == nil {
		t.Fatal("expected an error on an empty set")
	}
}

func TestEvmCodecAliases(t *testing.T) {
	for _, chain := range []string{"ethereum", "bsc", "matic", "arbitrum", "base", "optimism", "robinhood", "arc"} {
		if _, ok := codecFor(chain); !ok {
			t.Errorf("no codec for EVM chain %q", chain)
		}
	}
	if _, ok := codecFor("dogecoin"); ok {
		t.Error("codecFor returned a codec for an unsupported chain")
	}
}

func TestRender(t *testing.T) {
	tmpl, err := template.New("x").Option("missingkey=error").Parse(
		"WITH {{.SetLiteral}} AS set_a, {{.WStart}} AS w_start, {{.WEnd}} AS w_end, " +
			"{{.IncludeInternal}} AS inc, {{.TopK}} AS k, {{.MinUsd}} AS mu SELECT 1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := render(tmpl, stmtVars{
		SetLiteral:      "[toFixedString(unhex('aa'),20)]",
		WStart:          "toDateTime('2026-08-05 00:00:00','UTC')",
		WEnd:            "toDateTime('2026-11-03 00:00:00','UTC')",
		IncludeInternal: 1,
		TopK:            1000,
		MinUsd:          "0",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		"[toFixedString(unhex('aa'),20)] AS set_a",
		"1 AS inc", "1000 AS k", "0 AS mu",
		"toDateTime('2026-08-05 00:00:00','UTC') AS w_start",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("render missing %q in:\n%s", want, out)
		}
	}
}

func TestParseStatements(t *testing.T) {
	base := tools.ConfigBase{Name: "cp"}
	ok := Config{ConfigBase: base,
		ChainSources: map[string]string{"ethereum": "eth-src"},
		Statements:   map[string]string{"ethereum": "SELECT {{.TopK}} AS k, {{.SetLiteral}} AS s"}}
	if _, err := parseStatements(ok); err != nil {
		t.Fatalf("valid cfg: %v", err)
	}
	// A chain with no codec is refused at load.
	noCodec := Config{ConfigBase: base,
		ChainSources: map[string]string{"nope": "s"},
		Statements:   map[string]string{"nope": "SELECT 1"}}
	if _, err := parseStatements(noCodec); err == nil {
		t.Fatal("expected an error for a chain with no codec")
	}
	// A statement for a chain not in chainSources is refused.
	noSource := Config{ConfigBase: base,
		ChainSources: map[string]string{},
		Statements:   map[string]string{"ethereum": "SELECT 1"}}
	if _, err := parseStatements(noSource); err == nil {
		t.Fatal("expected an error for a chain not in chainSources")
	}
	// A malformed template (unclosed action) is refused at parse.
	badParse := Config{ConfigBase: base,
		ChainSources: map[string]string{"ethereum": "s"},
		Statements:   map[string]string{"ethereum": "SELECT {{.TopK"}}
	if _, err := parseStatements(badParse); err == nil {
		t.Fatal("expected an error for an unclosed template action")
	}
	// A template that reads a variable stmtVars does not have is caught by the
	// trial render at load, not on every call.
	badField := Config{ConfigBase: base,
		ChainSources: map[string]string{"ethereum": "s"},
		Statements:   map[string]string{"ethereum": "SELECT {{.Nope}}"}}
	if _, err := parseStatements(badField); err == nil {
		t.Fatal("expected an error for an unknown template field")
	}
}
