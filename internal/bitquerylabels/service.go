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

// Package bitquerylabels is Bitquery-specific: a client for the external
// labels-query-service (which looks addresses up in MetaSleuth and stores the
// labels it finds into directory.labels) and a query pre-process hook that calls
// it before a label tool reads directory.labels. It ports the graphql_gateway
// pattern (services/labels_service.go + datastream/metadata/labels_preprocessor.go)
// to the toolbox: same wire format, same chain resolution, same best-effort
// contract — a failure is logged and counted, never returned.
package bitquerylabels

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/googleapis/mcp-toolbox/internal/log"
	"github.com/googleapis/mcp-toolbox/internal/telemetry"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

const (
	defaultTimeoutSec = 5.0
	defaultSrvTtlSec  = 60
	labelsPath        = "/labels"
	userAgent         = "mcp-toolbox"
)

// Config is the `bitqueryLabelsQueryService` block of a clickhouse source. Both
// Service and Host empty means the hook is disabled, so the key can stay in a
// template whose env var is unset.
type Config struct {
	// Service is the fully-qualified SRV name to resolve, e.g.
	// "_labels-query-service._node_web.streaming-cluster.local".
	Service string `yaml:"service"`
	// Host is a static "host:port" that bypasses SRV resolution (dev/testing).
	Host string `yaml:"host"`
	// TimeoutSec bounds one refresh (SRV resolution plus every endpoint attempt).
	// Default 5s.
	TimeoutSec float64 `yaml:"timeoutSec"`
	// SrvTtlSec is how long a resolved SRV result is cached. Default 60s.
	SrvTtlSec int `yaml:"srvTtlSec"`
	// Persist is the service's `persist` flag. Default true: the point of the call
	// is to have the service store the labels the SELECT then reads.
	Persist *bool `yaml:"persist"`
	// AddressParams names the tool parameters that carry addresses. A parameter
	// may hold one address or a list separated by any non-alphanumeric characters.
	// Default: address, addresses.
	AddressParams []string `yaml:"addressParams"`
	// ChainParam names the tool parameter that carries the chain. Default: chain.
	ChainParam string `yaml:"chainParam"`
}

// Enabled reports whether the block names a service to call.
func (c *Config) Enabled() bool {
	return c != nil && (strings.TrimSpace(c.Service) != "" || strings.TrimSpace(c.Host) != "")
}

// Request is one lookup: a set of addresses against a chain (Chain) or several
// chains (Chains).
type Request struct {
	Addresses []string
	Chain     string
	Chains    []string
}

// Client is the narrow view of the labels-query-service the pre-process hook
// depends on. Refresh is best-effort: it never returns an error.
type Client interface {
	Refresh(ctx context.Context, req Request)
}

// Service is the HTTP client of the labels-query-service. It resolves the
// endpoint lazily via SRV (cached with a TTL), so a service that is down at
// startup does not fail the toolbox and endpoint changes need no restart.
type Service struct {
	service string
	host    string
	timeout time.Duration
	srvTTL  time.Duration
	persist bool
	client  *http.Client

	srvMu        sync.Mutex
	srvEndpoints []string
	srvExpires   time.Time
}

var _ Client = (*Service)(nil)

// NewService builds the client from a config. It performs no network I/O.
func NewService(cfg Config) (*Service, error) {
	if !cfg.Enabled() {
		return nil, fmt.Errorf("labels-query-service requires either `service` (SRV name) or `host`")
	}
	if cfg.TimeoutSec < 0 || cfg.SrvTtlSec < 0 {
		return nil, fmt.Errorf("labels-query-service timeoutSec and srvTtlSec must not be negative")
	}
	timeoutSec := cfg.TimeoutSec
	if timeoutSec == 0 {
		timeoutSec = defaultTimeoutSec
	}
	srvTTLSec := cfg.SrvTtlSec
	if srvTTLSec == 0 {
		srvTTLSec = defaultSrvTtlSec
	}
	timeout := time.Duration(timeoutSec * float64(time.Second))
	return &Service{
		service: strings.TrimSpace(cfg.Service),
		host:    strings.TrimSpace(cfg.Host),
		timeout: timeout,
		srvTTL:  time.Duration(srvTTLSec) * time.Second,
		persist: cfg.Persist == nil || *cfg.Persist,
		client:  &http.Client{Timeout: timeout},
	}, nil
}

