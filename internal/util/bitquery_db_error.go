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

// Bitquery-specific: keep server details out of database errors. When ClickHouse
// refuses a query, its message ends in the server version, a query that failed on
// a replica starts with "Received from <host>:<port>", network errors name peer
// addresses, file and replication errors name server paths, and every failed
// function call repeats its whole expression ("while executing 'FUNCTION …'") —
// for a throwIf guard that is the guard's condition around the author's message.
// The tool layer hands err.Error() to the model verbatim (and records it as the
// metric error.type), so the sources swap the text for a cleaned copy and write
// the original to the server log. The error code, the code name and the message
// itself stay: they are what a caller needs to fix a query.
package util

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// BitqueryDatabaseError is the caller-facing form of an error a database returned
// for a query. Error() is the cleaned text. It has no Unwrap, so errors.Unwrap and
// a "%w" chain cannot reach the original text; errors.Is and errors.As still see
// the original error through the Is and As methods.
type BitqueryDatabaseError struct {
	msg   string
	code  int
	name  string
	cause error
}

func (e *BitqueryDatabaseError) Error() string { return e.msg }

// Code is the ClickHouse error code (395 for a throwIf guard), 0 when the text
// was not a ClickHouse exception.
func (e *BitqueryDatabaseError) Code() int { return e.code }

// Name is the ClickHouse error code name (e.g. "TOO_SLOW"), empty when the
// server did not print one (older servers never do).
func (e *BitqueryDatabaseError) Name() string { return e.name }

func (e *BitqueryDatabaseError) Is(target error) bool {
	return e.cause != nil && errors.Is(e.cause, target)
}

func (e *BitqueryDatabaseError) As(target any) bool {
	return e.cause != nil && errors.As(e.cause, target)
}

// bitqueryDatabaseLogLimit caps the original text written to the log.
const bitqueryDatabaseLogLimit = 16 << 10

// BitqueryDatabaseFailure is what a database source calls on a query error. It
// returns err unchanged when there is nothing to clean — nil, a transport
// failure, an error already cleaned, or a text with no server detail in it.
// Otherwise it logs the original at WARN and returns a *BitqueryDatabaseError.
func BitqueryDatabaseFailure(ctx context.Context, sourceName string, err error) error {
	if err == nil {
		return nil
	}
	var cleaned *BitqueryDatabaseError
	var transport *BitqueryTransportError
	if errors.As(err, &cleaned) || errors.As(err, &transport) || BitqueryIsTransportError(err) {
		return err
	}
	original := err.Error()
	msg, code, name, changed := BitqueryCleanDatabaseMessage(original)
	if !changed {
		return err
	}
	dbErr := &BitqueryDatabaseError{msg: msg, code: code, name: name, cause: err}
	bitqueryLogDatabaseError(ctx, sourceName, dbErr, original)
	return dbErr
}

// BitqueryDatabaseErrorResponse builds the error for a non-2xx HTTP answer whose
// body is returned to the caller: "unexpected status code: N, response body: B",
// with B cleaned when the body carries server detail (and the original logged).
func BitqueryDatabaseErrorResponse(ctx context.Context, sourceName string, status int, body []byte) error {
	original := fmt.Errorf("unexpected status code: %d, response body: %s", status, string(body))
	msg, code, name, changed := BitqueryCleanDatabaseMessage(string(body))
	if !changed {
		return original
	}
	dbErr := &BitqueryDatabaseError{
		msg:   fmt.Sprintf("unexpected status code: %d, response body: %s", status, msg),
		code:  code,
		name:  name,
		cause: original,
	}
	bitqueryLogDatabaseError(ctx, sourceName, dbErr, original.Error())
	return dbErr
}

