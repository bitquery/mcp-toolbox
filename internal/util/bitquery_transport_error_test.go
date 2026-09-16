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
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/googleapis/mcp-toolbox/internal/log"
)

const bitqueryLeakyURL = "http://ch-bsc-api.streaming-cluster.local:8123/?default_format=JSONEachRow&password=s3cr3t&user=mcp"

// fakeTimeout mimics net/http's unexported timeoutError.
type fakeTimeout struct{}

func (fakeTimeout) Error() string {
	return "context deadline exceeded (Client.Timeout exceeded while awaiting headers)"
}
func (fakeTimeout) Timeout() bool   { return true }
func (fakeTimeout) Temporary() bool { return true }

func bitqueryAssertNoAddress(t *testing.T, text string) {
	t.Helper()
	for _, leak := range []string{"ch-bsc-api", "streaming-cluster", "8123", "10.1.2.3", "http://", "?", "user=", "password", "s3cr3t", "mcp"} {
		if strings.Contains(text, leak) {
			t.Fatalf("sanitized error %q still contains %q", text, leak)
		}
	}
}

func TestBitquerySanitizeTransportErrorClasses(t *testing.T) {
	dialRefused := &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.ParseIP("10.1.2.3"), Port: 8123},
		Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	tcs := []struct {
		desc         string
		err          error
		timeout      time.Duration
		want         string
		wantDeadline bool
		wantCanceled bool
	}{
		{
			desc:         "client timeout quotes the configured limit",
			err:          &url.Error{Op: "Post", URL: bitqueryLeakyURL, Err: fakeTimeout{}},
			timeout:      180 * time.Second,
			want:         "the data service did not answer within 180 seconds",
			wantDeadline: true,
		},
		{
			desc:         "timeout with unknown limit",
			err:          fmt.Errorf("sendQuery: %w", &url.Error{Op: "Post", URL: bitqueryLeakyURL, Err: context.DeadlineExceeded}),
			want:         "the data service did not answer in time",
			wantDeadline: true,
		},
		{
			desc: "connection refused",
			err:  &url.Error{Op: "Post", URL: bitqueryLeakyURL, Err: dialRefused},
			want: "could not connect to the data service (connection refused)",
		},
		{
			desc: "dns failure",
			err: &url.Error{Op: "Post", URL: bitqueryLeakyURL, Err: &net.OpError{Op: "dial", Net: "tcp",
				Err: &net.DNSError{Err: "no such host", Name: "ch-bsc-api.streaming-cluster.local", Server: "10.1.2.3:53", IsNotFound: true}}},
			want: "the data service address could not be resolved",
		},
		{
			desc: "other dial failure",
			err: &url.Error{Op: "Post", URL: bitqueryLeakyURL, Err: &net.OpError{Op: "dial", Net: "tcp",
				Addr: &net.TCPAddr{IP: net.ParseIP("10.1.2.3"), Port: 8123}, Err: errors.New("connection to blocked IP 10.1.2.3 denied")}},
			want: "could not connect to the data service",
		},
		{
			desc: "connection reset while reading the body",
			err: &net.OpError{Op: "read", Net: "tcp", Addr: &net.TCPAddr{IP: net.ParseIP("10.1.2.3"), Port: 8123},
				Err: os.NewSyscallError("read", syscall.ECONNRESET)},
			want: "the connection to the data service was interrupted",
		},
		{
			desc: "server closed the connection before answering",
			err:  &url.Error{Op: "Post", URL: bitqueryLeakyURL, Err: io.EOF},
			want: "the connection to the data service was interrupted",
		},
		{
			desc:         "cancelled by the caller",
			err:          &url.Error{Op: "Post", URL: bitqueryLeakyURL, Err: context.Canceled},
			want:         "the request to the data service was cancelled",
			wantCanceled: true,
		},
		{
			desc: "unclassified url error",
			err:  &url.Error{Op: "Post", URL: bitqueryLeakyURL, Err: errors.New("stopped after 10 redirects")},
			want: "the request to the data service failed",
		},
	}
	for _, tc := range tcs {
		t.Run(tc.desc, func(t *testing.T) {
			if !BitqueryIsTransportError(tc.err) {
				t.Fatalf("BitqueryIsTransportError(%v) = false", tc.err)
			}
			got := BitquerySanitizeTransportError(tc.err, tc.timeout)
			if !strings.Contains(got.Error(), tc.want) {
				t.Fatalf("got %q, want it to contain %q", got.Error(), tc.want)
			}
			bitqueryAssertNoAddress(t, got.Error())
			// Wrapping it again must not bring the address back.
			bitqueryAssertNoAddress(t, fmt.Errorf("error making HTTP request: %w", got).Error())
			if errors.Is(got, context.DeadlineExceeded) != tc.wantDeadline {
				t.Fatalf("errors.Is(DeadlineExceeded) = %v, want %v", !tc.wantDeadline, tc.wantDeadline)
			}
			if errors.Is(got, context.Canceled) != tc.wantCanceled {
				t.Fatalf("errors.Is(Canceled) = %v, want %v", !tc.wantCanceled, tc.wantCanceled)
			}
			var urlErr *url.Error
			var opErr *net.OpError
			var dnsErr *net.DNSError
			if errors.As(got, &urlErr) || errors.As(got, &opErr) || errors.As(got, &dnsErr) {
				t.Fatalf("sanitized error still unwraps to the original address-bearing error")
			}
		})
	}
}

