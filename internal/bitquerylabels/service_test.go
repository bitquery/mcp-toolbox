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

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func hostOf(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	return u.Host
}

func newTestService(t *testing.T, cfg Config) *Service {
	t.Helper()
	s, err := NewService(cfg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return s
}

func TestRefreshPostsExpectedBody(t *testing.T) {
	var got httpRequest
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.Method != http.MethodPost || r.URL.Path != "/labels" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("body is not JSON: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := newTestService(t, Config{Host: hostOf(t, srv.URL), TimeoutSec: 5})
	s.Refresh(context.Background(), Request{Addresses: []string{"0xabc"}, Chains: []string{"ethereum", "polygon"}})

	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	want := httpRequest{Addresses: []string{"0xabc"}, Chains: []string{"ethereum", "polygon"}, Persist: true}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("request body mismatch (-want +got):\n%s", diff)
	}
}

// A non-2xx response is a failure, but Refresh swallows it.
func TestRefreshIgnoresServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := newTestService(t, Config{Host: hostOf(t, srv.URL), TimeoutSec: 5})
	s.Refresh(context.Background(), Request{Addresses: []string{"0xabc"}, Chain: "ethereum"})

	if _, err := s.doRefresh(context.Background(), Request{Addresses: []string{"0xabc"}, Chain: "ethereum"}); err == nil {
		t.Fatalf("doRefresh on a 500 must report an error")
	}
}

// A hanging service is cut off at the configured timeout.
func TestRefreshHonoursTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	s := newTestService(t, Config{Host: hostOf(t, srv.URL), TimeoutSec: 0.2})
	start := time.Now()
	s.Refresh(context.Background(), Request{Addresses: []string{"0xabc"}, Chain: "ethereum"})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Refresh took %s, want about 200ms", elapsed)
	}
}

func TestRefreshNoAddressesNoCall(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	defer srv.Close()

	s := newTestService(t, Config{Host: hostOf(t, srv.URL)})
	s.Refresh(context.Background(), Request{Chain: "ethereum"})
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("calls = %d, want 0", calls)
	}
}

func TestNewServiceValidation(t *testing.T) {
	if _, err := NewService(Config{}); err == nil {
		t.Errorf("a config with neither service nor host must be rejected")
	}
	if _, err := NewService(Config{Host: "h:1", TimeoutSec: -1}); err == nil {
		t.Errorf("a negative timeout must be rejected")
	}
	s := newTestService(t, Config{Service: "_x._tcp.example.local"})
	if s.timeout != 5*time.Second || s.srvTTL != 60*time.Second || !s.persist {
		t.Errorf("defaults = timeout %s, srvTTL %s, persist %v", s.timeout, s.srvTTL, s.persist)
	}
}

func TestRefreshParsesUpdatedAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"updated_addresses":["0xa","0xb","0xc"],"unrecognized":[],"results":[]}`))
	}))
	defer srv.Close()

	s := newTestService(t, Config{Host: hostOf(t, srv.URL)})
	updated, err := s.doRefresh(context.Background(), Request{Addresses: []string{"0xa", "0xb", "0xc"}, Chain: "ethereum"})
	if err != nil || updated != 3 {
		t.Fatalf("doRefresh = %d, %v; want 3, nil", updated, err)
	}
}

// A 2xx body that is not JSON still counts as success with 0 updated.
func TestRefreshToleratesUnparsableBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	s := newTestService(t, Config{Host: hostOf(t, srv.URL)})
	updated, err := s.doRefresh(context.Background(), Request{Addresses: []string{"0xa"}, Chain: "ethereum"})
	if err != nil || updated != 0 {
		t.Fatalf("doRefresh = %d, %v; want 0, nil", updated, err)
	}
}

func TestRefreshPersistFalse(t *testing.T) {
	var got httpRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	persist := false
	s := newTestService(t, Config{Host: hostOf(t, srv.URL), Persist: &persist})
	got.Persist = true
	s.Refresh(context.Background(), Request{Addresses: []string{"0xabc"}, Chain: "ethereum"})
	if got.Persist {
		t.Fatalf("persist:false was not sent")
	}
}
