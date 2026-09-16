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

// Bitquery: a transport failure must not hand the caller (the model) the request
// URL — internal host, port, query params — while the server log keeps it.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/googleapis/mcp-toolbox/internal/log"
	"github.com/googleapis/mcp-toolbox/internal/util"
)

const bitqueryTestQuery = "/?default_format=JSONEachRow&output_format_json_array_of_rows=1&password=s3cr3t&user=mcp"

func bitqueryTestContext(t *testing.T) (context.Context, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	logger, err := log.NewLogger("standard", log.Info, logs, logs)
	if err != nil {
		t.Fatalf("failed to create logger: %v", err)
	}
	return util.WithLogger(context.Background(), logger), logs
}

func bitqueryTestSource(t *testing.T, ctx context.Context, baseURL, timeout string) *Source {
	t.Helper()
	cfg := Config{
		Name:                 "clickhouse-bsc-http",
		Type:                 SourceType,
		BaseURL:              baseURL,
		Timeout:              timeout,
		ReturnFullError:      true,
		AllowPrivateNetworks: true,
	}
	initialized, err := cfg.Initialize(ctx, nil)
	if err != nil {
		t.Fatalf("failed to initialize source: %v", err)
	}
	return initialized.(*Source)
}

func bitqueryRun(t *testing.T, ctx context.Context, source *Source, target string) (any, error) {
	t.Helper()
	req, err := nethttp.NewRequestWithContext(ctx, nethttp.MethodPost, target, strings.NewReader("SELECT 1"))
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	return source.RunRequest(ctx, req)
}

// bitqueryAssertSanitized checks the caller-facing error names no part of the
// target URL, and that the server log does.
func bitqueryAssertSanitized(t *testing.T, err error, target string, logs *bytes.Buffer, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error")
	}
	text := err.Error()
	host, port, splitErr := net.SplitHostPort(strings.TrimPrefix(strings.SplitN(target, "/?", 2)[0], "http://"))
	if splitErr != nil {
		t.Fatalf("bad target %q: %v", target, splitErr)
	}
	for _, leak := range []string{host, port, "http://", "?", "user=", "password", "s3cr3t", "default_format", "dial tcp", "lookup"} {
		if strings.Contains(text, leak) {
			t.Fatalf("caller-facing error %q contains %q", text, leak)
		}
	}
	if !strings.HasPrefix(text, "error making HTTP request: ") || !strings.Contains(text, want) {
		t.Fatalf("caller-facing error %q, want it to contain %q", text, want)
	}
	out := logs.String()
	for _, keep := range []string{"data source transport error", "clickhouse-bsc-http", net.JoinHostPort(host, port), "user=mcp", "password=***"} {
		if !strings.Contains(out, keep) {
			t.Fatalf("server log %q does not contain %q", out, keep)
		}
	}
	if strings.Contains(out, "s3cr3t") {
		t.Fatalf("server log leaks the password value: %q", out)
	}
}

// bitquerySilentHandler never answers. It drains the request body first: only
// then does the server watch the connection and cancel r.Context() when the
// client gives up, so server.Close() does not wait out the whole sleep.
func bitquerySilentHandler(w nethttp.ResponseWriter, r *nethttp.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	select {
	case <-r.Context().Done():
	case <-time.After(10 * time.Second):
	}
}

func TestBitqueryRunRequestTimeoutIsSanitized(t *testing.T) {
	server := httptest.NewServer(nethttp.HandlerFunc(bitquerySilentHandler))
	defer server.Close()

	ctx, logs := bitqueryTestContext(t)
	source := bitqueryTestSource(t, ctx, server.URL, "300ms")
	target := server.URL + bitqueryTestQuery
	_, err := bitqueryRun(t, ctx, source, target)
	bitqueryAssertSanitized(t, err, target, logs, "the data service did not answer within 0.3 seconds")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("errors.Is(err, context.DeadlineExceeded) = false for %q", err)
	}
	if !strings.Contains(logs.String(), "Client.Timeout exceeded") {
		t.Fatalf("server log lost the original cause: %q", logs.String())
	}
}

