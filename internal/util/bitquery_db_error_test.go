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
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/googleapis/mcp-toolbox/internal/log"
)

// Error texts as the sources see them. Unless marked "synthesized", each one was
// returned by a live server (25.6.4.12, 25.9.4.58 or 20.8.11.17) — through the
// clickhouse-go HTTP driver ("sendQuery: [HTTP N] response body: …") or as the raw
// body of the HTTP interface — with only replica host names changed and long
// "Expected one of" lists shortened.
const (
	bqDriver62  = "sendQuery: [HTTP 400] response body: \"Code: 62. DB::Exception: Syntax error: failed at position 1 (SELEC): SELEC 1. Expected one of: Query, Query with output, EXPLAIN, SELECT query, possibly with UNION, Delete query, DELETE, Update query, UPDATE. (SYNTAX_ERROR) (version 25.6.4.12 (official build))\n\""
	bqDriver47  = "sendQuery: [HTTP 404] response body: \"Code: 47. DB::Exception: Missing columns: 'no_such_column_xyz' while processing: 'SELECT no_such_column_xyz FROM system.one', required columns: 'no_such_column_xyz'. (UNKNOWN_IDENTIFIER) (version 25.6.4.12 (official build))\n\""
	bqDriver60  = "sendQuery: [HTTP 404] response body: \"Code: 60. DB::Exception: Table trading_rt.no_such_table_xyz does not exist. (UNKNOWN_TABLE) (version 25.6.4.12 (official build))\n\""
	bqDriver395 = "sendQuery: [HTTP 500] response body: \"Code: 395. DB::Exception: x: while executing 'FUNCTION throwIf(1 :: 0, 'x' :: 1) -> throwIf(1, 'x') UInt8 : 3'. (FUNCTION_THROW_IF_VALUE_IS_NON_ZERO) (version 25.6.4.12 (official build))\n\""
	// token_trade_bars with after_time=garbage-time.
	bqDriver395Guard = "sendQuery: [HTTP 500] response body: \"Code: 395. DB::Exception: after_time is not a date-time: use YYYY-MM-DD HH:MM:SS (UTC) or unix seconds: while executing 'FUNCTION throwIf(1 :: 3, 'after_time is not a date-time: use YYYY-MM-DD HH:MM:SS (UTC) or unix seconds' :: 4) -> throwIf(1, 'after_time is not a date-time: use YYYY-MM-DD HH:MM:SS (UTC) or unix seconds') UInt8 : 2'. (FUNCTION_THROW_IF_VALUE_IS_NON_ZERO) (version 25.6.4.12 (official build))\n\""
	bqDriver395Expr  = "sendQuery: [HTTP 500] response body: \"Code: 395. DB::Exception: addresses: at most 50 distinct addresses per call - split the list: while executing 'FUNCTION throwIf(greater(length(range(60)), 50) :: 0, 'addresses: at most 50 distinct addresses per call - split the list' :: 1) -> throwIf(greater(length(range(60)), 50), 'addresses: at most 50 distinct addresses per call - split the list') UInt8 : 3'. (FUNCTION_THROW_IF_VALUE_IS_NON_ZERO) (version 25.6.4.12 (official build))\n\""
	bqDriver164      = "sendQuery: [HTTP 500] response body: \"Code: 164. DB::Exception: Cannot modify 'max_execution_time' setting in readonly mode. (READONLY) (version 25.6.4.12 (official build))\n\""
	// An exception block sent after rows were streamed; the driver misreads the
	// block framing and prints part of it twice.
	bqDriverMidStream = "readData stream: ClickHouse exception: eption: Limit for result exceeded, max rows: 100.00 thousand, current rows: 130.82 thousand. (TOO_MANY_ROWS_OR_BYTES) (version 25.6.4.12 (official build))\nexception__\r\nCode: 396. DB::Exception: Limit for result exceeded, max rows: 100.00 thousand, current rows: 130.82 thousand. (TOO_MANY_ROWS_OR_BYTES) (version 25.6.4.12 (official build))"

	// HTTP interface, JSONEachRow with output_format_json_array_of_rows=1.
	bqBody62 = "[\n{\"exception\": \"Code: 62. DB::Exception: Syntax error: failed at position 1 (SELEC): SELEC 1\\n. Expected one of: Query, Update query, UPDATE, COPY query, COPY. (SYNTAX_ERROR) (version 25.9.4.58 (official build))\"}\n]\n"
	bqBody47 = "[\n{\"exception\": \"Code: 47. DB::Exception: Missing columns: 'no_such_column_xyz' while processing: 'SELECT no_such_column_xyz FROM system.one', required columns: 'no_such_column_xyz'. (UNKNOWN_IDENTIFIER) (version 25.9.4.58 (official build))\"}\n]\n"
	// throwIf that failed on a replica (prefer_localhost_replica=0).
	bqBody395Remote = "[\n{\"exception\": \"Code: 395. DB::Exception: Received from chnode47-fast:9000. DB::Exception: remote-throw: while executing 'FUNCTION throwIf(greater(Block_Number, 0) :: 3, 'remote-throw' :: 2) -> throwIf(greater(Block_Number, 0), 'remote-throw') UInt8 : 1'. (FUNCTION_THROW_IF_VALUE_IS_NON_ZERO) (version 25.9.4.58 (official build))\"}\n]\n"
	// The server shortens a long expression with "...", quotes left unbalanced.
	bqBody395Shortened = "{\"exception\": \"Code: 395. DB::Exception: addresses: at most 50 distinct addresses per call - split the list: while executing 'FUNCTION throwIf(greater(length(Wanted), 50) :: 0, 'addresses: at most 50 distinct addresses per call - split the list' :: 3) -> throwIf(greater(length(Wanted), 50), 'addresses: at most 50 distinct addresses per call - split the ... UInt8 : 2'. (FUNCTION_THROW_IF_VALUE_IS_NON_ZERO) (version 25.9.4.58 (official build))\"}"
	bqBody6Remote      = "[\n{\"exception\": \"Code: 6. DB::Exception: Received from chnode38:9000. DB::Exception: Cannot parse string 'x87281' as UInt8: syntax error at begin of string. Note: there are toUInt8OrZero and toUInt8OrNull functions, which returns zero\\/NULL instead of throwing exception: while executing 'FUNCTION toUInt8(concat('x', toString(Block_Number)) :: 0) -> toUInt8(concat('x', toString(Block_Number))) UInt8 : 2'. (CANNOT_PARSE_TEXT) (version 25.9.4.58 (official build))\"}\n]\n"
	bqBody241          = "[\n{\"exception\": \"Code: 241. DB::Exception: Query memory limit exceeded: would use 6.50 MiB (attempt to allocate chunk of 6.50 MiB), maximum: 1000.00 B: While executing AggregatingTransform. (MEMORY_LIMIT_EXCEEDED) (version 25.9.4.58 (official build))\"}\n]\n"
	bqBody241Old       = "{\"exception\": \"Code: 241. DB::Exception: Query memory limit exceeded: would use 6.50 MiB (attempt to allocate chunk of 6.50 MiB bytes), maximum: 1000.00 B.: While executing AggregatingTransform. (MEMORY_LIMIT_EXCEEDED) (version 25.6.4.12 (official build))\"}"
	bqBody160          = "[\n{\"exception\": \"Code: 160. DB::Exception: Estimated query execution time (1216.65782 seconds) is too long. Maximum: 1. Estimated rows to process: 1000000000000 (246591930 read in 0.30002 seconds): While executing Numbers. (TOO_SLOW) (version 25.9.4.58 (official build))\"}\n]\n"
	bqBody159          = "{\"exception\": \"Code: 159. DB::Exception: Timeout exceeded: elapsed 1000.067993 ms, maximum: 1000 ms: While executing NumbersRange. (TIMEOUT_EXCEEDED) (version 25.9.4.58 (official build))\"}"
	// Authentication errors are plain text even for JSON formats.
	bqBody516 = "Code: 516. DB::Exception: no_such_user_errredact: Authentication failed: password is incorrect, or there is no user with such name. (AUTHENTICATION_FAILED) (version 25.9.4.58 (official build))\n"

	// 20.8: "Code: N, e.displayText() = …", no code name, plain text.
	bqOld62        = "Code: 62, e.displayText() = DB::Exception: Syntax error: failed at position 1 ('SELEC'): SELEC 1\n. Expected one of: ALTER query, SHOW [TEMPORARY] TABLES|DATABASES|CLUSTERS|CLUSTER 'name' [[NOT] [I]LIKE 'str'] [LIMIT expr], CREATE TABLE or ATTACH TABLE query (version 20.8.11.17 (official build))\n"
	bqOld47        = "Code: 47, e.displayText() = DB::Exception: Missing columns: 'no_such_column_xyz' while processing query: 'SELECT no_such_column_xyz FROM system.one', required columns: 'no_such_column_xyz', source columns: 'dummy' (version 20.8.11.17 (official build))\n"
	bqOld60        = "Code: 60, e.displayText() = DB::Exception: Table default.no_such_table_xyz doesn't exist. (version 20.8.11.17 (official build))\n"
	bqOld395       = "Code: 395, e.displayText() = DB::Exception: addresses: at most 50 distinct addresses per call - split the list (version 20.8.11.17 (official build))\n"
	bqOld395Remote = "Code: 395, e.displayText() = DB::Exception: Received from ch1.dc426:9000. DB::Exception: remote-throw. (version 20.8.11.17 (official build))\n"
	bqOld6Remote   = "Code: 6, e.displayText() = DB::Exception: Received from ch1.dc426:9000. DB::Exception: Cannot parse string 'x688' as UInt8: syntax error at begin of string. Note: there are toUInt8OrZero and toUInt8OrNull functions, which returns zero/NULL instead of throwing exception.. (version 20.8.11.17 (official build))\n"
	bqOld159       = "Code: 159, e.displayText() = DB::Exception: Timeout exceeded: elapsed 1.000013212 seconds, maximum: 1: While executing Numbers (version 20.8.11.17 (official build))\n"
	bqOld396       = "Code: 396, e.displayText() = DB::Exception: Limit for result exceeded, max rows: 25.00 thousand, current rows: 65.50 thousand (version 20.8.11.17 (official build))\n"
	bqOld516       = "Code: 516, e.displayText() = DB::Exception: no_such_user_errredact: Authentication failed: password is incorrect or there is no user with such name (version 20.8.11.17 (official build))\n"
)

