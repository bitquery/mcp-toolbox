// Copyright 2025 Google LLC
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
	"context"
	"net/http"
	"regexp"
	"testing"
)

func bitqueryTestCtx(t *testing.T) context.Context {
	t.Helper()
	h := http.Header{}
	h.Set("X-Bitquery-Client-Id", "cli9630")
	h.Set("X-Bitquery-User-Id", "44504")
	h.Set("X-Bitquery-Payer-Id", "44504")
	h.Set("X-Bitquery-Plan", "pro")
	return WithBitqueryIdentity(context.Background(), h)
}

func TestBitqueryClickhouseQueryID(t *testing.T) {
	qid, ok := BitqueryClickhouseQueryID(bitqueryTestCtx(t))
	if !ok {
		t.Fatal("expected a query_id with identity in ctx")
	}
	// <rand>:<rand>:<client>:<user>:<payer>:<plan>:<server>
	re := regexp.MustCompile(`^[0-9a-f]{16}:[0-9a-f]{16}:cli9630:44504:44504:pro:mcp[^:]*$`)
	if !re.MatchString(qid) {
		t.Fatalf("modern query_id %q does not match the 7-part format", qid)
	}
	if _, ok := BitqueryClickhouseQueryID(context.Background()); ok {
		t.Fatal("expected no query_id without identity")
	}
}

func TestBitqueryLegacyClickhouseQueryID(t *testing.T) {
	qid, ok := BitqueryLegacyClickhouseQueryID(bitqueryTestCtx(t), "solana/transfers")
	if !ok {
		t.Fatal("expected a query_id with identity in ctx")
	}
	// <server>:<user_id>:<query_id(16)>:<cid(10)>:<payer_id>:<paths>
	// e.g. ed0cb589757d:44504:tizjVt1LyoN6csPE:LX9qFZxctR:44504:solana/transfers
	re := regexp.MustCompile(`^mcp[^:]*:44504:[0-9A-Za-z]{16}:[0-9A-Za-z]{10}:44504:solana/transfers$`)
	if !re.MatchString(qid) {
		t.Fatalf("legacy query_id %q does not match the v1 6-part format", qid)
	}
	if _, ok := BitqueryLegacyClickhouseQueryID(context.Background(), "solana/transfers"); ok {
		t.Fatal("expected no query_id without identity")
	}
}

func TestBitqueryLegacyClickhouseQueryIDSanitizesParts(t *testing.T) {
	h := http.Header{}
	h.Set("X-Bitquery-User-Id", " 4:4 ")
	h.Set("X-Bitquery-Payer-Id", "42")
	ctx := WithBitqueryIdentity(context.Background(), h)
	qid, ok := BitqueryLegacyClickhouseQueryID(ctx, "bad:paths/x")
	if !ok {
		t.Fatal("expected a query_id")
	}
	re := regexp.MustCompile(`^mcp[^:]*:44:[0-9A-Za-z]{16}:[0-9A-Za-z]{10}:42:badpaths/x$`)
	if !re.MatchString(qid) {
		t.Fatalf("legacy query_id %q not sanitized to 6 fields", qid)
	}
}