// httpRequest is the JSON body POSTed to /labels. omitempty keeps chain/chains
// mutually exclusive on the wire.
type httpRequest struct {
	Addresses []string `json:"addresses"`
	Chain     string   `json:"chain,omitempty"`
	Chains    []string `json:"chains,omitempty"`
	Persist   bool     `json:"persist"`
}

// httpResponse captures the one field of the /labels response that is recorded:
// the addresses the service reported as updated.
type httpResponse struct {
	UpdatedAddresses []string `json:"updated_addresses"`
}

// Refresh asks the labels-query-service to look up (and, with persist, store)
// labels for the given addresses/chains. Every failure is turned into a metric
// and a log line and then swallowed, so the query it precedes always proceeds.
func (s *Service) Refresh(ctx context.Context, req Request) {
	if s == nil || len(req.Addresses) == 0 {
		return
	}

	// Bound the whole operation (SRV resolution plus every endpoint attempt), so
	// trying several targets never costs more than the configured timeout.
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	start := time.Now()
	updated, err := s.doRefresh(ctx, req)
	elapsed := time.Since(start)
	result := "success"
	if err != nil {
		result = "failure"
	}

	userID := "0"
	if id, ok := util.BitqueryIdentityFromContext(ctx); ok && id.UserID != "" {
		userID = id.UserID
	}
	m := getInstruments()
	userAttrs := metric.WithAttributes(attribute.String("user_id", userID), attribute.String("result", result))
	m.requests.Add(ctx, 1, userAttrs)
	m.addresses.Add(ctx, int64(len(req.Addresses)), userAttrs)
	m.duration.Record(ctx, elapsed.Seconds(), metric.WithAttributes(attribute.String("result", result)))
	if err == nil {
		m.updated.Add(ctx, int64(updated), metric.WithAttributes(attribute.String("user_id", userID)))
	}

	logger := loggerFrom(ctx)
	if logger == nil {
		return
	}
	if err != nil {
		logger.WarnContext(ctx, "labels-query-service call failed, ignoring",
			"err", err.Error(), "addresses", len(req.Addresses), "chain", req.Chain, "chains", req.Chains)
	} else {
		logger.DebugContext(ctx, "labels-query-service call ok",
			"addresses", len(req.Addresses), "chain", req.Chain, "chains", req.Chains,
			"updated", updated, "ms", elapsed.Milliseconds())
	}
}

// doRefresh returns the number of addresses the service reported as updated on
// the successful attempt (0 on failure).
func (s *Service) doRefresh(ctx context.Context, req Request) (int, error) {
	endpoints, err := s.endpoints(ctx)
	if err != nil {
		return 0, err
	}

	body, err := json.Marshal(httpRequest{
		Addresses: req.Addresses,
		Chain:     req.Chain,
		Chains:    req.Chains,
		Persist:   s.persist,
	})
	if err != nil {
		return 0, err
	}

	// SRV advertises several instances, so a dead one fails over to the next. The
	// shared context deadline (set in Refresh) caps the total time.
	var lastErr error
	for _, endpoint := range endpoints {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		var updated int
		if updated, lastErr = s.post(ctx, endpoint, body); lastErr == nil {
			return updated, nil
		}
		if logger := loggerFrom(ctx); logger != nil {
			logger.DebugContext(ctx, "labels-query-service endpoint attempt failed", "endpoint", endpoint, "err", lastErr.Error())
		}
	}
	return 0, lastErr
}

// post sends one request to an endpoint and, on a 2xx, returns the count of
// updated_addresses parsed from the response.
func (s *Service) post(ctx context.Context, endpoint string, body []byte) (int, error) {
	url := fmt.Sprintf("http://%s%s", endpoint, labelsPath)
	if logger := loggerFrom(ctx); logger != nil {
		logger.DebugContext(ctx, "labels-query-service request", "url", url, "body", truncateForLog(body))
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", userAgent)

	resp, err := s.client.Do(httpReq)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if logger := loggerFrom(ctx); logger != nil {
		logger.DebugContext(ctx, "labels-query-service response", "endpoint", endpoint,
			"status", resp.StatusCode, "body", truncateForLog(respBody))
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("labels-query-service at %s returned status %d", endpoint, resp.StatusCode)
	}

	// A body that doesn't parse is not fatal (the tool reads labels from
	// ClickHouse regardless): report 0 updated.
	var decoded httpResponse
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return 0, nil
	}
	return len(decoded.UpdatedAddresses), nil
}

