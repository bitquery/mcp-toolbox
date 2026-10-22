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

package http_test

// Bitquery: bitqueryLabels goes through the server's strict decoder, where an
// unknown or misspelt key refuses the whole configuration.

import (
	"reflect"
	"strings"
	"testing"

	"github.com/googleapis/mcp-toolbox/internal/server"
	"github.com/googleapis/mcp-toolbox/internal/testutils"
	http "github.com/googleapis/mcp-toolbox/internal/tools/http"
)

const bitqueryLabelsToolYAML = `kind: tool
name: btc_address_flow_summary
type: http
source: ch-btc1-http
method: POST
path: /
description: flow summary
requestBody: SELECT 1
bodyParams:
  - name: address
    type: string
    description: address
bitqueryLabels:
  source: clickhouse-labels
  chains: [bitcoin]
  fields:
    - column: Top_Receivers
      namesColumn: Top_Fields
      key: counterparty
    - column: Top_Senders
      namesColumn: Top_Fields
      key: counterparty
  addressParam: address
  addressColumn: Address_Labels
  priority: [sanctioned-entity, cex-hot-wallet]
  outputNames:
    label: label_pick
  batch: 500
  timeoutSec: 3
  maxRowsToRead: 1000000
`

func TestBitqueryLabelsYAMLAccepted(t *testing.T) {
	ctx, err := testutils.ContextWithNewLogger()
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, toolCfgs, _, _, err := server.UnmarshalPrimitiveConfig(ctx, []byte(bitqueryLabelsToolYAML))
	if err != nil {
		t.Fatalf("unable to unmarshal: %s", err)
	}
	cfg, ok := toolCfgs["btc_address_flow_summary"].(http.Config)
	if !ok {
		t.Fatalf("tool not decoded: %#v", toolCfgs)
	}
	want := &http.LabelsConfig{
		Source: "clickhouse-labels",
		Chains: []string{"bitcoin"},
		Fields: []http.LabelsField{
			{Column: "Top_Receivers", NamesColumn: "Top_Fields", Key: "counterparty"},
			{Column: "Top_Senders", NamesColumn: "Top_Fields", Key: "counterparty"},
		},
		AddressParam:  "address",
		AddressColumn: "Address_Labels",
		Priority:      []string{"sanctioned-entity", "cex-hot-wallet"},
		OutputNames:   &http.LabelsOutputNames{Label: "label_pick"},
		Batch:         500,
		TimeoutSec:    3,
		MaxRowsToRead: 1000000,
	}
	if !reflect.DeepEqual(cfg.Labels, want) {
		t.Fatalf("bitqueryLabels = %#v, want %#v", cfg.Labels, want)
	}
	// Offline initialization (no sources) loads the tool.
	if _, err := cfg.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
}

func TestBitqueryLabelsYAMLRefusesUnknownKeys(t *testing.T) {
	ctx, err := testutils.ContextWithNewLogger()
	if err != nil {
		t.Fatal(err)
	}
	for _, edit := range [][2]string{
		{"  timeoutSec: 3\n", "  timeoutSecs: 3\n"},
		{"      key: counterparty\n", "      keys: counterparty\n"},
		{"    label: label_pick\n", "    pick: label_pick\n"},
		{"bitqueryLabels:\n", "bitqueryLabel:\n"},
	} {
		bad := strings.Replace(bitqueryLabelsToolYAML, edit[0], edit[1], 1)
		if bad == bitqueryLabelsToolYAML {
			t.Fatalf("the fixture no longer holds %q", edit[0])
		}
		if _, _, _, _, _, _, err := server.UnmarshalPrimitiveConfig(ctx, []byte(bad)); err == nil {
			t.Errorf("%q was accepted", strings.TrimSpace(edit[1]))
		}
	}
}
