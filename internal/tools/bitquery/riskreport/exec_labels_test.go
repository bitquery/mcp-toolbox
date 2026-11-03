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

package riskreport

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/googleapis/mcp-toolbox/internal/bitquerylabels"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

// hookLabels is a labels source that carries the labels-query-service hook, as
// the clickhouse source does. events records refreshes and label statements in
// the order they happen.
type hookLabels struct {
	fakeLabels
	pre    *bitquerylabels.Preprocessor
	events *[]string
}

func (h *hookLabels) BitqueryLabelsPreprocessor() *bitquerylabels.Preprocessor { return h.pre }

func (h *hookLabels) RunSQL(ctx context.Context, s string, p parameters.ParamValues) (any, error) {
	*h.events = append(*h.events, "select")
	return h.fakeLabels.RunSQL(ctx, s, p)
}

// refreshClient records what the report sends to the labels-query-service.
type refreshClient struct {
	events   *[]string
	requests []bitquerylabels.Request
}

func (c *refreshClient) Refresh(_ context.Context, req bitquerylabels.Request) {
	*c.events = append(*c.events, "refresh")
	c.requests = append(c.requests, req)
}

func newHookExecutor() (*executor, *refreshClient, *[]string) {
	events := &[]string{}
	client := &refreshClient{events: events}
	labels := &hookLabels{pre: bitquerylabels.NewPreprocessor(client, nil, ""), events: events}
	ex := newExecutor(&fakeChain{}, labels, withDefaults(Thresholds{}))
	return ex, client, events
}

// tronAddrs makes n distinct well-formed Tron addresses.
func tronAddrs(n int) []string {
	out := make([]string, n)
	for i := range out {
		b := []byte("T111111111111111111111111111111111")
		for j, v := len(b)-1, i; v > 0; j, v = j-1, v/9 {
			b[j] = byte('1' + v%9)
		}
		out[i] = string(b)
	}
	return out
}

// The report refreshes the addresses it meets in the labels-query-service,
// on the report's chain, before it reads their labels, and the step shows in
// the report's cost.
func TestLabelRowsRefreshesBeforeRead(t *testing.T) {
	ex, client, events := newHookExecutor()
	addrs := tronAddrs(3)
	if _, err := ex.labelRows(context.Background(), "tron", addrs); err != nil {
		t.Fatalf("labelRows: %v", err)
	}
	if len(client.requests) != 1 || client.requests[0].Chain != "tron" || len(client.requests[0].Addresses) != 3 {
		t.Fatalf("requests = %+v, want one request of 3 addresses on tron", client.requests)
	}
	if got := fmt.Sprint(*events); got != "[refresh select]" {
		t.Errorf("events = %s, want [refresh select]", got)
	}
	found := false
	for _, d := range ex.Cost.Detail {
		if d.Name == "labels_refresh" {
			found = true
		}
	}
	if !found {
		t.Errorf("cost.Detail = %+v, want a labels_refresh entry", ex.Cost.Detail)
	}
}

// One report sends at most the preprocessor's cap, across every label read
// it makes: the first read takes what it needs, the next the rest, then none.
func TestLabelRowsRefreshBudgetPerReport(t *testing.T) {
	ex, client, _ := newHookExecutor()
	for _, n := range []int{60, 60, 5} {
		if _, err := ex.labelRows(context.Background(), "tron", tronAddrs(n)); err != nil {
			t.Fatalf("labelRows(%d): %v", n, err)
		}
	}
	var sent []int
	for _, r := range client.requests {
		sent = append(sent, len(r.Addresses))
	}
	if got := fmt.Sprint(sent); got != "[60 40]" {
		t.Errorf("addresses sent per request = %s, want [60 40]", got)
	}
}

// The refresh never takes the time the label read needs: with less than the
// statement floor plus a second left, it is skipped and the read still runs.
func TestLabelRowsRefreshSkippedNearDeadline(t *testing.T) {
	ex, client, events := newHookExecutor()
	ctx, cancel := context.WithTimeout(context.Background(), 5500*time.Millisecond)
	defer cancel()
	if _, err := ex.labelRows(ctx, "tron", tronAddrs(2)); err != nil {
		t.Fatalf("labelRows: %v", err)
	}
	if len(client.requests) != 0 {
		t.Errorf("requests = %+v, want none with 5.5 s left", client.requests)
	}
	if got := fmt.Sprint(*events); got != "[select]" {
		t.Errorf("events = %s, want [select]", got)
	}
}

// A labels source without the hook reads as before.
func TestLabelRowsWithoutHook(t *testing.T) {
	labels := &fakeLabels{}
	ex := newExecutor(&fakeChain{}, labels, withDefaults(Thresholds{}))
	if _, err := ex.labelRows(context.Background(), "tron", tronAddrs(2)); err != nil {
		t.Fatalf("labelRows: %v", err)
	}
	if len(labels.statements) != 1 {
		t.Errorf("statements = %d, want 1", len(labels.statements))
	}
}
