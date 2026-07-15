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

// Bitquery-specific: propagate the caller identity injected by the edge proxy
// (haproxy authenticates against Hydra/KeyDB and sets X-Bitquery-* headers) so
// http tools can stamp a billing-attributable ClickHouse query_id. The Bitquery
// billing pipeline keys cost on a 7-part ":"-delimited query_id, not on headers
// (see api_v2_etl.query_log_to_query_log_with_query_id_mv).
package util

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"strings"
)

const (
	bitqueryClientIDHeader = "X-Bitquery-Client-Id"
	bitqueryUserIDHeader   = "X-Bitquery-User-Id"
	bitqueryPayerIDHeader  = "X-Bitquery-Payer-Id"
	bitqueryPlanHeader     = "X-Bitquery-Plan"
)

const bitqueryIdentityKey contextKey = "bitqueryIdentity"

// BitqueryIdentity is the caller identity taken from the edge-proxy headers.
type BitqueryIdentity struct {
	ClientID string
	UserID   string
	PayerID  string
	Plan     string
}

// bitqueryServer is the field-7 ("graphql_server") value, computed once. Prefixed
// with "mcp-" so MCP-originated rows are cleanly separable from GraphQL-gateway rows
// in api_v2.sql_requests (filter: graphql_server LIKE 'mcp%'). Sanitized so it can't
// break the query_id field structure.
var bitqueryServer = func() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return "mcp-" + sanitizeIDPart(h)
	}
	return "mcp"
}()

// WithBitqueryIdentity reads the X-Bitquery-* identity headers from an incoming
// request and stores them in the context. It is a no-op (returns ctx unchanged)
// when the payer id is absent — e.g. stdio, or an unauthenticated request — so a
// query_id is only stamped for real, authenticated callers.
func WithBitqueryIdentity(ctx context.Context, header http.Header) context.Context {
	if header == nil {
		return ctx
	}
	payer := strings.TrimSpace(header.Get(bitqueryPayerIDHeader))
	if payer == "" {
		return ctx
	}
	return context.WithValue(ctx, bitqueryIdentityKey, BitqueryIdentity{
		ClientID: strings.TrimSpace(header.Get(bitqueryClientIDHeader)),
		UserID:   strings.TrimSpace(header.Get(bitqueryUserIDHeader)),
		PayerID:  payer,
		Plan:     strings.TrimSpace(header.Get(bitqueryPlanHeader)),
	})
}

// BitqueryIdentityFromContext returns the stored identity, or false if absent.
func BitqueryIdentityFromContext(ctx context.Context) (BitqueryIdentity, bool) {
	id, ok := ctx.Value(bitqueryIdentityKey).(BitqueryIdentity)
	return id, ok
}

// BitqueryClickhouseQueryID builds the 7-part ":"-delimited ClickHouse query_id
// the billing pipeline keys on:
//
//	<rand>:<rand>:<client_id>:<user_id>:<payer_id>:<plan>:<server>
//
// matching extractAll(initial_query_id,'[^:]+') fields 3..7 = client_id, user_id,
// payer_id, plan, graphql_server. Every segment is non-empty so the ingest MV's
// '^(.+):(.+):(.+):(.+):(.+):(.+):(.+)$' match holds. Returns false when no
// identity is present, so the caller leaves query_id unset.
func BitqueryClickhouseQueryID(ctx context.Context) (string, bool) {
	id, ok := BitqueryIdentityFromContext(ctx)
	if !ok {
		return "", false
	}
	parts := []string{
		randomHex(8),
		randomHex(8),
		orDefault(sanitizeIDPart(id.ClientID), "mcp"),
		orDefault(sanitizeIDPart(id.UserID), "0"),
		orDefault(sanitizeIDPart(id.PayerID), "0"),
		orDefault(sanitizeIDPart(id.Plan), "unknown"),
		bitqueryServer,
	}
	return strings.Join(parts, ":"), true
}

// Legacy api-cluster (v1 graphql_server) query_id segment lengths, mirroring the
// v1 generate_query_id: a 16-char query id + a 10-char CID.
const (
	bitqueryLegacyGqlIDLength = 16
	bitqueryLegacyCIDLength   = 10
)

// BitqueryLegacyClickhouseQueryID builds the 6-part ":"-delimited query_id the
// LEGACY api-cluster billing pipeline parses (the v1 graphql_server format):
//
//	<server>:<user_id>:<query_id(16)>:<cid(10)>:<payer_id>:<paths>
//
// e.g. ed0cb589757d:44504:tizjVt1LyoN6csPE:LX9qFZxctR:44504:solana/transfers
// (v1: SERVER_NAME:user.id:graphql_query_id:random(CID):payer_id:paths). paths is
// the "<dataset>/<field>" the request is accounted to, configured per source
// (bitqueryLegacyBillingPaths). Returns false when no identity is present, so the
// caller leaves query_id unset.
func BitqueryLegacyClickhouseQueryID(ctx context.Context, paths string) (string, bool) {
	id, ok := BitqueryIdentityFromContext(ctx)
	if !ok {
		return "", false
	}
	parts := []string{
		bitqueryServer,
		orDefault(sanitizeIDPart(id.UserID), "0"),
		randomAlnum(bitqueryLegacyGqlIDLength),
		randomAlnum(bitqueryLegacyCIDLength),
		orDefault(sanitizeIDPart(id.PayerID), "0"),
		orDefault(sanitizeIDPart(paths), "mcp"),
	}
	return strings.Join(parts, ":"), true
}

// sanitizeIDPart strips ":" (the query_id field separator) and surrounding
// whitespace so a value can't break the 7-field structure.
func sanitizeIDPart(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), ":", "")
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "mcprnd"
	}
	return hex.EncodeToString(b)
}

const alnumChars = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// randomAlnum returns n random alphanumeric characters (the charset the v1
// generate_query_id uses for its random segments).
func randomAlnum(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "mcprnd"
	}
	out := make([]byte, n)
	for i, v := range b {
		out[i] = alnumChars[int(v)%len(alnumChars)]
	}
	return string(out)
}
