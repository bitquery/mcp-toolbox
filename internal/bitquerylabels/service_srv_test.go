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

// Bitquery: the SRV lookup runs outside the lock that guards the cached endpoint
// set. Up to 964ba806 the lock was held across the lookup, so once the cache had
// expired every concurrent label call queued behind one slow resolution, even
// though an older set was there to use.

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// slowSRV is a resolver that answers only when released.
type slowSRV struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
	once    sync.Once
	answer  []*net.SRV
	err     error
}

func newSlowSRV(answer []*net.SRV, err error) *slowSRV {
	return &slowSRV{started: make(chan struct{}, 16), release: make(chan struct{}), answer: answer, err: err}
}

func (f *slowSRV) lookup(ctx context.Context, name string) ([]*net.SRV, error) {
	f.calls.Add(1)
	select {
	case f.started <- struct{}{}:
	default:
	}
	select {
	case <-f.release:
		return f.answer, f.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *slowSRV) open() { f.once.Do(func() { close(f.release) }) }

func (f *slowSRV) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-f.started:
	case <-time.After(2 * time.Second):
		t.Fatalf("no SRV lookup started")
	}
}

func newSRVService(t *testing.T, f *slowSRV) *Service {
	t.Helper()
	s := newTestService(t, Config{Service: "_labels-query-service._tcp.example.local", TimeoutSec: 5})
	s.lookupSRV = f.lookup
	t.Cleanup(f.open)
	return s
}

var newSRVAnswer = []*net.SRV{{Target: "labels-2.example.", Port: 8082}}

type endpointsResult struct {
	endpoints []string
	err       error
	took      time.Duration
}

func callEndpoints(s *Service, ctx context.Context, out chan<- endpointsResult) {
	start := time.Now()
	endpoints, err := s.endpoints(ctx)
	out <- endpointsResult{endpoints, err, time.Since(start)}
}

// waitForEndpoints calls endpoints until it returns want, which it does once the
// lookup in flight has stored its answer.
func waitForEndpoints(t *testing.T, s *Service, want []string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := s.endpoints(context.Background())
		if err == nil && cmp.Equal(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("endpoints = %v, %v; want %v", got, err, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// With an expired set cached, no caller waits for the lookup: each gets the old
// set at once, and one lookup refreshes it in the background.
func TestEndpointsServeExpiredSetWhileOneLookupRuns(t *testing.T) {
	f := newSlowSRV(newSRVAnswer, nil)
	s := newSRVService(t, f)
	s.srvEndpoints = []string{"labels-1.example:8081"}
	s.srvExpires = time.Now().Add(-time.Second)

	const callers = 8
	results := make(chan endpointsResult, callers)
	for i := 0; i < callers; i++ {
		go callEndpoints(s, context.Background(), results)
	}
	deadline := time.After(2 * time.Second)
	for i := 0; i < callers; i++ {
		select {
		case r := <-results:
			if r.err != nil || !cmp.Equal(r.endpoints, []string{"labels-1.example:8081"}) {
				t.Errorf("endpoints = %v, %v; want the cached set", r.endpoints, r.err)
			}
			if r.took > 500*time.Millisecond {
				t.Errorf("a caller with a cached set waited %s for the lookup", r.took)
			}
		case <-deadline:
			t.Fatalf("%d of %d callers with a cached set were still waiting for the lookup after 2s", callers-i, callers)
		}
	}

	f.waitStarted(t)
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("lookups = %d while one was running, want 1", n)
	}
	f.open()
	waitForEndpoints(t, s, []string{"labels-2.example:8082"})
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("lookups = %d, want 1: the new set is cached for its TTL", n)
	}
}

// With nothing cached, callers wait for one shared lookup.
func TestEndpointsColdStartSharesOneLookup(t *testing.T) {
	f := newSlowSRV(newSRVAnswer, nil)
	s := newSRVService(t, f)

	const callers = 8
	results := make(chan endpointsResult, callers)
	for i := 0; i < callers; i++ {
		go callEndpoints(s, context.Background(), results)
	}
	f.waitStarted(t)
	time.Sleep(50 * time.Millisecond) // let every caller reach the wait
	f.open()
	for i := 0; i < callers; i++ {
		select {
		case r := <-results:
			if r.err != nil || !cmp.Equal(r.endpoints, []string{"labels-2.example:8082"}) {
				t.Errorf("endpoints = %v, %v; want the resolved set", r.endpoints, r.err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%d of %d callers got no answer", callers-i, callers)
		}
	}
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("lookups = %d, want 1", n)
	}
}

// A caller with nothing cached waits no longer than its own context, however long
// the lookup another caller started takes.
func TestEndpointsWaitIsBoundedByTheCallersContext(t *testing.T) {
	f := newSlowSRV(newSRVAnswer, nil)
	s := newSRVService(t, f)

	first := make(chan endpointsResult, 1)
	go callEndpoints(s, context.Background(), first)
	f.waitStarted(t)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	second := make(chan endpointsResult, 1)
	go callEndpoints(s, ctx, second)
	select {
	case r := <-second:
		if !errors.Is(r.err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want the caller's deadline", r.err)
		}
		if r.took > time.Second {
			t.Errorf("the caller waited %s past its 100ms deadline", r.took)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("a caller with a 100ms deadline was still waiting after 2s")
	}

	f.open()
	select {
	case r := <-first:
		if r.err != nil || !cmp.Equal(r.endpoints, []string{"labels-2.example:8082"}) {
			t.Errorf("endpoints = %v, %v; want the resolved set", r.endpoints, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("the first caller got no answer")
	}
}

// A failed lookup keeps the expired set in use; with nothing cached it is the
// caller's error.
func TestEndpointsFailedLookup(t *testing.T) {
	lookupErr := errors.New("no such host")

	f := newSlowSRV(nil, lookupErr)
	s := newSRVService(t, f)
	s.srvEndpoints = []string{"labels-1.example:8081"}
	s.srvExpires = time.Now().Add(-time.Second)
	f.open()
	for i := 0; i < 3; i++ {
		got, err := s.endpoints(context.Background())
		if err != nil || !cmp.Equal(got, []string{"labels-1.example:8081"}) {
			t.Fatalf("endpoints = %v, %v; want the expired set", got, err)
		}
	}

	cold := newSlowSRV(nil, lookupErr)
	s = newSRVService(t, cold)
	cold.open()
	if _, err := s.endpoints(context.Background()); !errors.Is(err, lookupErr) {
		t.Fatalf("err = %v, want %v", err, lookupErr)
	}
	empty := newSlowSRV(nil, nil)
	s = newSRVService(t, empty)
	empty.open()
	if _, err := s.endpoints(context.Background()); err == nil {
		t.Fatalf("an empty answer with nothing cached must be an error")
	}
}
