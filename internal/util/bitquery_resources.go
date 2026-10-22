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

// Bitquery-specific: tell the per-protocol initialize / server/discover handlers
// whether the server serves any resource (see internal/server/bitquery_resources.go).

package util

import "context"

const resourcesCapabilityKey contextKey = "resourcesCapability"

// WithResourcesCapability records whether the server advertises the `resources` capability.
func WithResourcesCapability(ctx context.Context, enabled bool) context.Context {
	return context.WithValue(ctx, resourcesCapabilityKey, enabled)
}

// ResourcesCapabilityFromContext reports whether the server advertises the `resources`
// capability. Absent means no: a server that serves no resource answers as it always did.
func ResourcesCapabilityFromContext(ctx context.Context) bool {
	enabled, _ := ctx.Value(resourcesCapabilityKey).(bool)
	return enabled
}
