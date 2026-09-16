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

package http

// Bitquery: bitquerySourceRoutes must send a call only to the source its
// parameter names, and must refuse — without sending anything anywhere — every
// value that is not literally one of the configured keys.

import (
	"bytes"
	"context"
	"io"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	yaml "github.com/goccy/go-yaml"
	"github.com/googleapis/mcp-toolbox/internal/log"
	"github.com/googleapis/mcp-toolbox/internal/sources"
	httpsrc "github.com/googleapis/mcp-toolbox/internal/sources/http"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

type bitqueryHit struct {
	database string // X-Bitquery-Database header as received
	body     string
	query    string
}

type bitqueryBackend struct {
	server *httptest.Server
	mu     sync.Mutex
	hits   []bitqueryHit
}

func newBitqueryBackend(t *testing.T) *bitqueryBackend {
	t.Helper()
	b := &bitqueryBackend{}
	b.server = httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		body, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.hits = append(b.hits, bitqueryHit{database: r.Header.Get("X-Bitquery-Database"), body: string(body), query: r.URL.RawQuery})
		b.mu.Unlock()
		_, _ = w.Write([]byte(`[{"ok":1}]`))
	}))
	t.Cleanup(b.server.Close)
	return b
}

func (b *bitqueryBackend) calls() []bitqueryHit {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]bitqueryHit(nil), b.hits...)
}

func bitqueryRoutesContext(t *testing.T) context.Context {
	t.Helper()
	logger, err := log.NewLogger("standard", log.Info, &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("failed to create logger: %v", err)
	}
	return util.WithLogger(context.Background(), logger)
}

func bitqueryRoutesSource(t *testing.T, ctx context.Context, name, baseURL string, headers map[string]string, legacyPaths string) sources.Source {
	t.Helper()
	cfg := httpsrc.Config{
		Name:                 name,
		Type:                 httpsrc.SourceType,
		BaseURL:              baseURL,
		Timeout:              "5s",
		DefaultHeaders:       headers,
		QueryParams:          map[string]string{"user": "mcp"},
		AllowPrivateNetworks: true,
		LegacyBillingPaths:   legacyPaths,
	}
	s, err := cfg.Initialize(ctx, nil)
	if err != nil {
		t.Fatalf("failed to initialize source %s: %v", name, err)
	}
	return s
}

type bitqueryFixture struct {
	ctx     context.Context
	gateway *bitqueryBackend // both *_api sources point here, each with its own routing header
	legacy  *bitqueryBackend
	sources tools.SourceMap
}

func newBitqueryFixture(t *testing.T) *bitqueryFixture {
	t.Helper()
	ctx := bitqueryRoutesContext(t)
	f := &bitqueryFixture{ctx: ctx, gateway: newBitqueryBackend(t), legacy: newBitqueryBackend(t)}
	f.sources = tools.SourceMap{
		"gw-eth":     bitqueryRoutesSource(t, ctx, "gw-eth", f.gateway.server.URL, map[string]string{"X-Bitquery-Database": "eth_api"}, ""),
		"gw-bsc":     bitqueryRoutesSource(t, ctx, "gw-bsc", f.gateway.server.URL, map[string]string{"X-Bitquery-Database": "bsc_api"}, ""),
		"legacy-btc": bitqueryRoutesSource(t, ctx, "legacy-btc", f.legacy.server.URL, nil, "bitcoin/transfers"),
	}
	return f
}

func bitqueryRoutedConfig() Config {
	return Config{
		ConfigBase:  tools.ConfigBase{Name: "chain_list_tables", Description: "lists tables"},
		Type:        resourceType,
		Source:      "gw-eth",
		Path:        "/",
		Method:      "POST",
		RequestBody: "SELECT name FROM system.tables WHERE database = '{{.database}}'",
		BodyParams:  parameters.Parameters{parameters.NewStringParameter("database", "database")},
		SourceRoutes: &SourceRoutes{
			Param: "database",
			Sources: map[string]string{
				"eth_api":      "gw-eth",
				"bsc_api":      "gw-bsc",
				"bitcoin":      "legacy-btc",
				"bitcoin_flow": "legacy-btc",
			},
		},
	}
}

