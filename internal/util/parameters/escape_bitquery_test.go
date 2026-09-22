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

package parameters_test

// Bitquery: `escape: single-quotes` must render a literal that ClickHouse
// decodes back to exactly the input, whatever the input holds. ClickHouse
// honours backslash escapes inside '...', so a backslash must be doubled as
// well as a quote; otherwise x\' OR 1=1 -- closes the literal early.

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/go-cmp/cmp"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

// singleQuoteEscapeCases: input -> rendered literal.
var singleQuoteEscapeCases = []struct {
	name string
	in   string
	want string
}{
	{"plain", "pepe", `'pepe'`},
	{"empty", "", `''`},
	{"apostrophe", "o'neil", `'o''neil'`},
	{"only an apostrophe", "'", `''''`},
	{"backslash", `a\b`, `'a\\b'`},
	{"only a backslash", `\`, `'\\'`},
	{"trailing backslash", `abc\`, `'abc\\'`},
	{"two backslashes", `\\`, `'\\\\'`},
	{"backslash quote", `a\'b`, `'a\\''b'`},
	{"injection via backslash quote", `x\' OR 1=1 --`, `'x\\'' OR 1=1 --'`},
	{"injection via quote", `val' OR 1=1--`, `'val'' OR 1=1--'`},
	{"backslash then closing attempt", `\'); DROP TABLE t; --`, `'\\''); DROP TABLE t; --'`},
	{"typed escape sequence stays literal", `line\nbreak\x41\0`, `'line\\nbreak\\x41\\0'`},
	{"regex class", `\d+`, `'\\d+'`},
	{"like escape", `100\%`, `'100\\%'`},
	{"unicode", "Ünïcødé 🐸 доге", `'Ünïcødé 🐸 доге'`},
	{"unicode with quote and backslash", `日本'語\`, `'日本''語\\'`},
}

func TestSingleQuoteEscapeParse(t *testing.T) {
	for _, tc := range singleQuoteEscapeCases {
		t.Run(tc.name, func(t *testing.T) {
			p := parameters.NewStringParameter("q", "q", parameters.WithStringEscape("single-quotes"))
			got, err := p.Parse(tc.in)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("Parse(%q) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// Every rendered literal must end exactly at its last byte (nothing after the
// closing quote is SQL) and decode to the input under ClickHouse's rules.
func TestSingleQuoteEscapeRoundTripsThroughClickHouseLiteral(t *testing.T) {
	inputs := []string{}
	for _, tc := range singleQuoteEscapeCases {
		inputs = append(inputs, tc.in)
	}
	// every short string over the characters that matter
	alphabet := []string{`\`, `'`, "a", "n", "0", "x", "%", " "}
	var gen func(prefix string, depth int)
	gen = func(prefix string, depth int) {
		inputs = append(inputs, prefix)
		if depth == 0 {
			return
		}
		for _, c := range alphabet {
			gen(prefix+c, depth-1)
		}
	}
	gen("", 4)

	p := parameters.NewStringParameter("q", "q", parameters.WithStringEscape("single-quotes"))
	for _, in := range inputs {
		got, err := p.Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		lit := got.(string)
		decoded, rest, err := decodeClickHouseStringLiteral(lit)
		if err != nil {
			t.Fatalf("input %q rendered %s: %v", in, lit, err)
		}
		if rest != "" {
			t.Fatalf("input %q rendered %s: the literal closes early, %q is left over as SQL", in, lit, rest)
		}
		if decoded != in {
			t.Fatalf("input %q rendered %s decodes to %q", in, lit, decoded)
		}
	}
}

func FuzzSingleQuoteEscape(f *testing.F) {
	for _, tc := range singleQuoteEscapeCases {
		f.Add(tc.in)
	}
	p := parameters.NewStringParameter("q", "q", parameters.WithStringEscape("single-quotes"))
	f.Fuzz(func(t *testing.T, in string) {
		if !utf8.ValidString(in) {
			t.Skip() // parameters arrive as JSON strings: always valid UTF-8
		}
		got, err := p.Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		decoded, rest, err := decodeClickHouseStringLiteral(got.(string))
		if err != nil || rest != "" || decoded != in {
			t.Fatalf("input %q rendered %s: decoded %q rest %q err %v", in, got, decoded, rest, err)
		}
	})
}

// The escaped value lands in the statement verbatim, placeholder written bare.
func TestSingleQuoteEscapeInTemplate(t *testing.T) {
	params := parameters.Parameters{
		parameters.NewStringParameter("query", "q", parameters.WithStringEscape("single-quotes")),
	}
	values, err := parameters.ParseParams(params, map[string]any{"query": `x\' OR 1=1 --`}, nil)
	if err != nil {
		t.Fatalf("ParseParams: %v", err)
	}
	got, err := parameters.ResolveTemplateParams(params, "SELECT 1 WHERE positionCaseInsensitiveUTF8(name, {{.query}}) > 0 LIMIT 5", values.AsMap())
	if err != nil {
		t.Fatalf("ResolveTemplateParams: %v", err)
	}
	want := `SELECT 1 WHERE positionCaseInsensitiveUTF8(name, 'x\\'' OR 1=1 --') > 0 LIMIT 5`
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("statement mismatch (-want +got):\n%s", diff)
	}
}

// A default goes through the same escape as a supplied value.
func TestSingleQuoteEscapeDefault(t *testing.T) {
	params := parameters.Parameters{
		parameters.NewStringParameter("q", "q", parameters.WithStringEscape("single-quotes"), parameters.WithStringDefault(`a\b`)),
	}
	values, err := parameters.ParseParams(params, map[string]any{}, nil)
	if err != nil {
		t.Fatalf("ParseParams: %v", err)
	}
	if got := values[0].Value; got != `'a\\b'` {
		t.Fatalf("default rendered %v, want 'a\\\\b'", got)
	}
}

// Array items with the escape are escaped one by one and joined by {{array}}.
func TestSingleQuoteEscapeArrayItems(t *testing.T) {
	params := parameters.Parameters{
		parameters.NewArrayParameter("names", "names", parameters.NewStringParameter("name", "name", parameters.WithStringEscape("single-quotes"))),
	}
	values, err := parameters.ParseParams(params, map[string]any{"names": []any{`a\`, `b'`, "c"}}, nil)
	if err != nil {
		t.Fatalf("ParseParams: %v", err)
	}
	got, err := parameters.ResolveTemplateParams(params, "SELECT 1 WHERE name IN ({{array .names}})", values.AsMap())
	if err != nil {
		t.Fatalf("ResolveTemplateParams: %v", err)
	}
	want := `SELECT 1 WHERE name IN ('a\\', 'b''', 'c')`
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("statement mismatch (-want +got):\n%s", diff)
	}
}

// The other delimiters are unchanged by the single-quotes fix.
func TestOtherEscapesUnchanged(t *testing.T) {
	for _, tc := range []struct{ escape, in, want string }{
		{"backticks", `a\b`, "`a\\b`"},
		{"double-quotes", `a\b`, `"a\b"`},
		{"square-brackets", `a\b`, `[a\b]`},
	} {
		p := parameters.NewStringParameter("q", "q", parameters.WithStringEscape(tc.escape))
		got, err := p.Parse(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.escape, err)
		}
		if got != tc.want {
			t.Fatalf("%s: Parse(%q) = %s, want %s", tc.escape, tc.in, got, tc.want)
		}
	}
}

// decodeClickHouseStringLiteral reads one single-quoted literal at the start of
// s the way the ClickHouse lexer does (probed on 20.8, 25.6 and 25.9): a
// doubled quote and \' are a quote, \\ a backslash, \" a double quote,
// \n \t \r \b \f \a \v \e \0 control bytes, \xHH a byte, \N nothing, and
// \d \% \_ keep their backslash. It returns the decoded value and whatever
// follows the closing quote. A correctly escaped literal holds no backslash
// sequence but \\, so the other rules only matter for catching a regression:
// a lone backslash in the output then decodes to something else or closes
// the literal early.
func decodeClickHouseStringLiteral(s string) (string, string, error) {
	if !strings.HasPrefix(s, "'") {
		return "", "", fmt.Errorf("no opening quote")
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\'':
			if i+1 < len(s) && s[i+1] == '\'' {
				b.WriteByte('\'')
				i++
				continue
			}
			return b.String(), s[i+1:], nil
		case '\\':
			if i+1 >= len(s) {
				return "", "", fmt.Errorf("unterminated literal: ends in a backslash")
			}
			i++
			switch e := s[i]; e {
			case '\\', '\'', '"':
				b.WriteByte(e)
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'a':
				b.WriteByte('\a')
			case 'v':
				b.WriteByte('\v')
			case 'e':
				b.WriteByte(0x1b)
			case '0':
				b.WriteByte(0)
			case 'N':
			case 'x':
				if i+2 >= len(s) {
					return "", "", fmt.Errorf("short \\x escape")
				}
				var v byte
				if _, err := fmt.Sscanf(s[i+1:i+3], "%02x", &v); err != nil {
					return "", "", fmt.Errorf("bad \\x escape: %v", err)
				}
				b.WriteByte(v)
				i += 2
			default:
				b.WriteByte('\\')
				b.WriteByte(e)
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", "", fmt.Errorf("unterminated literal")
}