// BitqueryDatabaseErrorInResult builds the error for a query that failed after the
// server had answered 200 and sent rows, so the exception came at the end of the
// result body. The rows are not returned; the exception text is cleaned like any
// other database error and the original logged.
func BitqueryDatabaseErrorInResult(ctx context.Context, sourceName string, rows int, exception string) error {
	original := fmt.Errorf("query failed after %d result rows had been sent: %s", rows, exception)
	msg, code, name, _ := BitqueryCleanDatabaseMessage(exception)
	reported := "query failed: " + msg
	if rows > 0 {
		reported = fmt.Sprintf("query failed after %d result rows had been sent (partial result discarded): %s", rows, msg)
	}
	dbErr := &BitqueryDatabaseError{msg: reported, code: code, name: name, cause: original}
	bitqueryLogDatabaseError(ctx, sourceName, dbErr, original.Error())
	return dbErr
}

func bitqueryLogDatabaseError(ctx context.Context, sourceName string, dbErr *BitqueryDatabaseError, original string) {
	logger, err := LoggerFromContext(ctx)
	if err != nil {
		return
	}
	// WARN, not ERROR: most of these are the database refusing a caller's query
	// (bad SQL, a guard, a limit), not a server fault.
	logger.WarnContext(ctx, "data source query error",
		"source", sourceName,
		"code", dbErr.code,
		"reported", dbErr.msg,
		"error", bitqueryTruncateForLog(bitqueryMaskSecrets(original), bitqueryDatabaseLogLimit))
}

func bitqueryTruncateForLog(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return fmt.Sprintf("%s...(%d bytes truncated)", text[:limit], len(text)-limit)
}

var (
	// "Code: 62." (current servers) or "Code: 62," (older: "Code: 62, e.displayText() = …").
	bqCodeRe = regexp.MustCompile(`Code: (\d+)[.,]`)
	// The old header, normalized to the current "Code: N. " form.
	bqLegacyHeadRe = regexp.MustCompile(`Code: (\d+), e\.(?:displayText|what)\(\) = `)
	// " (version 25.9.4.58 (official build))" — anywhere, nested messages repeat it.
	bqVersionRe = regexp.MustCompile(`,? ?\(version [^()]*(?:\([^()]*\)[^()]*)*\)`)
	// ", version 20.8.11.17" without parentheses, as some old builds print it.
	bqVersionBareRe = regexp.MustCompile(`, version \d+(?:\.\d+){1,3}(?: \(official build\))?`)
	bqStackTraceRe  = regexp.MustCompile(`(?s)\s*Stack trace(?: \([^)]*\))?:.*$`)
	// The trailing code name current servers print: "…exist. (UNKNOWN_TABLE)".
	bqNameTailRe = regexp.MustCompile(`\. \(([A-Z][A-Z0-9_]*)\)\s*$`)
	// A replica's error relayed by the initiator: "Received from chapi8:9000. DB::Exception: ".
	bqReceivedFromRe = regexp.MustCompile(`Received from \S+?\.(?:\s+DB::\w*Exception:)?\s+`)
	// The failed function call with its whole expression, up to the end of the message.
	bqFunctionContextRe = regexp.MustCompile(`[:,.]?\s*[Ww]hile executing 'FUNCTION `)
	// The pipeline step the error happened in: ": While executing AggregatingTransform".
	bqProcessorContextRe = regexp.MustCompile(`[:,.]?\s*While executing [A-Za-z]`)
	bqExceptionHeadRe    = regexp.MustCompile(`^(?:DB::\w*Exception: )+`)
	bqUserPrefixRe       = regexp.MustCompile(`^[A-Za-z0-9_.@-]+: `)
	bqExceptionTagLineRe = regexp.MustCompile(`\s*\n\d+ \S{16}\s*$`)

	// A proxy's request scope: "[ Id: …; User … to "host:port"(1); RemoteAddr: …]: ".
	bqProxyScopeRe = regexp.MustCompile(`\[ ?Id: [^\]]*\]:? ?`)
	bqOnHostRe     = regexp.MustCompile(`\s(?:on|from) host '?[A-Za-z0-9_.:\[\]-]*[A-Za-z0-9_\]]'?`)
	bqIPv6Re       = regexp.MustCompile(`\[[0-9A-Fa-f.]*:[0-9A-Fa-f.%]*:[0-9A-Fa-f:.%]*\](?::\d{1,5})?`)
	bqIPv4Re       = regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)\.){3}(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(?::\d{1,5})?\b`)
	bqInternalFQDN = regexp.MustCompile(`\b[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)*\.(?:local|internal|lan|localdomain|intranet|corp)\b(?::\d{1,5})?`)
	// host:port — replicas listen on more than the default ports (":9001" seen live),
	// so any port; a message puts a space after its own colons ("maximum: 1000").
	bqHostPortRe = regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9-]*(?:\.[A-Za-z0-9-]+)*:\d{2,5}\b`)
	// Server file-system and replication paths.
	bqServerPathRe = regexp.MustCompile(`(^|[\s'"(=,\[])/(?:var|etc|usr|opt|home|tmp|data|mnt|srv|root|clickhouse|proc|run)/[^\s'",;)\]]*`)
	bqSecretRe     = regexp.MustCompile(`(?i)\b(password|passwd|token|access_token|api_key|apikey|key)=[^&\s"',;()]*[^&\s"',;().]`)
	bqSpacesRe     = regexp.MustCompile(`[ \t]{2,}`)

	// A database user name in a URL or DSN, wherever it appears: "?user=mcp",
	// "&user=mcp", "http://mcp:secret@host" (the password goes with it). As with
	// secrets, a trailing period ends the sentence, not the name.
	bqUserURLParamRe = regexp.MustCompile(`([?&]user=)[^&\s"',;()#]{0,255}[^&\s"',;()#.]`)
	bqUserInfoRe     = regexp.MustCompile(`(\b[A-Za-z][A-Za-z0-9+.-]{0,20}://)[^\s/@?#'"]{1,256}@`)
	// Where a message starts echoing the caller's SQL: a syntax error's "failed at
	// position N (…): <fragment>", "while processing: '<query>'", "In scope <query>",
	// chproxy's `; query: "<query>"`. No user name is looked for from there on.
	bqSQLEchoRe = regexp.MustCompile(`failed at position \d+|while processing|[Ii]n scope |; query: "`)
)