func (f *bitqueryFixture) tool(t *testing.T, cfg Config) Tool {
	t.Helper()
	tl, err := cfg.Initialize(tools.WithSourceLookup(f.ctx, f.sources))
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return tl.(Tool)
}

func (f *bitqueryFixture) invoke(tl Tool, database any) (any, util.ToolboxError) {
	params := parameters.ParamValues{{Name: "database", Value: database}}
	// the server always hands Invoke the tool's own declared source
	return tl.Invoke(f.ctx, f.sources["gw-eth"], params, "")
}

func TestBitquerySourceRoutesSendToTheNamedSource(t *testing.T) {
	f := newBitqueryFixture(t)
	tl := f.tool(t, bitqueryRoutedConfig())

	for _, db := range []string{"bsc_api", "eth_api", "bitcoin_flow"} {
		if _, err := f.invoke(tl, db); err != nil {
			t.Fatalf("invoke %s: %v", db, err)
		}
	}

	gw := f.gateway.calls()
	if len(gw) != 2 {
		t.Fatalf("gateway got %d calls, want 2: %+v", len(gw), gw)
	}
	for i, want := range []string{"bsc_api", "eth_api"} {
		if gw[i].database != want {
			t.Errorf("call %d: routing header %q, want %q (it must come from the routed source)", i, gw[i].database, want)
		}
		if !strings.Contains(gw[i].body, "database = '"+want+"'") {
			t.Errorf("call %d: body %q does not carry %s", i, gw[i].body, want)
		}
		if !strings.Contains(gw[i].query, "user=mcp") {
			t.Errorf("call %d: query %q lost the source query params", i, gw[i].query)
		}
	}
	lg := f.legacy.calls()
	if len(lg) != 1 || lg[0].database != "" || !strings.Contains(lg[0].body, "'bitcoin_flow'") {
		t.Fatalf("legacy backend calls = %+v, want one bitcoin_flow call without a routing header", lg)
	}
}

func TestBitquerySourceRoutesRefuseBeforeAnyRequest(t *testing.T) {
	f := newBitqueryFixture(t)
	tl := f.tool(t, bitqueryRoutedConfig())

	refused := []any{
		"marketdata", "api_v2", "system", "default", "trading_rt", "solana", "eth_api_s1",
		"BSC_API", " bsc_api", "bsc_api ", "bsc_api\n", "bsc_api' OR 1=1 --", "", nil, 42,
		strings.Repeat("x", 500),
	}
	for _, v := range refused {
		_, err := f.invoke(tl, v)
		if err == nil {
			t.Fatalf("value %q was not refused", v)
		}
		if err.Category() != util.CategoryAgent {
			t.Errorf("value %q: error category %v, want agent", v, err.Category())
		}
		msg := err.Error()
		if !strings.Contains(msg, "use one of: bitcoin, bitcoin_flow, bsc_api, eth_api") {
			t.Errorf("value %q: message %q does not list the allowed values", v, msg)
		}
		if strings.Contains(msg, "gw-") || strings.Contains(msg, "legacy-btc") || strings.Contains(msg, "127.0.0.1") {
			t.Errorf("value %q: message %q names an internal source or address", v, msg)
		}
		if len(msg) > 200 {
			t.Errorf("value %q: message is %d bytes, the echoed value is not capped", v, len(msg))
		}
	}
	if n := len(f.gateway.calls()) + len(f.legacy.calls()); n != 0 {
		t.Fatalf("refused values sent %d request(s)", n)
	}
}

func TestBitquerySourceRoutesWithoutSourcesRefuseEveryCall(t *testing.T) {
	// Offline initialization (no source lookup in the context) must still load the
	// tool, and must never fall back to the tool's own source when invoked.
	f := newBitqueryFixture(t)
	tl, err := bitqueryRoutedConfig().Initialize(f.ctx)
	if err != nil {
		t.Fatalf("offline initialize: %v", err)
	}
	if _, err := f.invoke(tl.(Tool), "eth_api"); err == nil {
		t.Fatalf("an uninitialized route was invoked")
	}
	if n := len(f.gateway.calls()) + len(f.legacy.calls()); n != 0 {
		t.Fatalf("sent %d request(s)", n)
	}
}