var bqLeakRe = regexp.MustCompile(`(?i)version|official build|\.local\b|received from|while executing|e\.displayText|__exception__|\[HTTP|sendQuery|response body: "|:9000|chnode|dc426|no_such_user|s3cr3t|\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b|for user ["'\x60]?(?:mcp|guest|default|svc|someone)|user ["'\x60](?:mcp|someone)|user=mcp`)

func bqAssertClean(t *testing.T, text string) {
	t.Helper()
	if m := bqLeakRe.FindString(text); m != "" {
		t.Fatalf("cleaned text %q still contains %q", text, m)
	}
}

func TestBitqueryCleanDatabaseMessage(t *testing.T) {
	tcs := []struct {
		desc, in, want string
		code           int
		name           string
	}{
		{desc: "25.6 syntax", in: bqDriver62, code: 62, name: "SYNTAX_ERROR",
			want: "Code: 62. DB::Exception: Syntax error: failed at position 1 (SELEC): SELEC 1. Expected one of: Query, Query with output, EXPLAIN, SELECT query, possibly with UNION, Delete query, DELETE, Update query, UPDATE. (SYNTAX_ERROR)"},
		{desc: "25.6 unknown identifier keeps the query echo", in: bqDriver47, code: 47, name: "UNKNOWN_IDENTIFIER",
			want: "Code: 47. DB::Exception: Missing columns: 'no_such_column_xyz' while processing: 'SELECT no_such_column_xyz FROM system.one', required columns: 'no_such_column_xyz'. (UNKNOWN_IDENTIFIER)"},
		{desc: "25.6 unknown table", in: bqDriver60, code: 60, name: "UNKNOWN_TABLE",
			want: "Code: 60. DB::Exception: Table trading_rt.no_such_table_xyz does not exist. (UNKNOWN_TABLE)"},
		{desc: "25.6 throwIf", in: bqDriver395, code: 395, name: "FUNCTION_THROW_IF_VALUE_IS_NON_ZERO",
			want: "invalid request: x"},
		{desc: "25.6 throwIf guard with parentheses and colons in the message", in: bqDriver395Guard, code: 395, name: "FUNCTION_THROW_IF_VALUE_IS_NON_ZERO",
			want: "invalid request: after_time is not a date-time: use YYYY-MM-DD HH:MM:SS (UTC) or unix seconds"},
		{desc: "25.6 throwIf over an expression", in: bqDriver395Expr, code: 395, name: "FUNCTION_THROW_IF_VALUE_IS_NON_ZERO",
			want: "invalid request: addresses: at most 50 distinct addresses per call - split the list"},
		{desc: "25.6 readonly", in: bqDriver164, code: 164, name: "READONLY",
			want: "Code: 164. DB::Exception: Cannot modify 'max_execution_time' setting in readonly mode. (READONLY)"},
		{desc: "25.6 exception block after streamed rows", in: bqDriverMidStream, code: 396, name: "TOO_MANY_ROWS_OR_BYTES",
			want: "Code: 396. DB::Exception: Limit for result exceeded, max rows: 100.00 thousand, current rows: 130.82 thousand. (TOO_MANY_ROWS_OR_BYTES)"},
		{desc: "25.9 JSON body syntax", in: bqBody62, code: 62, name: "SYNTAX_ERROR",
			want: "Code: 62. DB::Exception: Syntax error: failed at position 1 (SELEC): SELEC 1\n. Expected one of: Query, Update query, UPDATE, COPY query, COPY. (SYNTAX_ERROR)"},
		{desc: "25.9 JSON body unknown identifier", in: bqBody47, code: 47, name: "UNKNOWN_IDENTIFIER",
			want: "Code: 47. DB::Exception: Missing columns: 'no_such_column_xyz' while processing: 'SELECT no_such_column_xyz FROM system.one', required columns: 'no_such_column_xyz'. (UNKNOWN_IDENTIFIER)"},
		{desc: "25.9 throwIf on a replica", in: bqBody395Remote, code: 395, name: "FUNCTION_THROW_IF_VALUE_IS_NON_ZERO",
			want: "invalid request: remote-throw"},
		{desc: "25.9 throwIf with a shortened expression", in: bqBody395Shortened, code: 395, name: "FUNCTION_THROW_IF_VALUE_IS_NON_ZERO",
			want: "invalid request: addresses: at most 50 distinct addresses per call - split the list"},
		{desc: "25.9 parse error on a replica", in: bqBody6Remote, code: 6, name: "CANNOT_PARSE_TEXT",
			want: "Code: 6. DB::Exception: Cannot parse string 'x87281' as UInt8: syntax error at begin of string. Note: there are toUInt8OrZero and toUInt8OrNull functions, which returns zero/NULL instead of throwing exception. (CANNOT_PARSE_TEXT)"},
		{desc: "25.9 memory limit", in: bqBody241, code: 241, name: "MEMORY_LIMIT_EXCEEDED",
			want: "Code: 241. DB::Exception: Query memory limit exceeded: would use 6.50 MiB (attempt to allocate chunk of 6.50 MiB), maximum: 1000.00 B. (MEMORY_LIMIT_EXCEEDED)"},
		{desc: "25.6 memory limit", in: bqBody241Old, code: 241, name: "MEMORY_LIMIT_EXCEEDED",
			want: "Code: 241. DB::Exception: Query memory limit exceeded: would use 6.50 MiB (attempt to allocate chunk of 6.50 MiB bytes), maximum: 1000.00 B. (MEMORY_LIMIT_EXCEEDED)"},
		{desc: "25.9 estimated time", in: bqBody160, code: 160, name: "TOO_SLOW",
			want: "Code: 160. DB::Exception: Estimated query execution time (1216.65782 seconds) is too long. Maximum: 1. Estimated rows to process: 1000000000000 (246591930 read in 0.30002 seconds). (TOO_SLOW)"},
		{desc: "25.9 timeout", in: bqBody159, code: 159, name: "TIMEOUT_EXCEEDED",
			want: "Code: 159. DB::Exception: Timeout exceeded: elapsed 1000.067993 ms, maximum: 1000 ms. (TIMEOUT_EXCEEDED)"},
		{desc: "25.9 authentication loses the user name", in: bqBody516, code: 516, name: "AUTHENTICATION_FAILED",
			want: "Code: 516. DB::Exception: Authentication failed. (AUTHENTICATION_FAILED)"},

		{desc: "20.8 syntax, brackets are not addresses", in: bqOld62, code: 62,
			want: "Code: 62. DB::Exception: Syntax error: failed at position 1 ('SELEC'): SELEC 1\n. Expected one of: ALTER query, SHOW [TEMPORARY] TABLES|DATABASES|CLUSTERS|CLUSTER 'name' [[NOT] [I]LIKE 'str'] [LIMIT expr], CREATE TABLE or ATTACH TABLE query"},
		{desc: "20.8 unknown identifier", in: bqOld47, code: 47,
			want: "Code: 47. DB::Exception: Missing columns: 'no_such_column_xyz' while processing query: 'SELECT no_such_column_xyz FROM system.one', required columns: 'no_such_column_xyz', source columns: 'dummy'"},
		{desc: "20.8 unknown table", in: bqOld60, code: 60,
			want: "Code: 60. DB::Exception: Table default.no_such_table_xyz doesn't exist."},
		{desc: "20.8 throwIf", in: bqOld395, code: 395,
			want: "invalid request: addresses: at most 50 distinct addresses per call - split the list"},
		{desc: "20.8 throwIf on a replica", in: bqOld395Remote, code: 395,
			want: "invalid request: remote-throw."},
		{desc: "20.8 parse error on a replica", in: bqOld6Remote, code: 6,
			want: "Code: 6. DB::Exception: Cannot parse string 'x688' as UInt8: syntax error at begin of string. Note: there are toUInt8OrZero and toUInt8OrNull functions, which returns zero/NULL instead of throwing exception.."},
		{desc: "20.8 timeout", in: bqOld159, code: 159,
			want: "Code: 159. DB::Exception: Timeout exceeded: elapsed 1.000013212 seconds, maximum: 1"},
		{desc: "20.8 result limit", in: bqOld396, code: 396,
			want: "Code: 396. DB::Exception: Limit for result exceeded, max rows: 25.00 thousand, current rows: 65.50 thousand"},
		{desc: "20.8 authentication", in: bqOld516, code: 516,
			want: "Code: 516. DB::Exception: Authentication failed"},
		{desc: "a partly read exception block without a code", name: "TOO_MANY_ROWS_OR_BYTES",
			in:   "readData stream: ClickHouse exception: eption: Limit for result exceeded, max rows: 100.00 thousand, current rows: 130.82 thousand. (TOO_MANY_ROWS_OR_BYTES) (version 25.6.4.12 (official build))",
			want: "readData stream: ClickHouse exception: eption: Limit for result exceeded, max rows: 100.00 thousand, current rows: 130.82 thousand. (TOO_MANY_ROWS_OR_BYTES)"},

		// synthesized from the server message formats
		{desc: "version without parentheses (synthesized)", code: 60,
			in:   "Code: 60, e.displayText() = DB::Exception: Table default.t doesn't exist., version 20.8.11.17",
			want: "Code: 60. DB::Exception: Table default.t doesn't exist."},
		{desc: "a throwIf without a code, context only (synthesized)",
			in:   "DB::Exception: remote-throw: while executing 'FUNCTION throwIf(greater(Block_Number, 0) :: 3, 'remote-throw' :: 2) -> throwIf(greater(Block_Number, 0), 'remote-throw') UInt8 : 1'",
			want: "DB::Exception: remote-throw"},
		{desc: "20.8 memory limit (synthesized)", code: 241,
			in:   "Code: 241, e.displayText() = DB::Exception: Memory limit (for query) exceeded: would use 9.31 GiB (attempt to allocate chunk of 4219648 bytes), maximum: 9.31 GiB: While executing AggregatingTransform (version 20.8.11.17 (official build))",
			want: "Code: 241. DB::Exception: Memory limit (for query) exceeded: would use 9.31 GiB (attempt to allocate chunk of 4219648 bytes), maximum: 9.31 GiB"},
		{desc: "connection errors name peers (synthesized)", code: 279, name: "ALL_CONNECTION_TRIES_FAILED",
			in:   "Code: 279. DB::NetException: All connection tries failed. Log: \n\nCode: 210. DB::NetException: Connection refused (chnode3-hdd:9001). (NETWORK_ERROR) (version 25.9.4.58 (official build))\nCode: 209. DB::NetException: Timeout: connect timed out: 10.20.30.40:9000 (chnode3.streaming-cluster.local:9000, 1000 ms). (SOCKET_TIMEOUT) (version 25.9.4.58 (official build))\n: While executing Remote. (ALL_CONNECTION_TRIES_FAILED) (version 25.9.4.58 (official build))",
			want: "Code: 279. DB::NetException: All connection tries failed. Log: \n\nCode: 210. DB::NetException: Connection refused ([address]). (NETWORK_ERROR)\nCode: 209. DB::NetException: Timeout: connect timed out: [address] ([address], 1000 ms). (SOCKET_TIMEOUT). (ALL_CONNECTION_TRIES_FAILED)"},
		{desc: "IPv6 peer (synthesized)", code: 209, name: "SOCKET_TIMEOUT",
			in:   "Code: 209. DB::NetException: Timeout exceeded while reading from socket (peer: [::ffff:10.20.30.40]:9000, local: [::ffff:10.20.30.41]:50412, 300000 ms). (SOCKET_TIMEOUT) (version 25.9.4.58 (official build))",
			want: "Code: 209. DB::NetException: Timeout exceeded while reading from socket (peer: [address], local: [address], 300000 ms). (SOCKET_TIMEOUT)"},
		{desc: "server file path (synthesized)", code: 76, name: "CANNOT_OPEN_FILE",
			in:   "Code: 76. DB::ErrnoException: Cannot open file /var/lib/clickhouse/store/0a1/0a1b2c3d-1111-2222-3333-444455556666/all_1_1_0/data.bin, errno: 24, strerror: Too many open files. (CANNOT_OPEN_FILE) (version 25.9.4.58 (official build))",
			want: "Code: 76. DB::ErrnoException: Cannot open file [path], errno: 24, strerror: Too many open files. (CANNOT_OPEN_FILE)"},
		{desc: "replication path names the replica (synthesized)", code: 242, name: "TABLE_IS_READ_ONLY",
			in:   "Code: 242. DB::Exception: Table is in readonly mode (replica path: /clickhouse/tables/01/eth_api/transfers/replicas/chnode3). (TABLE_IS_READ_ONLY) (version 25.9.4.58 (official build))",
			want: "Code: 242. DB::Exception: Table is in readonly mode (replica path: [path]). (TABLE_IS_READ_ONLY)"},
		{desc: "access denied loses the user name (synthesized)", code: 497, name: "ACCESS_DENIED",
			in:   "Code: 497. DB::Exception: mcp: Not enough privileges. To execute this query, it's necessary to have the grant SELECT(Amount) ON eth_api.transfers_tx. (ACCESS_DENIED) (version 25.9.4.58 (official build))",
			want: "Code: 497. DB::Exception: Not enough privileges. To execute this query, it's necessary to have the grant SELECT(Amount) ON eth_api.transfers_tx. (ACCESS_DENIED)"},
		{desc: "on host (synthesized)", code: 159, name: "TIMEOUT_EXCEEDED",
			in:   "Code: 159. DB::Exception: Timeout exceeded on host chnode3.dc426: elapsed 5 ms. (TIMEOUT_EXCEEDED) (version 25.9.4.58 (official build))",
			want: "Code: 159. DB::Exception: Timeout exceeded: elapsed 5 ms. (TIMEOUT_EXCEEDED)"},
		{desc: "stack trace (synthesized)", code: 60, name: "UNKNOWN_TABLE",
			in:   "Code: 60. DB::Exception: Table default.t does not exist. (UNKNOWN_TABLE)\nStack trace (when copying this message, always include the lines below):\n\n0. DB::Exception::Exception() @ 0x000000000c5d4a3b in /usr/bin/clickhouse\n (version 25.9.4.58 (official build))",
			want: "Code: 60. DB::Exception: Table default.t does not exist. (UNKNOWN_TABLE)"},
		{desc: "a query echo with slashes, times, maps and URLs survives (synthesized)", code: 47, name: "UNKNOWN_IDENTIFIER",
			in:   "Code: 47. DB::Exception: Missing columns: 'x' while processing: 'SELECT a / b / c, x, map('k', 1)['k'], toDateTime('2026-09-16 12:30:45'), '2026-09-16T12:30:45', number::UInt32, 'HH:MM:SS', '/api/v1', 'Asia/Dubai' FROM t WHERE u = 'https://example.com/a/b'', required columns: 'x' 'a' 'b' 'c'. (UNKNOWN_IDENTIFIER) (version 25.9.4.58 (official build))",
			want: "Code: 47. DB::Exception: Missing columns: 'x' while processing: 'SELECT a / b / c, x, map('k', 1)['k'], toDateTime('2026-09-16 12:30:45'), '2026-09-16T12:30:45', number::UInt32, 'HH:MM:SS', '/api/v1', 'Asia/Dubai' FROM t WHERE u = 'https://example.com/a/b'', required columns: 'x' 'a' 'b' 'c'. (UNKNOWN_IDENTIFIER)"},
		{desc: "a proxy request scope, and the user it names (synthesized)",
			in:   `[ Id: 17A2B3C4D5E6F708; User "mcp"(1) proxying as "mcp"(1) to "10.20.30.40:8123"(1); RemoteAddr: "10.20.30.41:51234"; LocalAddr: "10.20.30.42:8123"; Duration: 120000123 μs]: timeout for user "mcp" exceeded: 2m0s`,
			want: `timeout for user [user] exceeded: 2m0s`},
		{desc: "a secret and a user in key=value form (synthesized)", code: 36, name: "BAD_ARGUMENTS",
			in:   "Code: 36. DB::Exception: Bad URL http://example.com/?user=mcp&password=s3cr3t. (BAD_ARGUMENTS) (version 25.9.4.58 (official build))",
			want: "Code: 36. DB::Exception: Bad URL http://example.com/?user=[user]&password=***. (BAD_ARGUMENTS)"},

		// Too many simultaneous queries (Code 202) names the database user on every
		// server version. Live texts: see bitquery_busy_retry_test.go.
		{desc: "25.6 per-user limit, live", in: bqBusy25User, code: 202, name: "TOO_MANY_SIMULTANEOUS_QUERIES",
			want: "Code: 202. DB::Exception: Too many simultaneous queries for user [user]. Current: 13, maximum: 1. (TOO_MANY_SIMULTANEOUS_QUERIES)"},
		{desc: "25.6 per-user limit on a connection handshake, as production logs it", in: bqBusy25Handshake, code: 202, name: "TOO_MANY_SIMULTANEOUS_QUERIES",
			want: "Code: 202. DB::Exception: Too many simultaneous queries for user [user]. Current: 4, maximum: 4. (TOO_MANY_SIMULTANEOUS_QUERIES)"},
		{desc: "25.6 server-wide limit names no user, live", in: bqBusy25AllUsersArray, code: 202, name: "TOO_MANY_SIMULTANEOUS_QUERIES",
			want: "Code: 202. DB::Exception: Too many simultaneous queries for all users. Current: 23, maximum: 1. (TOO_MANY_SIMULTANEOUS_QUERIES)"},
		{desc: "20.8 per-user limit, live", in: bqBusy20User, code: 202,
			want: "Code: 202. DB::Exception: Too many simultaneous queries for user [user]. Current: 3, maximum: 1"},
		{desc: "20.8 per-user limit for mcp (synthesized from the live text)", code: 202,
			in:   "Code: 202, e.displayText() = DB::Exception: Too many simultaneous queries for user mcp. Current: 4, maximum: 4 (version 20.8.11.17 (official build))\n",
			want: "Code: 202. DB::Exception: Too many simultaneous queries for user [user]. Current: 4, maximum: 4"},
		{desc: "a dotted user name keeps the sentence's period (synthesized)", code: 202, name: "TOO_MANY_SIMULTANEOUS_QUERIES",
			in:   "Code: 202. DB::Exception: Too many simultaneous queries for user svc.mcp-2. Current: 4, maximum: 4. (TOO_MANY_SIMULTANEOUS_QUERIES) (version 25.9.4.58 (official build))",
			want: "Code: 202. DB::Exception: Too many simultaneous queries for user [user]. Current: 4, maximum: 4. (TOO_MANY_SIMULTANEOUS_QUERIES)"},
		{desc: "25.x quota names the user in backquotes (synthesized)", code: 201, name: "QUOTA_EXCEEDED",
			in:   "Code: 201. DB::Exception: Quota for user `mcp` for 3600s has been exceeded: queries = 1001/1000. Interval will end at 2026-09-21 11:00:00. Name of quota template: `default`. (QUOTA_EXCEEDED) (version 25.9.4.58 (official build))",
			want: "Code: 201. DB::Exception: Quota for user [user] for 3600s has been exceeded: queries = 1001/1000. Interval will end at 2026-09-21 11:00:00. Name of quota template: `default`. (QUOTA_EXCEEDED)"},
		{desc: "20.8 quota (synthesized)", code: 201,
			in:   "Code: 201, e.displayText() = DB::Exception: Quota for user `default` for 3600s has been exceeded: queries = 1001/1000. Interval will end at 2026-09-21 11:00:00. Name of quota template: `default`. (version 20.8.11.17 (official build))",
			want: "Code: 201. DB::Exception: Quota for user [user] for 3600s has been exceeded: queries = 1001/1000. Interval will end at 2026-09-21 11:00:00. Name of quota template: `default`."},
		{desc: "an unknown user in backquotes (synthesized)", code: 192, name: "UNKNOWN_USER",
			in:   "Code: 192. DB::Exception: There is no user `someone` in user directories. (UNKNOWN_USER) (version 25.9.4.58 (official build))",
			want: "Code: 192. DB::Exception: There is no user [user] in user directories. (UNKNOWN_USER)"},
		{desc: "a user that is not allowed (synthesized)", code: 497, name: "ACCESS_DENIED",
			in:   "Code: 497. DB::Exception: User `mcp` is not allowed to change settings. (ACCESS_DENIED) (version 25.9.4.58 (official build))",
			want: "Code: 497. DB::Exception: User [user] is not allowed to change settings. (ACCESS_DENIED)"},
		{desc: "chproxy concurrency limit (synthesized)",
			in:   `[ Id: 17A2B3C4D5E6F709; User "mcp"(1) proxying as "mcp"(1) to "10.20.30.40:8123"(3); RemoteAddr: "10.20.30.41:51234"; LocalAddr: "10.20.30.42:8123"; Duration: 12 μs]: limits for user "mcp" are exceeded: max_concurrent_queries limit: 4`,
			want: `limits for user [user] are exceeded: max_concurrent_queries limit: 4`},
		{desc: "chproxy refusal keeps its query echo (synthesized)",
			in:   `limits for user "mcp" are exceeded: max_concurrent_queries limit: 4; query: "SELECT 'Too many simultaneous queries for user bob. Current: 1' WHERE user = 'alice'"`,
			want: `limits for user [user] are exceeded: max_concurrent_queries limit: 4; query: "SELECT 'Too many simultaneous queries for user bob. Current: 1' WHERE user = 'alice'"`},
		{desc: "chproxy rate limit for a cluster user (synthesized)",
			in:   `rate limit for cluster user "default" is exceeded: requests_per_minute limit: 100`,
			want: `rate limit for cluster user [user] is exceeded: requests_per_minute limit: 100`},
		{desc: "chproxy authentication and access (synthesized)",
			in:   `invalid username or password for user "mcp"; user "someone" is not allowed to access via http`,
			want: `invalid username or password for user [user]; user [user] is not allowed to access via http`},
		{desc: "a DSN names the user and its password (synthesized)", code: 36, name: "BAD_ARGUMENTS",
			in:   "Code: 36. DB::Exception: Bad URL http://mcp:s3cr3t@example.com/db. (BAD_ARGUMENTS) (version 25.9.4.58 (official build))",
			want: "Code: 36. DB::Exception: Bad URL http://[user]@example.com/db. (BAD_ARGUMENTS)"},

		// "user" that names no database user stays as it is: SQL echoes (system.processes
		// and system.query_log have a user column) and guard messages.
		{desc: "a syntax error echo keeps user columns and quoted names (synthesized)", code: 62, name: "SYNTAX_ERROR",
			in:   "Code: 62. DB::Exception: Syntax error: failed at position 36 ('FORM'): FORM system.processes WHERE user=currentUser() AND user = 'alice' OR user 'alice' GROUP BY user. Expected one of: token, Comma, FROM. (SYNTAX_ERROR) (version 25.9.4.58 (official build))",
			want: "Code: 62. DB::Exception: Syntax error: failed at position 36 ('FORM'): FORM system.processes WHERE user=currentUser() AND user = 'alice' OR user 'alice' GROUP BY user. Expected one of: token, Comma, FROM. (SYNTAX_ERROR)"},
		{desc: "a syntax error echo keeps even a message-like fragment (synthesized)", code: 62, name: "SYNTAX_ERROR",
			in:   "Code: 62. DB::Exception: Syntax error: failed at position 8 ('Too'): SELECT 'Too many simultaneous queries for user bob. Current: 1' Too. Expected one of: token. (SYNTAX_ERROR) (version 25.9.4.58 (official build))",
			want: "Code: 62. DB::Exception: Syntax error: failed at position 8 ('Too'): SELECT 'Too many simultaneous queries for user bob. Current: 1' Too. Expected one of: token. (SYNTAX_ERROR)"},
		{desc: "a query echo keeps user comparisons (synthesized)", code: 47, name: "UNKNOWN_IDENTIFIER",
			in:   "Code: 47. DB::Exception: Missing columns: 'usr' while processing: 'SELECT usr FROM system.query_log WHERE user=currentUser() OR user=1 GROUP BY user', required columns: 'usr'. (UNKNOWN_IDENTIFIER) (version 25.9.4.58 (official build))",
			want: "Code: 47. DB::Exception: Missing columns: 'usr' while processing: 'SELECT usr FROM system.query_log WHERE user=currentUser() OR user=1 GROUP BY user', required columns: 'usr'. (UNKNOWN_IDENTIFIER)"},
		{desc: "a guard message about user wallets stays (synthesized)", code: 395, name: "FUNCTION_THROW_IF_VALUE_IS_NON_ZERO",
			in:   "Code: 395. DB::Exception: at most 50 addresses for user wallets: while executing 'FUNCTION throwIf(1 :: 0, 'at most 50 addresses for user wallets' :: 1) -> throwIf(1, 'at most 50 addresses for user wallets') UInt8 : 3'. (FUNCTION_THROW_IF_VALUE_IS_NON_ZERO) (version 25.9.4.58 (official build))",
			want: "invalid request: at most 50 addresses for user wallets"},
		{desc: "a guard message saying a user word is not allowed stays (synthesized)", code: 395, name: "FUNCTION_THROW_IF_VALUE_IS_NON_ZERO",
			in:   "Code: 395. DB::Exception: address for user wallets is not allowed: while executing 'FUNCTION throwIf(1 :: 0, 'address for user wallets is not allowed' :: 1) -> throwIf(1, 'address for user wallets is not allowed') UInt8 : 3'. (FUNCTION_THROW_IF_VALUE_IS_NON_ZERO) (version 25.9.4.58 (official build))",
			want: "invalid request: address for user wallets is not allowed"},
		{desc: "a URL user before a sentence period keeps the period (synthesized)", code: 36, name: "BAD_ARGUMENTS",
			in:   "Code: 36. DB::Exception: Bad URL http://example.com/?user=mcp. (BAD_ARGUMENTS) (version 25.9.4.58 (official build))",
			want: "Code: 36. DB::Exception: Bad URL http://example.com/?user=[user]. (BAD_ARGUMENTS)"},
		{desc: "the per-user memory limit names no user (synthesized)", code: 241, name: "MEMORY_LIMIT_EXCEEDED",
			in:   "Code: 241. DB::Exception: Memory limit (for user) exceeded: would use 9.31 GiB (attempt to allocate chunk of 4219648 bytes), maximum: 9.31 GiB. (MEMORY_LIMIT_EXCEEDED) (version 25.9.4.58 (official build))",
			want: "Code: 241. DB::Exception: Memory limit (for user) exceeded: would use 9.31 GiB (attempt to allocate chunk of 4219648 bytes), maximum: 9.31 GiB. (MEMORY_LIMIT_EXCEEDED)"},
		{desc: "a query echo naming a user column is left alone (synthesized)", code: 47, name: "UNKNOWN_IDENTIFIER",
			in:   "Code: 47. DB::Exception: Missing columns: 'usr' while processing: 'SELECT user, usr FROM system.processes WHERE user = 'guest'', required columns: 'usr' 'user'. (UNKNOWN_IDENTIFIER) (version 25.9.4.58 (official build))",
			want: "Code: 47. DB::Exception: Missing columns: 'usr' while processing: 'SELECT user, usr FROM system.processes WHERE user = 'guest'', required columns: 'usr' 'user'. (UNKNOWN_IDENTIFIER)"},
	}
	for _, tc := range tcs {
		t.Run(tc.desc, func(t *testing.T) {
			got, code, name, changed := BitqueryCleanDatabaseMessage(tc.in)
			if got != tc.want {
				t.Fatalf("cleaned text:\n got %q\nwant %q", got, tc.want)
			}
			if code != tc.code || name != tc.name {
				t.Fatalf("code/name = %d/%q, want %d/%q", code, name, tc.code, tc.name)
			}
			if !changed {
				t.Fatalf("changed = false")
			}
			bqAssertClean(t, got)
		})
	}
}

