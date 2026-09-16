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

package server_test

// Bitquery: the server hands its sources to tool initialization, so an http tool
// with bitquerySourceRoutes resolves every route target at startup — and a
// config naming a missing source fails to load instead of failing on a call.

import (
	"strings"
	"testing"

	"github.com/googleapis/mcp-toolbox/internal/server"
	httpsrc "github.com/googleapis/mcp-toolbox/internal/sources/http"
	"github.com/googleapis/mcp-toolbox/internal/telemetry"
	"github.com/googleapis/mcp-toolbox/internal/testutils"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	httptool "github.com/googleapis/mcp-toolbox/internal/tools/http"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

func bitqueryRoutedServerConfig(targets map[string]string) server.ServerConfig {
	src := func(name string) httpsrc.Config {
		return httpsrc.Config{Name: name, Type: httpsrc.SourceType, BaseURL: "http://127.0.0.1:1", Timeout: "1s", AllowPrivateNetworks: true}
	}
	return server.ServerConfig{
		Version: "0.0.0",
		SourceConfigs: server.SourceConfigs{
			"gw-eth":     src("gw-eth"),
			"legacy-btc": src("legacy-btc"),
		},
		ToolConfigs: server.ToolConfigs{
			"chain_list_tables": httptool.Config{
				ConfigBase:   tools.ConfigBase{Name: "chain_list_tables", Description: "lists tables"},
				Type:         "http",
				Source:       "gw-eth",
				Path:         "/",
				Method:       "POST",
				RequestBody:  "SELECT '{{.database}}'",
				BodyParams:   parameters.Parameters{parameters.NewStringParameter("database", "database")},
				SourceRoutes: &httptool.SourceRoutes{Param: "database", Sources: targets},
			},
		},
	}
}

func TestBitquerySourceRoutesResolvedAtStartup(t *testing.T) {
	ctx, err := testutils.ContextWithNewLogger()
	if err != nil {
		t.Fatalf("logger: %s", err)
	}
	instrumentation, err := telemetry.CreateTelemetryInstrumentation("0.0.0")
	if err != nil {
		t.Fatalf("instrumentation: %s", err)
	}
	ctx = util.WithInstrumentation(ctx, instrumentation)

	good := bitqueryRoutedServerConfig(map[string]string{"eth_api": "gw-eth", "bitcoin": "legacy-btc"})
	if _, _, _, toolsMap, _, _, err := server.InitializeConfigs(ctx, good); err != nil {
		t.Fatalf("a config whose route targets exist must load: %s", err)
	} else if _, ok := toolsMap["chain_list_tables"]; !ok {
		t.Fatalf("routed tool missing after initialization")
	}

	bad := bitqueryRoutedServerConfig(map[string]string{"eth_api": "gw-eth", "bitcoin": "ch-btc-typo"})
	_, _, _, _, _, _, err = server.InitializeConfigs(ctx, bad)
	if err == nil || !strings.Contains(err.Error(), "unable to retrieve source ch-btc-typo") {
		t.Fatalf("a route naming a missing source must fail at startup, got %v", err)
	}

	// Offline flows have no sources at all; the tool metadata must still load.
	if _, _, err := server.InitializeOfflineConfigs(ctx, bad); err != nil {
		t.Fatalf("offline initialization must not resolve routes: %s", err)
	}
}