func TestBitqueryRunRequestBodyReadTimeoutIsSanitized(t *testing.T) {
	server := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		w.WriteHeader(nethttp.StatusOK)
		_, _ = w.Write([]byte("[{"))
		w.(nethttp.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer server.Close()

	ctx, logs := bitqueryTestContext(t)
	source := bitqueryTestSource(t, ctx, server.URL, "300ms")
	_, err := bitqueryRun(t, ctx, source, server.URL+bitqueryTestQuery)
	if err == nil || !strings.Contains(err.Error(), "did not answer within 0.3 seconds") {
		t.Fatalf("got %v, want a sanitized timeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("errors.Is(err, context.DeadlineExceeded) = false for %q", err)
	}
	if !strings.Contains(logs.String(), "while reading body") {
		t.Fatalf("server log lost the original cause: %q", logs.String())
	}
}

func TestBitqueryRunRequestConnectionRefusedIsSanitized(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	baseURL := "http://" + listener.Addr().String()
	_ = listener.Close()

	ctx, logs := bitqueryTestContext(t)
	source := bitqueryTestSource(t, ctx, baseURL, "5s")
	target := baseURL + bitqueryTestQuery
	_, err = bitqueryRun(t, ctx, source, target)
	bitqueryAssertSanitized(t, err, target, logs, "could not connect to the data service (connection refused)")
	if !strings.Contains(logs.String(), "connection refused") {
		t.Fatalf("server log lost the original cause: %q", logs.String())
	}
}

func TestBitqueryRunRequestUnresolvableHostIsSanitized(t *testing.T) {
	// A resolver that fails at once keeps the test off the machine's DNS; the
	// error still comes back as a *net.DNSError for the .invalid name.
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return nil, errors.New("resolver unreachable")
		},
	}
	client, err := createHTTPClient(5*time.Second, &nethttp.Transport{}, &SSRFGuard{AllowPrivateNetworks: true}, resolver)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	baseURL := "http://ch-bsc-api.streaming-cluster.invalid:8123"
	source := &Source{Config: Config{Name: "clickhouse-bsc-http", Type: SourceType, BaseURL: baseURL}, client: client}

	ctx, logs := bitqueryTestContext(t)
	target := baseURL + bitqueryTestQuery
	_, err = bitqueryRun(t, ctx, source, target)
	bitqueryAssertSanitized(t, err, target, logs, "the data service address could not be resolved")
	for _, leak := range []string{"streaming-cluster", "invalid"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("caller-facing error %q contains %q", err.Error(), leak)
		}
	}
	if !strings.Contains(logs.String(), "lookup ch-bsc-api.streaming-cluster.invalid") {
		t.Fatalf("server log lost the original cause: %q", logs.String())
	}
}

func TestBitqueryRunRequestConnectionResetIsSanitized(t *testing.T) {
	server := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(nethttp.StatusOK)
		_, _ = w.Write([]byte("[{"))
		w.(nethttp.Flusher).Flush()
		conn, _, err := w.(nethttp.Hijacker).Hijack()
		if err != nil {
			return
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0) // close with RST
		}
		_ = conn.Close()
	}))
	defer server.Close()

	ctx, logs := bitqueryTestContext(t)
	source := bitqueryTestSource(t, ctx, server.URL, "5s")
	_, err := bitqueryRun(t, ctx, source, server.URL+bitqueryTestQuery)
	if err == nil {
		t.Fatalf("expected an error")
	}
	// Depending on timing the read ends in a reset (names both IPs) or a short body.
	if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "tcp") {
		t.Fatalf("caller-facing error %q names the connection", err.Error())
	}
	t.Logf("caller-facing: %q; log: %q", err.Error(), logs.String())
}

func TestBitqueryRunRequestCancelIsSanitized(t *testing.T) {
	server := httptest.NewServer(nethttp.HandlerFunc(bitquerySilentHandler))
	defer server.Close()

	baseCtx, logs := bitqueryTestContext(t)
	source := bitqueryTestSource(t, baseCtx, server.URL, "5s")
	ctx, cancel := context.WithCancel(baseCtx)
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err := bitqueryRun(t, ctx, source, server.URL+bitqueryTestQuery)
	if err == nil || !strings.Contains(err.Error(), "cancelled") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("got %v, want a sanitized cancellation", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("errors.Is(err, context.Canceled) = false for %q", err)
	}
	if !strings.Contains(logs.String(), "WARN") {
		t.Fatalf("cancellation not logged at WARN: %q", logs.String())
	}
}

func TestBitqueryRunRequestSuccessUnchanged(t *testing.T) {
	server := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		_, _ = w.Write([]byte(`[{"Holder":"0xabc","Balance":1.5}]`))
	}))
	defer server.Close()

	ctx, logs := bitqueryTestContext(t)
	source := bitqueryTestSource(t, ctx, server.URL, "5s")
	got, err := bitqueryRun(t, ctx, source, server.URL+bitqueryTestQuery)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []any{map[string]any{"Holder": "0xabc", "Balance": 1.5}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("unexpected result (-want +got):\n%s", diff)
	}
	if strings.Contains(logs.String(), "data source") {
		t.Fatalf("unexpected transport log output: %q", logs.String())
	}
}

func TestBitqueryRunRequestFullErrorBodyUnchanged(t *testing.T) {
	server := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		w.WriteHeader(nethttp.StatusInternalServerError)
		_, _ = w.Write([]byte("Code: 62. DB::Exception: Syntax error"))
	}))
	defer server.Close()

	ctx, _ := bitqueryTestContext(t)
	source := bitqueryTestSource(t, ctx, server.URL, "5s")
	_, err := bitqueryRun(t, ctx, source, server.URL+bitqueryTestQuery)
	if err == nil || err.Error() != "unexpected status code: 500, response body: Code: 62. DB::Exception: Syntax error" {
		t.Fatalf("returnFullError body changed: %v", err)
	}
}