func TestBitqueryCleanDatabaseMessageLeavesOtherText(t *testing.T) {
	for _, in := range []string{
		"",
		"sql: expected 2 destination arguments in Scan, not 1",
		"<html><body><h1>404 Not Found</h1>\nThe resource could not be found.\n</body></html>",
		`{"exception": "not a database error"}`,
		"Code: 7 is not a ClickHouse message without its exception name",
		"the data service did not answer within 180 seconds; the query may be too heavy, narrow it (shorter time window, fewer rows) or retry later",
		// "user" in prose or SQL, not naming a user
		"Memory limit (for user) exceeded",
		"Too many simultaneous queries for all users. Current: 23, maximum: 1",
		"a limit for user defined functions, set by user-defined settings for user with the id 5",
		"SELECT count() FROM system.processes WHERE user=currentUser() OR user=1 GROUP BY user",
		"at most 50 addresses for user wallets",
		"a user that is not allowed to do this",
		// Letter case other than the servers print is not a message shape (R2): SQL
		// writes USER in upper case ("CREATE USER 'x'"), no server writes "for User".
		"CREATE USER 'alice' IDENTIFIED BY 'x'",
		"Too many simultaneous queries for User alice. Current: 4, maximum: 4",
		"select count() from system.processes group by user order by count() desc",
	} {
		if got, _, _, changed := BitqueryCleanDatabaseMessage(in); changed || got != strings.TrimSpace(in) {
			t.Errorf("BitqueryCleanDatabaseMessage(%q) = %q, changed=%v", in, got, changed)
		}
	}
}

