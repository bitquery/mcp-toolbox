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

// Bitquery-specific: keep internal addresses out of tool errors. A transport
// failure of a data-source client (timeout, refused connection, DNS failure, a
// connection dropped mid-response) comes back from net/http as *url.Error,
// *net.OpError or *net.DNSError, and their text carries the full request URL —
// scheme, internal host, port and every query parameter (user, password, server
// settings, the billing query_id) — or the dialled and resolver IPs. The tool
// layer hands err.Error() to the model verbatim (and records it as the metric
// error.type), so the sources swap such an error for an address-free message and
// write the original to the server log instead.
package util

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// BitqueryTransportError is the caller-facing replacement for a transport
// failure. Error() is the classified message only. Unwrap() deliberately exposes
// just a sentinel — context.DeadlineExceeded for a timeout, context.Canceled for a
// cancelled request, nothing otherwise — and never the original error, whose text
// holds the address, so a later fmt.Errorf("%w") or errors.Unwrap cannot bring it
// back.
type BitqueryTransportError struct {
	msg      string
	sentinel error
}

func (e *BitqueryTransportError) Error() string { return e.msg }

func (e *BitqueryTransportError) Unwrap() error { return e.sentinel }

// BitqueryIsTransportError reports whether err is a network-level failure of the
// request (no answer from the data service) rather than an answer it gave, such
// as a database error body.
func BitqueryIsTransportError(err error) bool {
	if err == nil {
		return false
	}
	var urlErr *url.Error
	var opErr *net.OpError
	var dnsErr *net.DNSError
	return errors.As(err, &urlErr) || errors.As(err, &opErr) || errors.As(err, &dnsErr) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || isNetTimeout(err)
}

// BitquerySanitizeTransportError returns the address-free replacement for a
// transport failure, or err itself when it is not one. timeout is the configured
// request timeout quoted in a timeout message; pass 0 when it is unknown or was
// not what expired.
func BitquerySanitizeTransportError(err error, timeout time.Duration) error {
	if !bitqueryNeedsSanitizing(err) {
		return err
	}
	var sentinel error
	switch {
	case errors.Is(err, context.Canceled):
		sentinel = context.Canceled
	case errors.Is(err, context.DeadlineExceeded), isNetTimeout(err):
		sentinel = context.DeadlineExceeded
	}
	return &BitqueryTransportError{msg: bitqueryTransportMessage(err, timeout), sentinel: sentinel}
}

// BitqueryTransportFailure is what a source calls on a request error: it returns
// err unchanged unless it is a transport failure; for one, it logs the original
// error (query-string secrets masked) and returns the sanitized replacement. A
// cancelled request is logged at WARN (the caller went away), everything else at
// ERROR, so operators keep the host, port and cause without DEBUG logging.
func BitqueryTransportFailure(ctx context.Context, sourceName string, err error, timeout time.Duration) error {
	if !bitqueryNeedsSanitizing(err) {
		return err
	}
	sanitized := BitquerySanitizeTransportError(err, timeout)
	if logger, lerr := LoggerFromContext(ctx); lerr == nil {
		args := []any{"source", sourceName, "reported", sanitized.Error(), "error", bitqueryRedactErrorForLog(err)}
		if errors.Is(err, context.Canceled) {
			logger.WarnContext(ctx, "data source request cancelled", args...)
		} else {
			logger.ErrorContext(ctx, "data source transport error", args...)
		}
	}
	return sanitized
}

// bitqueryNeedsSanitizing is true for a transport failure that has not already
// been replaced (re-sanitizing would only log the same failure twice).
func bitqueryNeedsSanitizing(err error) bool {
	var done *BitqueryTransportError
	return BitqueryIsTransportError(err) && !errors.As(err, &done)
}

func bitqueryTransportMessage(err error, timeout time.Duration) string {
	var dnsErr *net.DNSError
	var opErr *net.OpError
	switch {
	case errors.Is(err, context.Canceled):
		return "the request to the data service was cancelled before it answered"
	case errors.As(err, &dnsErr):
		return "the data service address could not be resolved; retry later"
	case errors.Is(err, context.DeadlineExceeded), isNetTimeout(err):
		limit := "in time"
		if timeout > 0 {
			limit = "within " + bitqueryFormatSeconds(timeout)
		}
		return fmt.Sprintf("the data service did not answer %s; the query may be too heavy, narrow it (shorter time window, fewer rows) or retry later", limit)
	case errors.Is(err, syscall.ECONNREFUSED):
		return "could not connect to the data service (connection refused); retry later"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE),
		errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "the connection to the data service was interrupted; retry later"
	case errors.As(err, &opErr) && opErr.Op == "dial":
		return "could not connect to the data service; retry later"
	default:
		return "the request to the data service failed; retry later"
	}
}

func isNetTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// bitqueryFormatSeconds renders a timeout as "180 seconds" / "0.3 seconds".
func bitqueryFormatSeconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64) + " seconds"
}

// bitquerySecretQueryParams are query-parameter names whose non-empty values are
// masked in the logged error. net/http already masks a userinfo password, but not
// a password passed as a query parameter (the ClickHouse HTTP interface accepts
// ?user=&password=).
var bitquerySecretQueryParams = map[string]bool{
	"password": true, "passwd": true, "key": true, "api_key": true, "api-key": true,
	"apikey": true, "token": true, "access_token": true,
}

// bitqueryRedactErrorForLog is err.Error() with secret query-parameter values in
// the request URL replaced by "***"; everything else, host and port included, is
// kept for the operator.
func bitqueryRedactErrorForLog(err error) string {
	text := err.Error()
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.URL != "" {
		if redacted := bitqueryRedactURLSecrets(urlErr.URL); redacted != urlErr.URL {
			text = strings.ReplaceAll(text, urlErr.URL, redacted)
		}
	}
	return text
}

func bitqueryRedactURLSecrets(raw string) string {
	base, query, found := strings.Cut(raw, "?")
	if !found || query == "" {
		return raw
	}
	fragment := ""
	if i := strings.IndexByte(query, '#'); i >= 0 {
		query, fragment = query[:i], query[i:]
	}
	parts := strings.Split(query, "&")
	changed := false
	for i, part := range parts {
		name, value, hasValue := strings.Cut(part, "=")
		if !hasValue || value == "" {
			continue
		}
		if decoded, derr := url.QueryUnescape(name); derr == nil {
			name = decoded
		}
		if bitquerySecretQueryParams[strings.ToLower(name)] {
			parts[i] = part[:len(part)-len(value)] + "***"
			changed = true
		}
	}
	if !changed {
		return raw
	}
	return base + "?" + strings.Join(parts, "&") + fragment
}
