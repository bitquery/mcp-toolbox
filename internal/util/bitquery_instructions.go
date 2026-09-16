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

// Bitquery-specific: carry the MCP server instructions to the per-protocol initialize
// handlers (see internal/server/bitquery_instructions.go).

package util

import "context"

const serverInstructionsKey contextKey = "serverInstructions"

// WithServerInstructions adds the MCP server instructions into the context.
func WithServerInstructions(ctx context.Context, instructions string) context.Context {
	return context.WithValue(ctx, serverInstructionsKey, instructions)
}

// ServerInstructionsFromContext returns the MCP server instructions, or "" when none are set.
// Absence is not an error: an empty value means the field is omitted from the result.
func ServerInstructionsFromContext(ctx context.Context) string {
	if s, ok := ctx.Value(serverInstructionsKey).(string); ok {
		return s
	}
	return ""
}