func bqTestLogger(t *testing.T) (context.Context, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	logger, err := log.NewLogger("standard", log.Info, &logs, &logs)
	if err != nil {
		t.Fatalf("failed to create logger: %v", err)
	}
	return WithLogger(context.Background(), logger), &logs
}

type bqDriverError struct{ text string }

func (e *bqDriverError) Error() string { return e.text }

var errBqSentinel = errors.New("sentinel")

func TestBitqueryDatabaseFailure(t *testing.T) {
	ctx, logs := bqTestLogger(t)

	cause := &bqDriverError{text: bqDriver60}
	got := BitqueryDatabaseFailure(ctx, "clickhouse-trading", fmt.Errorf("wrapped: %w", cause))
	want := "Code: 60. DB::Exception: Table trading_rt.no_such_table_xyz does not exist. (UNKNOWN_TABLE)"
	if got.Error() != want {
		t.Fatalf("got %q, want %q", got.Error(), want)
	}
	// errors.As / errors.Is reach the original error; Unwrap does not hand out its text.
	var driverErr *bqDriverError
	if !errors.As(got, &driverErr) || driverErr != cause {
		t.Fatalf("errors.As lost the original error")
	}
	if !errors.Is(got, cause) {
		t.Fatalf("errors.Is lost the original error")
	}
	if errors.Unwrap(got) != nil {
		t.Fatalf("Unwrap exposes the original error")
	}
	outer := fmt.Errorf("unable to execute query: %w", got)
	var dbErr *BitqueryDatabaseError
	if !errors.As(outer, &dbErr) || dbErr.Code() != 60 || dbErr.Name() != "UNKNOWN_TABLE" {
		t.Fatalf("errors.As(*BitqueryDatabaseError) = %v", dbErr)
	}
	if outer.Error() != "unable to execute query: "+want {
		t.Fatalf("wrapped text %q", outer.Error())
	}

	sentinelErr := BitqueryDatabaseFailure(ctx, "src", fmt.Errorf("%w: %s", errBqSentinel, bqOld159))
	if !errors.Is(sentinelErr, errBqSentinel) {
		t.Fatalf("errors.Is(sentinel) = false for %q", sentinelErr)
	}

	out := logs.String()
	for _, keep := range []string{"WARN", "data source query error", `"clickhouse-trading" 60`, "version 25.6.4.12"} {
		if !strings.Contains(out, keep) {
			t.Fatalf("log %q does not contain %q", out, keep)
		}
	}

	// Cleaning is idempotent and logs once.
	logs.Reset()
	if again := BitqueryDatabaseFailure(ctx, "clickhouse-trading", outer); again != outer {
		t.Fatalf("re-cleaning changed the error: %q", again)
	}
	if logs.Len() != 0 {
		t.Fatalf("re-cleaning logged again: %q", logs.String())
	}
}

