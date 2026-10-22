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

// Bitquery-specific: refuse a statement that calls a ClickHouse table function
// which makes the SERVER open a connection somewhere else — url(), remote(),
// redis(), s3(), file() and their kin. The raw-SQL tools accept any statement a
// caller writes, and the database users they run as are only `readonly`:
// readonly stops writes, not a SELECT that fetches an arbitrary address through
// the server. Seen in production (2026-08-09, 2026-10-07): url() against
// internal cluster hosts and the cloud metadata address, redis() scans of
// internal IPs, url() with a third party's bearer token in the headers.
//
// This is a defence in depth in front of the database grants, not a
// replacement: the grants (REVOKE URL, REMOTE, REDIS, S3, FILE, ... ON *.*)
// stay the real boundary. The check is textual and deliberately simple: string
// literals and comments are removed, quoted identifiers unquoted, and any
// remaining forbidden name followed by `(` is a refusal. A false positive
// (a column literally named `url` called like a function) is not possible in
// ClickHouse SQL, since a name followed by `(` is always a call.
package util

import (
	"fmt"
	"regexp"
	"strings"
)

// bitqueryForbiddenTableFunctions is every ClickHouse table function that
// reaches outside the server, with its *Cluster / AllReplicas variants. Benign
// table functions (numbers, values, generateRandom, dictionary, view, merge,
// input, format, null, zeros, loop, mergeTreeIndex) are not listed.
var bitqueryForbiddenTableFunctions = []string{
	// HTTP and cross-server reads
	"url", "urlCluster",
	"remote", "remoteSecure",
	"cluster", "clusterAllReplicas",
	// other databases
	"mysql", "postgresql", "redis", "mongodb", "sqlite", "odbc", "jdbc",
	"arrowFlight", "ytsaurus",
	// object stores and files
	"s3", "s3Cluster", "gcs", "oss", "cosn",
	"azureBlobStorage", "azureBlobStorageCluster",
	"hdfs", "hdfsCluster",
	"file", "fileCluster",
	"iceberg", "icebergS3", "icebergAzure", "icebergHDFS", "icebergLocal",
	"icebergCluster", "icebergS3Cluster", "icebergAzureCluster", "icebergHDFSCluster",
	"deltaLake", "deltaLakeCluster", "deltaLakeS3", "deltaLakeAzure", "deltaLakeLocal",
	"hudi", "hudiCluster",
	"paimon", "paimonS3", "paimonCluster",
	// local processes
	"executable", "executablePool",
}

// bitqueryForbiddenCall matches a forbidden name (as a whole identifier, case
// insensitive — ClickHouse resolves table functions case-insensitively) followed
// by `(`, after literals, comments and identifier quotes are gone.
var bitqueryForbiddenCall = regexp.MustCompile(`(?i)\b(` + strings.Join(bitqueryForbiddenTableFunctions, "|") + `)\s*\(`)

// BitqueryForbiddenTableFunction reports whether statement calls a table
// function from bitqueryForbiddenTableFunctions and returns that name as the
// caller wrote it (without the parenthesis).
func BitqueryForbiddenTableFunction(statement string) (string, bool) {
	m := bitqueryForbiddenCall.FindStringSubmatch(bitqueryStripSQL(statement))
	if m == nil {
		return "", false
	}
	return m[1], true
}

// BitqueryTableFunctionRefused is the caller-facing error for a refused
// statement: an agent error (the request was wrong, not the server), naming the
// function and nothing of the statement itself.
func BitqueryTableFunctionRefused(name string) *AgentError {
	return NewAgentError(fmt.Sprintf("invalid request: the table function %s() is not available through this tool; query the data tables of this database only", name), nil)
}

// bitqueryStripSQL returns statement with every string literal replaced by a
// space, comments removed, and the quotes of quoted identifiers dropped, so that
// `url`(...) and "url"(...) read as url(...) and 'url(' inside a literal does
// not. It handles ClickHouse's lexical forms: '...' with \' and ” escapes,
// "..." and `...` identifiers with the same escapes, $tag$...$tag$ heredocs,
// -- and # line comments, and /* */ block comments (unterminated forms run to
// the end of the text).
func bitqueryStripSQL(statement string) string {
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
			// quoted identifier: keep the content, drop the quotes
			end := bitquerySkipQuoted(s, i, c)
			if end-1 > i && s[end-1] == c {
				// closed: keep the name so `url`(...) still reads as url(...)
				b.WriteString(s[i+1 : end-1])
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
	return b.String()
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