func TestBitquerySanitizeTransportErrorLeavesOtherErrors(t *testing.T) {
	dbErr := errors.New(`failed to query server hello: [HTTP 500] response body: "Code: 62. DB::Exception: Syntax error"`)
	for _, err := range []error{nil, dbErr, fmt.Errorf("unexpected status code: 500, response body: Code: 62")} {
		if BitqueryIsTransportError(err) {
			t.Fatalf("BitqueryIsTransportError(%v) = true", err)
		}
		if got := BitquerySanitizeTransportError(err, time.Second); got != err {
			t.Fatalf("non-transport error changed: got %v, want %v", got, err)
		}
		if got := BitqueryTransportFailure(context.Background(), "src", err, time.Second); got != err {
			t.Fatalf("non-transport error changed: got %v, want %v", got, err)
		}
	}
}

func TestBitqueryTransportFailureLogsOriginal(t *testing.T) {
	var logs bytes.Buffer
	logger, err := log.NewLogger("standard", log.Info, &logs, &logs)
	if err != nil {
		t.Fatalf("failed to create logger: %v", err)
	}
	ctx := WithLogger(context.Background(), logger)

	orig := &url.Error{Op: "Post", URL: bitqueryLeakyURL, Err: fakeTimeout{}}
	got := BitqueryTransportFailure(ctx, "clickhouse-bsc-http", orig, 180*time.Second)
	bitqueryAssertNoAddress(t, got.Error())
	// Sanitizing an already sanitized error is a no-op and logs nothing more.
	if again := BitqueryTransportFailure(ctx, "clickhouse-bsc-http", fmt.Errorf("wrap: %w", got), 0); !strings.HasSuffix(again.Error(), got.Error()) {
		t.Fatalf("re-sanitizing changed the message: %q", again.Error())
	}

	out := logs.String()
	for _, want := range []string{"ERROR", "data source transport error", "clickhouse-bsc-http", "ch-bsc-api.streaming-cluster.local:8123", "Client.Timeout exceeded", "password=***", "user=mcp"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log %q does not contain %q", out, want)
		}
	}
	if strings.Contains(out, "s3cr3t") {
		t.Fatalf("log leaks the password query value: %q", out)
	}
	if n := strings.Count(out, "data source transport error"); n != 1 {
		t.Fatalf("expected one log line, got %d: %q", n, out)
	}

	logs.Reset()
	BitqueryTransportFailure(ctx, "clickhouse-bsc-http", &url.Error{Op: "Post", URL: bitqueryLeakyURL, Err: context.Canceled}, 0)
	if out := logs.String(); !strings.Contains(out, "WARN") || !strings.Contains(out, "data source request cancelled") {
		t.Fatalf("cancelled request not logged at WARN: %q", out)
	}
}

func TestBitqueryRedactURLSecrets(t *testing.T) {
	tcs := map[string]string{
		"http://h:1/?user=mcp&password=":                   "http://h:1/?user=mcp&password=",
		"http://h:1/?user=mcp&password=x&a=1":              "http://h:1/?user=mcp&password=***&a=1",
		"http://h:1/?Api-Key=abc#frag":                     "http://h:1/?Api-Key=***#frag",
		"http://h:1/path":                                  "http://h:1/path",
		"http://h:1/?query_id=a%3Ab&pass%77ord=q&token=zz": "http://h:1/?query_id=a%3Ab&pass%77ord=***&token=***",
	}
	for in, want := range tcs {
		if got := bitqueryRedactURLSecrets(in); got != want {
			t.Errorf("bitqueryRedactURLSecrets(%q) = %q, want %q", in, got, want)
		}
	}
	if got := bitqueryFormatSeconds(300 * time.Millisecond); got != "0.3 seconds" {
		t.Errorf("bitqueryFormatSeconds(300ms) = %q", got)
	}
}