func TestBitqueryDatabaseFailureLeavesOtherErrors(t *testing.T) {
	ctx, logs := bqTestLogger(t)
	transport := BitquerySanitizeTransportError(&url.Error{Op: "Post", URL: bitqueryLeakyURL, Err: fakeTimeout{}}, 0)
	plain := errors.New("sql: expected 2 destination arguments in Scan, not 1")
	for _, err := range []error{nil, plain, transport, fmt.Errorf("unable to execute query: %w", transport)} {
		if got := BitqueryDatabaseFailure(ctx, "src", err); got != err {
			t.Errorf("BitqueryDatabaseFailure(%v) = %v, want the same error", err, got)
		}
	}
	// A raw transport error is left for BitqueryTransportFailure, never logged here.
	raw := &url.Error{Op: "Post", URL: bitqueryLeakyURL, Err: fakeTimeout{}}
	if got := BitqueryDatabaseFailure(ctx, "src", raw); got != raw {
		t.Errorf("transport error changed: %v", got)
	}
	if logs.Len() != 0 {
		t.Fatalf("unexpected log output: %q", logs.String())
	}
}

func TestBitqueryDatabaseErrorResponse(t *testing.T) {
	ctx, logs := bqTestLogger(t)

	err := BitqueryDatabaseErrorResponse(ctx, "clickhouse-eth-http", 500, []byte(bqBody395Remote))
	if want := "unexpected status code: 500, response body: invalid request: remote-throw"; err.Error() != want {
		t.Fatalf("got %q, want %q", err.Error(), want)
	}
	var dbErr *BitqueryDatabaseError
	if !errors.As(err, &dbErr) || dbErr.Code() != 395 {
		t.Fatalf("not a *BitqueryDatabaseError with code 395: %#v", err)
	}
	out := logs.String()
	for _, keep := range []string{"WARN", "data source query error", "clickhouse-eth-http", "chnode47-fast:9000", "version 25.9.4.58", "while executing"} {
		if !strings.Contains(out, keep) {
			t.Fatalf("log %q does not contain %q", out, keep)
		}
	}

	err = BitqueryDatabaseErrorResponse(ctx, "ch-btc1-http", 400, []byte(bqOld6Remote))
	if want := "unexpected status code: 400, response body: Code: 6. DB::Exception: Cannot parse string 'x688' as UInt8: syntax error at begin of string. Note: there are toUInt8OrZero and toUInt8OrNull functions, which returns zero/NULL instead of throwing exception.."; err.Error() != want {
		t.Fatalf("got %q, want %q", err.Error(), want)
	}

	// A body with nothing to clean keeps the original text byte for byte.
	logs.Reset()
	html := "<html><body><h1>404 Not Found</h1>\nThe resource could not be found.\n</body></html>\n"
	err = BitqueryDatabaseErrorResponse(ctx, "clickhouse-eth-http", 404, []byte(html))
	if want := "unexpected status code: 404, response body: " + html; err.Error() != want {
		t.Fatalf("got %q, want %q", err.Error(), want)
	}
	if errors.As(err, &dbErr) {
		t.Fatalf("an unchanged body was wrapped")
	}
	if logs.Len() != 0 {
		t.Fatalf("an unchanged body was logged: %q", logs.String())
	}

	// The log keeps the original but masks secrets.
	logs.Reset()
	body := "Code: 36. DB::Exception: Bad URL http://example.com/?user=mcp&password=s3cr3t. (BAD_ARGUMENTS) (version 25.9.4.58 (official build))"
	err = BitqueryDatabaseErrorResponse(ctx, "clickhouse-eth-http", 400, []byte(body))
	bqAssertClean(t, err.Error())
	if strings.Contains(logs.String(), "s3cr3t") || !strings.Contains(logs.String(), "password=***") {
		t.Fatalf("log does not mask the secret: %q", logs.String())
	}
}
