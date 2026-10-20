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

package bitquerylabels

// Bitquery: the configured timeout bounds one tool call, not each lookup of it. A
// call with no chain splits its addresses into up to four lookups; up to 964ba806
// each had its own timeout, so a service that accepted and never answered held a
// label tool for four times the configured value.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fourPartitions is an address list that, with no chain, becomes four lookups:
// EVM, other base58, Tron and bech32 Bitcoin.
var fourPartitions = map[string]any{
	"addresses": strings.Join([]string{evmAddr, solanaAddr, tronAddr, bech32Addr}, ", "),
	"chain":     "",
}

func TestPreprocessTimeoutBoundsTheCall(t *testing.T) {
	const timeout = 300 * time.Millisecond

	builders := map[string]func(t *testing.T, host string) *Preprocessor{
		"New": func(t *testing.T, host string) *Preprocessor {
			p, err := New(context.Background(), "clickhouse-labels", &Config{Host: host, TimeoutSec: timeout.Seconds()})
			if err != nil || p == nil {
				t.Fatalf("New = %v, %v", p, err)
			}
			return p
		},
		"NewPreprocessor around a Service": func(t *testing.T, host string) *Preprocessor {
			return NewPreprocessor(newTestService(t, Config{Host: host, TimeoutSec: timeout.Seconds()}), nil, "")
		},
	}
	for name, build := range builders {
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			release := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}))
			defer srv.Close()
			defer close(release)

			p := build(t, hostOf(t, srv.URL))
			if n := len(buildLabelsRequests(p.addresses(fourPartitions), nil)); n != 4 {
				t.Fatalf("the address list makes %d lookups, want 4", n)
			}

			start := time.Now()
			p.Preprocess(context.Background(), fourPartitions)
			elapsed := time.Since(start)

			if elapsed > 2*timeout {
				t.Fatalf("Preprocess took %s with a %s timeout and 4 lookups; want about one timeout", elapsed.Round(time.Millisecond), timeout)
			}
			if n := requests.Load(); n != 1 {
				t.Errorf("the service received %d requests, want 1: the lookups left when the budget is spent are skipped", n)
			}
		})
	}
}

// A service that answers in time still gets every lookup of the call.
func TestPreprocessBudgetKeepsEveryLookupOfAFastService(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"updated_addresses":[]}`))
	}))
	defer srv.Close()

	p, err := New(context.Background(), "clickhouse-labels", &Config{Host: hostOf(t, srv.URL), TimeoutSec: 5})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.Preprocess(context.Background(), fourPartitions)
	if n := requests.Load(); n != 4 {
		t.Fatalf("the service received %d requests, want 4", n)
	}
}

// A client that is not the HTTP service (a test double) gets no extra bound.
func TestPreprocessOtherClientHasNoBudget(t *testing.T) {
	client := &recordingClient{}
	p := NewPreprocessor(client, nil, "")
	p.Preprocess(context.Background(), fourPartitions)
	if len(client.requests) != 4 {
		t.Fatalf("requests = %d, want 4", len(client.requests))
	}
}
