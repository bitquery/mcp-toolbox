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

// Bitquery: the label statement through a real clickhouse source, against a
// server that answers ClickHouse's Native format over HTTP: the statement and
// its billing query_id arrive as the database would see them, and the rows
// come back through the driver.

import (
	"encoding/binary"
	"io"
	"net"
	nethttp "net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	chproto "github.com/ClickHouse/clickhouse-go/v2/lib/proto"
	chsrc "github.com/googleapis/mcp-toolbox/internal/sources/clickhouse"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"go.opentelemetry.io/otel"
)

type lbNativeColumn struct {
	name, typ string
	values    []any // string, uint8 or uint32
}

// lbNativeBlock encodes one block of the Native format as the driver reads it
// at the given protocol revision.
func lbNativeBlock(revision uint64, cols ...lbNativeColumn) []byte {
	var b []byte
	str := func(s string) {
		b = binary.AppendUvarint(b, uint64(len(s)))
		b = append(b, s...)
	}
	if revision > 0 {
		b = binary.AppendUvarint(b, 1)
		b = append(b, 0)
		b = binary.AppendUvarint(b, 2)
		b = binary.LittleEndian.AppendUint32(b, 0xffffffff)
		b = binary.AppendUvarint(b, 0)
	}
	rows := 0
	if len(cols) > 0 {
		rows = len(cols[0].values)
	}
	b = binary.AppendUvarint(b, uint64(len(cols)))
	b = binary.AppendUvarint(b, uint64(rows))
	for _, c := range cols {
		str(c.name)
		str(c.typ)
		if revision >= chproto.DBMS_MIN_REVISION_WITH_CUSTOM_SERIALIZATION {
			b = append(b, 0)
		}
		for _, v := range c.values {
			switch x := v.(type) {
			case string:
				str(x)
			case uint8:
				b = append(b, x)
			case uint32:
				b = binary.LittleEndian.AppendUint32(b, x)
			}
		}
	}
	return b
}

type lbNativeHit struct {
	query     url.Values
	statement string
}

// lbNativeServer answers the driver's handshake (revision 0 from then on), its
// ping, and label statements from a fixed table.
func lbNativeServer(t *testing.T, labels map[string][][3]string) (*httptest.Server, func() []lbNativeHit) {
	t.Helper()
	var mu sync.Mutex
	var hits []lbNativeHit
	server := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		body, _ := io.ReadAll(r.Body)
		statement := string(body)
		switch {
		case strings.Contains(statement, "displayName()"):
			_, _ = w.Write(lbNativeBlock(chproto.DBMS_TCP_PROTOCOL_VERSION,
				lbNativeColumn{"displayName()", "String", []any{"fake"}},
				lbNativeColumn{"version()", "String", []any{"26.8.7.19"}},
				lbNativeColumn{"revision()", "UInt32", []any{uint32(0)}},
				lbNativeColumn{"timezone()", "String", []any{"UTC"}}))
		case statement == "SELECT 1":
			_, _ = w.Write(lbNativeBlock(0, lbNativeColumn{"1", "UInt8", []any{uint8(1)}}))
		case strings.HasPrefix(statement, "SELECT address, chain, label_type, label_value"):
			mu.Lock()
			hits = append(hits, lbNativeHit{query: r.URL.Query(), statement: statement})
			mu.Unlock()
			cols := []lbNativeColumn{{name: "address", typ: "String"}, {name: "chain", typ: "String"},
				{name: "label_type", typ: "String"}, {name: "label_value", typ: "String"}}
			chains := lbList(lbChainListRe, statement)
			for _, a := range lbList(lbAddressListRe, statement) {
				for _, l := range labels[a] {
					if !slices.Contains(chains, l[0]) {
						continue
					}
					for i, v := range []string{a, l[0], l[1], l[2]} {
						cols[i].values = append(cols[i].values, v)
					}
				}
			}
			_, _ = w.Write(lbNativeBlock(0, cols...))
		default:
			w.WriteHeader(nethttp.StatusInternalServerError)
			_, _ = w.Write([]byte("Code: 62. DB::Exception: unexpected statement"))
		}
	}))
	t.Cleanup(server.Close)
	return server, func() []lbNativeHit {
		mu.Lock()
		defer mu.Unlock()
		return append([]lbNativeHit(nil), hits...)
	}
}

func TestBitqueryLabelsThroughClickhouseSource(t *testing.T) {
	f := newLBFixture(t, lbEVMBody([]string{lbEvmA, lbEvmB}, []string{lbEvmC}))
	server, hits := lbNativeServer(t, f.labels.labels)
	host, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	src, err := chsrc.Config{Name: "clickhouse-labels", Type: chsrc.SourceType, Host: host, Port: port,
		Database: "directory", User: "mcp", Protocol: "http"}.Initialize(f.ctx, otel.Tracer("test"))
	if err != nil {
		t.Fatalf("clickhouse source: %v", err)
	}
	t.Cleanup(func() { _ = src.(*chsrc.Source).Pool.Close() })
	f.srcs["clickhouse-labels"] = src

	h := nethttp.Header{}
	h.Set("X-Bitquery-Payer-Id", "42")
	h.Set("X-Bitquery-User-Id", "7")
	ctx := util.WithBitqueryIdentity(f.ctx, h)
	tl, err := lbToolConfig(lbEVMLabels()).Initialize(tools.WithSourceLookup(f.ctx, f.srcs))
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	row := lbRow(t, f.invokeCtx(t, ctx, tl.(Tool), map[string]any{"address": lbEvmSelf}))

	lbCheckEntry(t, "receiver 0", lbStrings("bsc/cex-hot-wallet:Binance", "ethereum/cex-hot-wallet:Binance", "ethereum/token-contract:X"),
		"bsc/cex-hot-wallet:Binance", "labeled")(lbObjValues(lbObj(row, "Top_Receivers", 0)))
	lbCheckEntry(t, "receiver 1", []any{}, nil, "none")(lbObjValues(lbObj(row, "Top_Receivers", 1)))
	lbCheckEntry(t, "sender 0", lbStrings("ethereum/dex-router:Uniswap"), "ethereum/dex-router:Uniswap", "labeled")(lbObjValues(lbObj(row, "Top_Senders", 0)))
	if got := row["Address_Labels"]; !reflect.DeepEqual(got, lbStrings("ethereum/cex-deposit-address:Kraken")) {
		t.Errorf("Address_Labels = %#v", got)
	}

	got := hits()
	if len(got) != 1 {
		t.Fatalf("label statements = %d, want 1", len(got))
	}
	qid := got[0].query.Get("query_id")
	if parts := strings.Split(qid, ":"); len(parts) != 7 || parts[3] != "7" || parts[4] != "42" {
		t.Fatalf("query_id = %q, want the caller's 7-part billing id", qid)
	}
	for _, want := range []string{"'" + lbEvmSelf + "', '" + lbEvmA + "'", "chain IN ('ethereum', 'bsc')", "read_overflow_mode = 'throw'"} {
		if !strings.Contains(got[0].statement, want) {
			t.Errorf("statement %q does not contain %q", got[0].statement, want)
		}
	}
}