// bqUserRedacted replaces a database user name in a caller-facing message.
const bqUserRedacted = "[user]"

// bqUserQuoted is a user name as ClickHouse (`mcp`, 'mcp') or chproxy ("mcp") quotes it.
const bqUserQuoted = "`[^`\\n]{1,128}`|'[^'\\n]{1,128}'|\"(?:[^\"\\\\\\n]|\\\\.){1,128}\""

// bqUserShapes are the messages known to name the database user, in the exact
// wording and letter case the servers print, each as (prefix)(name)(suffix). A
// bare word "user" is never enough: SQL echoes are full of it — system.processes
// and system.query_log have a user column ("WHERE user=currentUser()", "GROUP BY
// user", "user = 'x'") — and so are guard messages ("for user wallets").
var bqUserShapes = []*regexp.Regexp{
	// Code 202, 25.x and 20.8: "Too many simultaneous queries for user mcp. Current: 4, maximum: 4".
	regexp.MustCompile(`(Too many simultaneous queries for user )(.{1,256}?)(\. Current: \d)`),
	// Code 201: "Quota for user `mcp` for 3600s has been exceeded".
	regexp.MustCompile("(Quota for user )(" + bqUserQuoted + "|[^\\s`'\"]{1,128})( for \\S+ has )"),
	// Code 192: "There is no user `mcp` in user directories".
	regexp.MustCompile("(There is no user )(" + bqUserQuoted + "|[^\\s`'\"]{1,128})( in )"),
	// "User `mcp` is not allowed to …" (ClickHouse), `user "mcp" is not allowed to access via http` (chproxy).
	// Both quote the name; an unquoted word here is prose ("a user that is not allowed").
	regexp.MustCompile("(\\b(?:[Uu]ser|cluster user) )(" + bqUserQuoted + ")( is not allowed\\b)"),
	// chproxy: `limits for user "mcp" are exceeded`, `rate limit for user "mcp" is exceeded`,
	// `timeout for user "mcp" exceeded`, and the same for a "cluster user".
	regexp.MustCompile(`(\b(?:limits|rate limit|timeout) for (?:cluster )?user )("(?:[^"\\\n]|\\.){1,128}")( (?:are |is )?exceeded\b)`),
	// chproxy: `invalid username or password for user "mcp"`.
	regexp.MustCompile(`(\binvalid username or password for (?:cluster )?user )("(?:[^"\\\n]|\\.){1,128}")()`),
}

