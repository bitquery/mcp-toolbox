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

package tools

// Bitquery: a tool result with two readers. Since MCP 2025-06-18 a tools/call
// result may carry `structuredContent` next to `content`; an MCP Apps view
// renders the structured object while the model reads the text (MCP Apps
// 2026-01-26, "Data Passing": `content` is the "text representation for model
// context and text-only hosts", `structuredContent` is "structured data
// optimized for UI rendering (not added to model context)").
//
// A tool's Invoke returns a StructuredResult (or a pointer to one) to send
// both. Every other result goes out exactly as before: a []any becomes one text
// item per element, anything else one text item.

// StructuredResult is a tools/call result with a structured object.
type StructuredResult struct {
	// Content becomes the text content items: one per element, each the JSON
	// encoding of that element, exactly as a []any result does. When it is
	// empty, the one text item is the JSON encoding of Structured, so a client
	// that reads only `content` still gets the data ("Tools MUST return
	// meaningful content array even when UI is available").
	Content []any `json:"content"`
	// Structured goes out as `structuredContent` in the protocol versions that
	// have it (2025-06-18 and later); the older ones drop it.
	Structured map[string]any `json:"structuredContent,omitempty"`
}

func (r StructuredResult) items() []any {
	if len(r.Content) == 0 && r.Structured != nil {
		return []any{r.Structured}
	}
	return r.Content
}

// TextResult is a result delivered as a single text content item verbatim, with
// no JSON encoding and no structured content. A []any or any other result has
// each element JSON-encoded into its text item, so a string element becomes a
// quoted JSON literal (`"line"`), and a multi-line one keeps its newlines as
// `\n`. A tool whose result is already prose for the model — a rendered
// document — returns a TextResult so the text reaches the model and a text-only
// host as written, not as an escaped JSON string. Only this type opts into the
// raw path; every other string element stays JSON-encoded as before.
type TextResult struct {
	Text string
}

// SplitResult returns the elements that become a tools/call result's text
// content items, and its structured object (nil for a plain result).
func SplitResult(result any) ([]any, map[string]any) {
	switch r := result.(type) {
	case StructuredResult:
		return r.items(), r.Structured
	case *StructuredResult:
		if r != nil {
			return r.items(), r.Structured
		}
	case TextResult:
		return []any{r}, nil
	case *TextResult:
		if r != nil {
			return []any{*r}, nil
		}
	case []any:
		return r, nil
	}
	return []any{result}, nil
}

// PlainResult returns a result as a caller that knows nothing of structured
// content receives it: a StructuredResult becomes its content elements, any
// other result is returned unchanged.
func PlainResult(result any) any {
	switch r := result.(type) {
	case StructuredResult:
		return r.items()
	case *StructuredResult:
		if r != nil {
			return r.items()
		}
	case TextResult:
		return r.Text
	case *TextResult:
		if r != nil {
			return r.Text
		}
	}
	return result
}
