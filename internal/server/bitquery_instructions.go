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

// Bitquery-specific: MCP server-level instructions.
//
// Clients put the `instructions` string of the initialize result (server/discover on the
// draft protocol) into the model's context before the first call, so it is the one place to
// tell the model how the tools fit together. The text comes from a file named by
// --server-instructions-file or, when that flag is empty, by the
// TOOLBOX_SERVER_INSTRUCTIONS_FILE environment variable. Deployments should prefer the
// environment variable: a binary that predates this feature ignores an unknown variable but
// refuses to start on an unknown flag.
//
// The same text is served on every endpoint (/mcp and /mcp/<toolset>, HTTP and stdio). It is
// read at startup (an unreadable, empty or oversized file fails the start) and re-read on
// every config reload; a reload that fails validation keeps the previous text.

package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"unicode"
	"unicode/utf8"
)

const (
	// ServerInstructionsFileEnv names the environment variable read when
	// --server-instructions-file is not set.
	ServerInstructionsFileEnv = "TOOLBOX_SERVER_INSTRUCTIONS_FILE"
	// MaxServerInstructionsBytes caps the instructions text (after trailing whitespace is
	// trimmed). Clients add it to the context of every conversation.
	MaxServerInstructionsBytes = 8 << 10
	// maxServerInstructionsReadBytes bounds how much of a wrongly configured path is read.
	maxServerInstructionsReadBytes = 1 << 20
)

// serverInstructions holds the resolved file path and the loaded text. The text is swapped
// atomically on reload while requests read it.
type serverInstructions struct {
	path string
	text atomic.Pointer[string]
}

// ResolveServerInstructionsFile returns the flag value when it is set, otherwise the value of
// the ServerInstructionsFileEnv environment variable. An empty result means not configured.
func ResolveServerInstructionsFile(flagValue string, getenv func(string) string) string {
	if flagValue != "" {
		return flagValue
	}
	if getenv == nil {
		return ""
	}
	return getenv(ServerInstructionsFileEnv)
}

// LoadServerInstructions reads the instructions file at path. An empty path returns "" and no
// error (instructions are omitted). Validation order: read, normalize CRLF to LF, trim
// trailing whitespace, then reject an empty text, a text over MaxServerInstructionsBytes and
// invalid UTF-8.
func LoadServerInstructions(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("unable to read server instructions file %q: %w", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxServerInstructionsReadBytes+1))
	if err != nil {
		return "", fmt.Errorf("unable to read server instructions file %q: %w", path, err)
	}
	if len(data) > maxServerInstructionsReadBytes {
		return "", fmt.Errorf("server instructions file %q is larger than %d bytes; the limit is %d bytes", path, maxServerInstructionsReadBytes, MaxServerInstructionsBytes)
	}
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	data = bytes.TrimRightFunc(data, unicode.IsSpace)
	if len(data) == 0 {
		return "", fmt.Errorf("server instructions file %q is empty", path)
	}
	if len(data) > MaxServerInstructionsBytes {
		return "", fmt.Errorf("server instructions file %q is %d bytes; the limit is %d bytes", path, len(data), MaxServerInstructionsBytes)
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("server instructions file %q is not valid UTF-8", path)
	}
	return string(data), nil
}

// loadConfiguredServerInstructions resolves the path from the config (flag) or the
// environment and loads the text.
func loadConfiguredServerInstructions(cfg ServerConfig) (string, string, error) {
	path := ResolveServerInstructionsFile(cfg.ServerInstructionsFile, os.Getenv)
	text, err := LoadServerInstructions(path)
	if err != nil {
		return "", "", err
	}
	return path, text, nil
}

// setServerInstructions stores the loaded instructions on the server.
func (s *Server) setServerInstructions(ctx context.Context, path, text string) {
	s.instructions.path = path
	if path == "" {
		return
	}
	s.instructions.text.Store(&text)
	s.logger.InfoContext(ctx, fmt.Sprintf("Loaded MCP server instructions from %q (%d bytes)", path, len(text)))
}

// ServerInstructions returns the text sent as `instructions`, or "" when none is configured.
func (s *Server) ServerInstructions() string {
	if p := s.instructions.text.Load(); p != nil {
		return *p
	}
	return ""
}

// ServerInstructionsFile returns the configured instructions file path, or "".
func (s *Server) ServerInstructionsFile() string {
	return s.instructions.path
}

// IsServerInstructionsFile reports whether name refers to the configured instructions file.
func (s *Server) IsServerInstructionsFile(name string) bool {
	if s.instructions.path == "" || name == "" {
		return false
	}
	return absClean(name) == absClean(s.instructions.path)
}

// ReloadServerInstructions re-reads the configured instructions file. On error the previous
// text stays in effect and a warning is logged. Without a configured file it does nothing.
func (s *Server) ReloadServerInstructions(ctx context.Context) error {
	path := s.instructions.path
	if path == "" {
		return nil
	}
	text, err := LoadServerInstructions(path)
	if err != nil {
		s.logger.WarnContext(ctx, fmt.Sprintf("keeping the previous MCP server instructions: %s", err))
		return err
	}
	if old := s.instructions.text.Swap(&text); old == nil || *old != text {
		s.logger.InfoContext(ctx, fmt.Sprintf("Reloaded MCP server instructions from %q (%d bytes)", path, len(text)))
	}
	return nil
}

func absClean(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
}