// endpoints returns the candidate "host:port" targets in random order, so load
// spreads across SRV instances. A static host wins; otherwise the SRV record is
// resolved and cached for srvTTL. On a resolution failure a previously cached
// (even expired) result is reused, so a transient DNS blip does not disable the
// hook.
func (s *Service) endpoints(ctx context.Context) ([]string, error) {
	if s.host != "" {
		return []string{s.host}, nil
	}

	s.srvMu.Lock()
	defer s.srvMu.Unlock()

	if time.Now().Before(s.srvExpires) && len(s.srvEndpoints) > 0 {
		return shuffled(s.srvEndpoints), nil
	}

	_, addrs, err := net.DefaultResolver.LookupSRV(ctx, "", "", s.service)
	if err != nil || len(addrs) == 0 {
		if len(s.srvEndpoints) > 0 {
			return shuffled(s.srvEndpoints), nil
		}
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("no SRV records for %s", s.service)
	}

	endpoints := make([]string, 0, len(addrs))
	for _, a := range addrs {
		endpoints = append(endpoints, fmt.Sprintf("%s:%d", strings.TrimSuffix(a.Target, "."), a.Port))
	}
	s.srvEndpoints = endpoints
	s.srvExpires = time.Now().Add(s.srvTTL)

	return shuffled(endpoints), nil
}

func shuffled(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// maxLogBodyBytes bounds request/response bodies in debug logs.
const maxLogBodyBytes = 4096

func truncateForLog(b []byte) string {
	if len(b) <= maxLogBodyBytes {
		return string(b)
	}
	return fmt.Sprintf("%s...(%d bytes total)", b[:maxLogBodyBytes], len(b))
}

func loggerFrom(ctx context.Context) log.Logger {
	logger, err := util.LoggerFromContext(ctx)
	if err != nil {
		return nil
	}
	return logger
}

// instruments are the OTel counterparts of graphql_gateway's
// graphql_labels_query_service_* Prometheus series. They are taken from the
// global meter provider, which the toolbox sets up when telemetry export is on
// (a no-op provider otherwise).
type instruments struct {
	requests  metric.Int64Counter
	addresses metric.Int64Counter
	updated   metric.Int64Counter
	duration  metric.Float64Histogram
}

var (
	instrumentsOnce sync.Once
	instrumentsVal  *instruments
)

func getInstruments() *instruments {
	instrumentsOnce.Do(func() {
		meter := otel.Meter(telemetry.MetricName)
		fallback := noop.Meter{}
		inst := &instruments{}
		var err error
		if inst.requests, err = meter.Int64Counter("toolbox.bitquery.labels_query_service.requests",
			metric.WithDescription("Calls to the external labels-query-service, by user and result (success/failure)."),
			metric.WithUnit("{request}")); err != nil {
			inst.requests, _ = fallback.Int64Counter("")
		}
		if inst.addresses, err = meter.Int64Counter("toolbox.bitquery.labels_query_service.addresses",
			metric.WithDescription("Addresses sent to the labels-query-service, by user and result."),
			metric.WithUnit("{address}")); err != nil {
			inst.addresses, _ = fallback.Int64Counter("")
		}
		if inst.updated, err = meter.Int64Counter("toolbox.bitquery.labels_query_service.updated_addresses",
			metric.WithDescription("Addresses the labels-query-service reported as updated, by user."),
			metric.WithUnit("{address}")); err != nil {
			inst.updated, _ = fallback.Int64Counter("")
		}
		if inst.duration, err = meter.Float64Histogram("toolbox.bitquery.labels_query_service.request.duration",
			metric.WithDescription("Duration of labels-query-service calls, including SRV resolution."),
			metric.WithUnit("s")); err != nil {
			inst.duration, _ = fallback.Float64Histogram("")
		}
		instrumentsVal = inst
	})
	return instrumentsVal
}