func TestBitquerySourceRoutesConfigErrors(t *testing.T) {
	f := newBitqueryFixture(t)
	withLookup := tools.WithSourceLookup(f.ctx, f.sources)
	notHTTP := tools.SourceMap{"gw-eth": f.sources["gw-eth"], "gw-bsc": f.sources["gw-bsc"], "legacy-btc": nonHTTPSource{}}

	cases := []struct {
		name   string
		ctx    context.Context
		mutate func(*Config)
		want   string
	}{
		{"no param", withLookup, func(c *Config) { c.SourceRoutes.Param = "" }, "param is required"},
		{"no sources", withLookup, func(c *Config) { c.SourceRoutes.Sources = nil }, "sources is empty"},
		{"unknown param", withLookup, func(c *Config) { c.SourceRoutes.Param = "db" }, `"db" is not a parameter`},
		{"int param", withLookup, func(c *Config) {
			c.BodyParams = parameters.Parameters{parameters.NewIntParameter("database", "database")}
		}, "must be a string parameter"},
		{"escaped param", withLookup, func(c *Config) {
			c.BodyParams = parameters.Parameters{parameters.NewStringParameter("database", "database", parameters.WithStringEscape("single-quotes"))}
		}, "must not declare escape"},
		{"empty key", withLookup, func(c *Config) { c.SourceRoutes.Sources[""] = "gw-eth" }, "empty value or source name"},
		{"own source not a target", withLookup, func(c *Config) { c.Source = "gw-other" }, "must be one of the bitquerySourceRoutes targets"},
		{"default not a key", withLookup, func(c *Config) {
			c.BodyParams = parameters.Parameters{parameters.NewStringParameter("database", "database", parameters.WithStringDefault("marketdata"))}
		}, "is not a bitquerySourceRoutes value"},
		{"missing target", withLookup, func(c *Config) { c.SourceRoutes.Sources["tron_api"] = "gw-tron" }, "unable to retrieve source gw-tron"},
		{"target not http", tools.WithSourceLookup(f.ctx, notHTTP), func(c *Config) {}, "legacy-btc named in bitquerySourceRoutes is not an http source"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := bitqueryRoutedConfig()
			tc.mutate(&cfg)
			_, err := cfg.Initialize(tc.ctx)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestBitquerySourceRoutesDefaultThatIsAKey(t *testing.T) {
	f := newBitqueryFixture(t)
	cfg := bitqueryRoutedConfig()
	cfg.BodyParams = parameters.Parameters{parameters.NewStringParameter("database", "database", parameters.WithStringDefault("eth_api"))}
	if _, err := cfg.Initialize(tools.WithSourceLookup(f.ctx, f.sources)); err != nil {
		t.Fatalf("a default that is a route value must load: %v", err)
	}
}

func TestBitquerySourceRoutesUnroutedToolUnchanged(t *testing.T) {
	f := newBitqueryFixture(t)
	cfg := bitqueryRoutedConfig()
	cfg.SourceRoutes = nil
	tl := f.tool(t, cfg)
	// without routes any value goes to the tool's own source, exactly as before
	if _, err := f.invoke(tl, "bsc_api"); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	gw := f.gateway.calls()
	if len(gw) != 1 || gw[0].database != "eth_api" {
		t.Fatalf("gateway calls = %+v, want one call with the tool source's own header", gw)
	}
}

func TestBitquerySourceRoutesYAML(t *testing.T) {
	in := `
source: gw-eth
bitquerySourceRoutes:
  param: database
  sources:
    eth_api: gw-eth
    bitcoin: legacy-btc
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(in), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.SourceRoutes == nil || cfg.SourceRoutes.Param != "database" ||
		cfg.SourceRoutes.Sources["eth_api"] != "gw-eth" || cfg.SourceRoutes.Sources["bitcoin"] != "legacy-btc" {
		t.Fatalf("parsed routes = %+v", cfg.SourceRoutes)
	}
}

type nonHTTPSource struct{}

func (nonHTTPSource) SourceType() string             { return "not-http" }
func (nonHTTPSource) ToConfig() sources.SourceConfig { return nil }