// Codes whose message starts with the database user name ("mcp: Not enough privileges").
var bqUserPrefixedCodes = map[int]bool{164: true, 192: true, 193: true, 194: true, 195: true, 497: true}

const (
	bqCodeThrowIf        = 395
	bqCodeAuthentication = 516
)

// BitqueryCleanDatabaseMessage returns text with server detail removed, the
// ClickHouse error code and code name when the text is a ClickHouse exception,
// and whether anything changed.
//
// For a ClickHouse exception it drops what precedes "Code: N" (driver framing),
// unwraps a JSON {"exception": …} body, normalizes the old "Code: N,
// e.displayText() = " header, removes the version, a stack trace, "Received from
// <host>." and the "while executing 'FUNCTION …'" / "While executing <step>"
// context, and keeps "Code: N. DB::Exception: <message>. (NAME)". A throwIf guard
// (code 395) becomes "invalid request: <message>" — the guard author's text only.
// The authentication error loses the user name. Any text, ClickHouse or not, has
// IP addresses, host:port pairs, internal host names, server paths, secrets in
// key=value form, proxy request scopes and database user names (in the messages
// known to print one, e.g. "… for user mcp. Current: 4", and in URLs / DSNs)
// replaced; a user name inside an echo of the caller's SQL is left alone.
func BitqueryCleanDatabaseMessage(text string) (clean string, code int, name string, changed bool) {
	original := strings.TrimSpace(text)
	clean = original
	if inner, ok := bitqueryJSONException(original); ok {
		clean = inner
	}
	if bitqueryLooksLikeClickHouse(clean) {
		if loc := bqCodeRe.FindStringSubmatchIndex(clean); loc != nil {
			code, _ = strconv.Atoi(clean[loc[2]:loc[3]])
			clean, name = bitqueryCleanClickHouse(clean, loc[0], code)
		} else {
			// No "Code: N" (a partly read exception block, say): the version and the
			// expression context still go.
			var body string
			body, name = bitqueryStripClickHouseContext(clean)
			clean = bitqueryAppendName(body, name)
		}
	}
	clean = bitqueryScrubServerDetail(clean)
	clean = strings.TrimSpace(bqSpacesRe.ReplaceAllString(clean, " "))
	return clean, code, name, clean != original
}

func bitqueryLooksLikeClickHouse(text string) bool {
	return strings.Contains(text, "DB::") || strings.Contains(text, "e.displayText()") || strings.Contains(text, "(version ")
}

// bitqueryJSONException unwraps the body a current server sends for a failed
// query in a JSON output format: {"exception": "…"}, or [{"exception": "…"}] with
// output_format_json_array_of_rows.
func bitqueryJSONException(text string) (string, bool) {
	if text == "" || (text[0] != '{' && text[0] != '[') {
		return "", false
	}
	var v any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		return "", false
	}
	pick := func(item any) (string, bool) {
		obj, ok := item.(map[string]any)
		if !ok {
			return "", false
		}
		s, ok := obj["exception"].(string)
		return s, ok && bqCodeRe.MatchString(s)
	}
	if arr, ok := v.([]any); ok {
		for i := len(arr) - 1; i >= 0; i-- {
			if s, ok := pick(arr[i]); ok {
				return s, true
			}
		}
		return "", false
	}
	return pick(v)
}

