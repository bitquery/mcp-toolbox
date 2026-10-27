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

// Bitquery-specific: refuse a statement that would make the SERVER reach
// outside itself — a table function such as url(), remote(), redis(), s3() or
// file(), or a per-query setting that carries a subquery (additional_table_filters,
// additional_result_filter). The raw-SQL tools accept any statement a caller
// writes, and the database users they run as are only `readonly`: readonly=2
// still allows a SELECT that fetches an arbitrary address through the server,
// and allows setting changes in the query. Seen in production (2026-08-09,
// 2026-10-07, BIT-16095): url() against internal cluster hosts and the cloud
// metadata address, redis() scans of internal IPs, url() with a third party's
// bearer token in the headers.
//
// This is a defence in depth in front of the database grants, not a
// replacement: the grants (REVOKE SOURCES ON *.*) stay the real boundary. The
// check is textual: string literals, comments and heredocs are removed and
// identifier quotes dropped, then a forbidden name followed by `(` — or a
// forbidden setting name — is a refusal. A name followed by `(` is always a
// call in ClickHouse SQL, so a column named `url` used as data (url, domain(url))
// is never a false positive. It errs toward refusing: a quoted identifier that
// hides an escape (`\x75rl`(…) reads as url(…) to the server) is refused
// outright rather than decoded.
package util

import (
	"fmt"
	"regexp"
	"strings"
)

// bitqueryForbiddenExact is every table function that reaches outside the
// server and must match as a whole name. Longest name first within a family so
// Go's leftmost-first alternation picks urlCluster over url, remoteSecure over
// remote, and so on. Benign table functions (numbers, values, generateRandom,
// dictionary, view, merge, input, format, null, zeros, loop, mergeTreeIndex)
// are not listed.
var bitqueryForbiddenExact = []string{
	// HTTP and cross-server reads
	"urlCluster", "url",
	"remoteSecure", "remote",
	"clusterAllReplicas", "cluster",
	// other databases
	"mysql", "postgresql", "redis", "mongodb", "sqlite", "odbc", "jdbc",
	"arrowFlight", "ytsaurus",
	// object stores without a growing variant family
	"oss", "cosn",
	// files and local processes
	"fileCluster", "file", "executablePool", "executable",
}

// bitqueryForbiddenPrefixes are families whose names keep gaining variants
// (icebergLocalCluster, deltaLakeS3Cluster, s3Cluster, azureBlobStorageCluster,
// hiveCluster, ...): any name that STARTS with one of these and is followed by
// `(` is refused, so a new variant needs no code change. None of ClickHouse's
// scalar functions starts with one of these and is called as a table function.
var bitqueryForbiddenPrefixes = []string{
	"s3", "gcs", "hdfs", "azureBlobStorage",
	"iceberg", "deltaLake", "hudi", "paimon", "hive",
}

// bitqueryWS is ClickHouse's whitespace between a function name and its `(`.
// Go's \s omits the vertical tab, which ClickHouse treats as whitespace, so it
// is listed explicitly.
const bitqueryWS = `[ \t\n\v\f\r]*`

// bitqueryForbiddenCall matches an exact forbidden name or a forbidden-prefix
// name followed by `(`, case-insensitively (ClickHouse resolves table functions
// case-insensitively).
var bitqueryForbiddenCall = regexp.MustCompile(
	`(?i)(\b(?:` + strings.Join(bitqueryForbiddenExact, "|") + `)|\b(?:` +
		strings.Join(bitqueryForbiddenPrefixes, "|") + `)\w*)` + bitqueryWS + `\(`)

// bitqueryForbiddenSetting matches a per-query setting whose value is parsed as
// SQL (and so can carry a forbidden table function inside a string literal this
// check strips). None of our tools set either.
var bitqueryForbiddenSetting = regexp.MustCompile(`(?i)\b(additional_table_filters|additional_result_filter)\b`)

// bitquerySettingsClause matches the SETTINGS keyword that opens a per-query
// settings clause. It is used ONLY on a statement the caller wrote in full (see
// BitqueryRefuseCallerSQL), never on a server-built one: the server's own tools
// set SETTINGS and must keep working. ClickHouse parses SETTINGS as one
// contiguous keyword token — a comment inside it breaks the token, and a quoted
// identifier makes it a column name rather than the clause — so a word-boundary
// match on the stripped text (string literals and comments already removed)
// catches every real clause. A benign identifier such as my_settings or
// settings_v2 keeps the SETTINGS inside a longer word, so \b does not fire.
var bitquerySettingsClause = regexp.MustCompile(`(?i)\bsettings\b`)

