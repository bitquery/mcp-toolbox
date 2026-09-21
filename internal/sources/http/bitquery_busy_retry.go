// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package http

// Bitquery: ClickHouse refuses a query when its user (or the server) already runs
// the maximum number of simultaneous queries — Code 202, TOO_MANY_SIMULTANEOUS_QUERIES,
// HTTP 500 with X-ClickHouse-Exception-Code: 202 on 20.8 and 25.x alike. The
// refusal comes before the server reads or sends anything, so the request is
// sent again after a short pause (util.BitqueryBusyRetryPause: up to 3 retries,
// ~0.2 s / 0.5 s / 1 s, never past the caller's deadline). The same goes for a
// proxy in front of ClickHouse refusing the request for its own concurrency limit
// (HTTP 429 "Too Many Sessions" from the gateway, chproxy's max_concurrent_queries);
// its other refusals are not retried (see util.BitqueryBusyResponse). Only a
// non-2xx answer can be a refusal; an answer that carried rows (200, even one
// that ends in an exception) is never sent again.

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/googleapis/mcp-toolbox/internal/util"
)

// bitqueryRoundTrip sends req and reads the whole answer, sending it again while
// the answer is a busy refusal and a retry is allowed. It returns the last
// response (its body already read and closed) with the body bytes. A transport
// failure is returned as an address-free error, as RunRequest always did.
func (s *Source) bitqueryRoundTrip(ctx context.Context, req *http.Request) (*http.Response, []byte, error) {
	for retried := 0; ; retried++ {
		resp, body, err := s.bitqueryDoOnce(ctx, req)
		if err != nil {
			return nil, nil, err
		}
		reason := util.BitqueryBusyResponse(resp.StatusCode, resp.Header, body)
		if reason == "" {
			return resp, body, nil
		}
		next, ok := bitqueryReplayRequest(req)
		if !ok || !util.BitqueryBusyRetryPause(ctx, s.Name, reason, retried) {
			return resp, body, nil
		}
		req = next
	}
}

// bitqueryDoOnce sends req once and reads the whole body.
func (s *Source) bitqueryDoOnce(ctx context.Context, req *http.Request) (*http.Response, []byte, error) {
	resp, err := s.Client().Do(req)
	if err != nil {
		// Bitquery: a transport error prints the request URL (internal host, port,
		// query params); log it and hand the caller an address-free message instead.
		return nil, nil, fmt.Errorf("error making HTTP request: %w", util.BitqueryTransportFailure(ctx, s.Name, err, s.bitqueryTimeout(req)))
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		// Bitquery: a body read cut by the timeout or a reset names the peer IPs.
		return nil, nil, util.BitqueryTransportFailure(ctx, s.Name, err, s.bitqueryTimeout(req))
	}
	return resp, body, nil
}

// bitqueryReplayRequest returns a copy of req that can be sent again: same
// context, URL (query_id included) and headers, and a fresh body from GetBody.
// It returns false when the body cannot be produced again (a request built from
// a plain io.Reader has no GetBody) — such a request is not retried.
func bitqueryReplayRequest(req *http.Request) (*http.Request, bool) {
	next := req.Clone(req.Context())
	if req.Body == nil || req.Body == http.NoBody {
		return next, true
	}
	if req.GetBody == nil {
		return nil, false
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, false
	}
	next.Body = body
	return next, true
}