func bitqueryCleanClickHouse(text string, start, code int) (string, string) {
	prefix := strings.TrimSpace(text[:start])
	text = text[start:]
	// A mid-stream exception block ends with "<length> <tag>\r\n__exception__".
	if i := strings.Index(text, "__exception__"); i >= 0 {
		text = text[:i]
	}
	text = strings.TrimSpace(bqExceptionTagLineRe.ReplaceAllString(text, ""))
	// The driver quotes the body: `[HTTP 500] response body: "Code: …"`.
	if strings.HasSuffix(prefix, `"`) {
		text = strings.TrimSpace(strings.TrimSuffix(text, `"`))
	}
	text = bqLegacyHeadRe.ReplaceAllString(text, "Code: $1. ")
	text, name := bitqueryStripClickHouseContext(text)

	head := "Code: " + strconv.Itoa(code) + ". "
	message := strings.TrimSpace(strings.TrimPrefix(text, head))
	exception := bqExceptionHeadRe.FindString(message)
	message = strings.TrimSpace(message[len(exception):])
	if exception != "" {
		// Collapse "DB::Exception: DB::Exception: " left by a relayed replica error.
		exception = exception[:strings.Index(exception, ": ")+2]
	}

	switch {
	case code == bqCodeThrowIf:
		return "invalid request: " + message, name
	case code == bqCodeAuthentication:
		message = "Authentication failed"
	case bqUserPrefixedCodes[code]:
		message = bqUserPrefixRe.ReplaceAllString(message, "")
	}
	return bitqueryAppendName(head+exception+message, name), name
}

// bitqueryStripClickHouseContext removes the stack trace, the version, "Received
// from <host>." and the execution context from a ClickHouse message, and splits
// off the trailing code name.
func bitqueryStripClickHouseContext(text string) (body, name string) {
	text = bqStackTraceRe.ReplaceAllString(text, "")
	text = bqVersionRe.ReplaceAllString(text, "")
	text = strings.TrimSpace(bqVersionBareRe.ReplaceAllString(text, ""))
	if m := bqNameTailRe.FindStringSubmatchIndex(text); m != nil {
		name = text[m[2]:m[3]]
		text = text[:m[0]]
	}
	text = bqReceivedFromRe.ReplaceAllString(text, "")
	if m := bqFunctionContextRe.FindStringIndex(text); m != nil {
		text = text[:m[0]]
	}
	if m := bqProcessorContextRe.FindStringIndex(text); m != nil {
		text = text[:m[0]]
	}
	return strings.TrimSpace(text), name
}

func bitqueryAppendName(text, name string) string {
	switch {
	case name == "":
		return text
	case strings.HasSuffix(text, "."):
		return text + " (" + name + ")"
	default:
		return text + ". (" + name + ")"
	}
}

func bitqueryScrubServerDetail(text string) string {
	text = bqProxyScopeRe.ReplaceAllString(text, "")
	// User names before addresses: a DSN's "user:password@" goes whole.
	text = bitqueryScrubUserNames(text)
	text = bqReceivedFromRe.ReplaceAllString(text, "")
	text = bqOnHostRe.ReplaceAllString(text, "")
	text = bqServerPathRe.ReplaceAllString(text, "${1}[path]")
	text = bqIPv6Re.ReplaceAllString(text, "[address]")
	text = bqInternalFQDN.ReplaceAllString(text, "[address]")
	text = bqIPv4Re.ReplaceAllString(text, "[address]")
	text = bqHostPortRe.ReplaceAllString(text, "[address]")
	return bitqueryMaskSecrets(text)
}

// bitqueryScrubUserNames replaces the database user name where a message names
// it — "Too many simultaneous queries for user mcp" becomes "… for user [user]" —
// on every text, not only authentication errors: in the known message shapes
// (bqUserShapes) before any echo of the caller's SQL, and in URLs and DSNs
// anywhere. The log keeps the original.
func bitqueryScrubUserNames(text string) string {
	text = bqUserURLParamRe.ReplaceAllString(text, "${1}"+bqUserRedacted)
	text = bqUserInfoRe.ReplaceAllString(text, "${1}"+bqUserRedacted+"@")
	if !strings.Contains(text, "user") && !strings.Contains(text, "User") {
		return text
	}
	message, echo := text, ""
	if loc := bqSQLEchoRe.FindStringIndex(text); loc != nil {
		message, echo = text[:loc[0]], text[loc[0]:]
	}
	for _, shape := range bqUserShapes {
		message = shape.ReplaceAllString(message, "${1}"+bqUserRedacted+"${3}")
	}
	return message + echo
}

func bitqueryMaskSecrets(text string) string {
	return bqSecretRe.ReplaceAllString(text, "${1}=***")
}