// BitqueryForbiddenTableFunction reports whether statement calls a forbidden
// table function and returns the name as the caller wrote it (without the
// parenthesis). It does not catch the setting vector or an escaped identifier;
// BitqueryRefuseSQL is the full check the sources call.
func BitqueryForbiddenTableFunction(statement string) (string, bool) {
	stripped, _ := bitqueryStripSQL(statement)
	m := bitqueryForbiddenCall.FindStringSubmatch(stripped)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// BitqueryRefuseSQL returns an agent error when statement reaches outside the
// server — a forbidden table function, a backslash-escaped quoted identifier
// (which could hide a forbidden name), or a setting that carries a subquery —
// and nil otherwise. The sources call it before they send anything.
func BitqueryRefuseSQL(statement string) *AgentError {
	stripped, escapedIdent := bitqueryStripSQL(statement)
	if escapedIdent {
		// A quoted identifier that holds a backslash escape is refused whatever
		// follows: a textual check cannot safely reproduce ClickHouse's decoding,
		// and nothing of ours writes such a name.
		return bitqueryRefuse("an escaped identifier")
	}
	if m := bitqueryForbiddenCall.FindStringSubmatch(stripped); m != nil {
		return BitqueryTableFunctionRefused(m[1])
	}
	if m := bitqueryForbiddenSetting.FindStringSubmatch(stripped); m != nil {
		return bitqueryRefuse("the setting " + m[1])
	}
	return nil
}

// BitqueryRefuseCallerSQL is BitqueryRefuseSQL plus a refusal of any SETTINGS
// clause, for a statement whose whole text the CALLER wrote — the raw-SQL hatch
// tools, whose request body is substituted entirely from the caller's SQL
// parameter. Those sources run as a readonly=2 ClickHouse user, under which a
// SETTINGS clause in the statement can raise, or with `= 0` REMOVE, the limits
// the server relies on (max_execution_time, max_result_rows, max_memory_usage,
// max_threads, …). Because `= 0` means "no limit", capping or allowlisting
// values is a trap: the PRESENCE of the clause is refused, not a value. It is
// called only where the whole statement is caller text, so it never sees — and
// never refuses — the SETTINGS the server itself appends: the templated tools
// carry their SETTINGS in server-authored SQL text, and the address_risk_report
// engine builds and sends its own statements without going through a raw-SQL
// hatch. BitqueryRefuseSQL stays the check every statement runs through at the
// source, caller- and server-built alike, and carries no SETTINGS rule.
func BitqueryRefuseCallerSQL(statement string) *AgentError {
	if refusal := BitqueryRefuseSQL(statement); refusal != nil {
		return refusal
	}
	stripped, _ := bitqueryStripSQL(statement)
	if bitquerySettingsClause.MatchString(stripped) {
		return bitqueryRefuse("a SETTINGS clause")
	}
	return nil
}

// BitqueryTableFunctionRefused is the caller-facing error for a refused table
// function: an agent error (the request was wrong, not the server), naming the
// function and nothing of the statement itself.
func BitqueryTableFunctionRefused(name string) *AgentError {
	return bitqueryRefuse("the table function " + name + "()")
}

// bitqueryRefuse builds the refusal message. what names only the forbidden
// construct, never any part of the statement (the log and ELK must not carry a
// caller's hosts or tokens).
func bitqueryRefuse(what string) *AgentError {
	return NewAgentError(fmt.Sprintf("invalid request: %s is not available through this tool; query the data tables of this database only", what), nil)
}

// bitqueryStripSQL returns statement with every string literal replaced by a
// space, comments removed, and quoted identifiers reduced to their name (so
// `url`(...) and "url"(...) read as url(...), while 'url(' in a literal does
// not). escapedIdent is true when a closed quoted identifier held a backslash:
// ClickHouse decodes `\x75rl` to url, which a textual check cannot safely
// reproduce, so the caller refuses the whole statement. It handles ClickHouse's
// lexical forms: '...' with \' and ” escapes, "..." and `...` identifiers with
// the same escapes, $tag$...$tag$ heredocs, -- and # line comments, and /* */
// block comments (unterminated forms run to the end of the text).
func bitqueryStripSQL(statement string) (stripped string, escapedIdent bool) {
	var b strings.Builder
	b.Grow(len(statement))
	s := statement
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\'':
			// string literal: drop it entirely
			i = bitquerySkipQuoted(s, i, c)
			b.WriteByte(' ')
		case c == '"' || c == '`':
			// quoted identifier: keep the name, drop the quotes
			end := bitquerySkipQuoted(s, i, c)
			if end-1 > i && s[end-1] == c {
				name := s[i+1 : end-1]
				if strings.IndexByte(name, '\\') >= 0 {
					// an escape could hide a forbidden name; the statement is refused
					escapedIdent = true
					b.WriteByte(' ')
				} else {
					b.WriteString(name)
				}
			} else {
				// unterminated: ClickHouse rejects the statement; drop the rest
				b.WriteByte(' ')
			}
			i = end
		case c == '$':
			// $tag$ ... $tag$ heredoc (tag may be empty)
			if end, ok := bitquerySkipHeredoc(s, i); ok {
				b.WriteByte(' ')
				i = end
				continue
			}
			b.WriteByte(c)
			i++
		case c == '-' && i+1 < len(s) && s[i+1] == '-', c == '#':
			// line comment
			nl := strings.IndexByte(s[i:], '\n')
			if nl < 0 {
				i = len(s)
			} else {
				i += nl
			}
			b.WriteByte(' ')
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			// block comment
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				i = len(s)
			} else {
				i += 2 + end + 2
			}
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), escapedIdent
}

// bitquerySkipQuoted returns the index just past the quoted token that starts
// at s[start] with quote q, honouring backslash escapes and doubled quotes. An
// unterminated token runs to the end of s.
func bitquerySkipQuoted(s string, start int, q byte) int {
	for i := start + 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++ // skip the escaped byte
		case q:
			if i+1 < len(s) && s[i+1] == q {
				i++ // doubled quote: part of the token
				continue
			}
			return i + 1
		}
	}
	return len(s)
}

// bitquerySkipHeredoc recognises $tag$...$tag$ at s[start] (tag = letters,
// digits, underscore, possibly empty) and returns the index past its closing
// delimiter. ok is false when s[start] does not open a heredoc.
func bitquerySkipHeredoc(s string, start int) (int, bool) {
	i := start + 1
	for i < len(s) && (s[i] == '_' || s[i] >= '0' && s[i] <= '9' || s[i] >= 'a' && s[i] <= 'z' || s[i] >= 'A' && s[i] <= 'Z') {
		i++
	}
	if i >= len(s) || s[i] != '$' {
		return 0, false
	}
	tag := s[start : i+1]
	end := strings.Index(s[i+1:], tag)
	if end < 0 {
		return len(s), true
	}
	return i + 1 + end + len(tag), true
}
