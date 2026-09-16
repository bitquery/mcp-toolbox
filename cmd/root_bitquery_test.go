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

package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/googleapis/mcp-toolbox/cmd/internal"
	"github.com/googleapis/mcp-toolbox/internal/log"
	"github.com/googleapis/mcp-toolbox/internal/server"
	"github.com/googleapis/mcp-toolbox/internal/telemetry"
	"github.com/googleapis/mcp-toolbox/internal/util"
)

func TestServerInstructionsFileFlag(t *testing.T) {
	_, opts, _, err := invokeCommand([]string{"--server-instructions-file", "cfg/_instructions.md"})
	if err != nil {
		t.Fatalf("unexpected error invoking command: %s", err)
	}
	if got := opts.Cfg.ServerInstructionsFile; got != "cfg/_instructions.md" {
		t.Errorf("ServerInstructionsFile = %q, want %q", got, "cfg/_instructions.md")
	}

	_, opts, _, err = invokeCommand([]string{})
	if err != nil {
		t.Fatalf("unexpected error invoking command: %s", err)
	}
	if got := opts.Cfg.ServerInstructionsFile; got != "" {
		t.Errorf("default ServerInstructionsFile = %q, want empty", got)
	}

	root := NewCommand(internal.NewToolboxOptions())
	serveCmd, _, err := root.Find([]string{"serve"})
	if err != nil {
		t.Fatalf("serve subcommand not found: %s", err)
	}
	if serveCmd.Flags().Lookup("server-instructions-file") == nil {
		t.Error("serve subcommand does not accept --server-instructions-file")
	}
}

// TestWatchChangesReloadsServerInstructions checks that an edit to the instructions file in the
// watched config folder reaches the running server without a YAML change.
func TestWatchChangesReloadsServerInstructions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	logger, err := log.NewStdLogger(io.Discard, io.Discard, "DEBUG")
	if err != nil {
		t.Fatalf("failed to setup logger %s", err)
	}
	ctx = util.WithLogger(ctx, logger)
	instrumentation, err := telemetry.CreateTelemetryInstrumentation(versionString)
	if err != nil {
		t.Fatalf("failed to setup instrumentation %s", err)
	}
	ctx = util.WithInstrumentation(ctx, instrumentation)

	dir := t.TempDir()
	instructionsFile := filepath.Join(dir, "_instructions.md")
	if err := os.WriteFile(instructionsFile, []byte("first\n"), 0o644); err != nil {
		t.Fatalf("unable to write instructions: %s", err)
	}
	s, err := server.NewServer(ctx, server.ServerConfig{
		Version:                versionString,
		Address:                "127.0.0.1",
		AllowedHosts:           []string{"*"},
		ServerInstructionsFile: instructionsFile,
	})
	if err != nil {
		t.Fatalf("unable to initialize server: %s", err)
	}
	if got := s.ServerInstructions(); got != "first" {
		t.Fatalf("instructions at start %q, want %q", got, "first")
	}

	// folder mode: no individual files, one watched directory
	go watchChanges(ctx, map[string]bool{dir: true}, map[string]bool{}, s, 0)

	// The watcher starts asynchronously; keep rewriting until the edit is picked up.
	for i := 0; ; i++ {
		want := fmt.Sprintf("second %d", i)
		if err := os.WriteFile(instructionsFile, []byte(want+"\n"), 0o644); err != nil {
			t.Fatalf("unable to write instructions: %s", err)
		}
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if s.ServerInstructions() == want {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		if ctx.Err() != nil {
			t.Fatalf("instructions were not reloaded; still %q", s.ServerInstructions())
		}
	}
}
