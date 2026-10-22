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

package v20241105

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/googleapis/mcp-toolbox/internal/server/mcp/jsonrpc"
	mcputil "github.com/googleapis/mcp-toolbox/internal/server/mcp/util"
	"github.com/googleapis/mcp-toolbox/internal/server/primitives"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Bitquery: the resources methods. They serve the built-in resources the cfg
// enables (internal/bitqueryresources), the same on every endpoint; a server
// that serves none answers them as unknown methods.

/* Resources */

// Sent from the client to request a list of resources the server has.
type ListResourcesRequest struct {
	PaginatedRequest
}

// The server's response to a resources/list request from the client.
type ListResourcesResult struct {
	PaginatedResult
	Resources []Resource `json:"resources"`
}

// A known resource that the server is capable of reading.
type Resource struct {
	// The URI of this resource.
	URI string `json:"uri"`
	// A human-readable name for this resource.
	Name string `json:"name"`
	// A description of what this resource represents.
	Description string `json:"description,omitempty"`
	// The MIME type of this resource, if known.
	MimeType string `json:"mimeType,omitempty"`
	// Extension of the schema: This was not in the original schema for this version.
	Metadata map[string]any `json:"_meta,omitempty"`
}

// Sent from the client to request a list of resource templates the server has.
type ListResourceTemplatesRequest struct {
	PaginatedRequest
}

// The server's response to a resources/templates/list request from the client.
type ListResourceTemplatesResult struct {
	PaginatedResult
	ResourceTemplates []ResourceTemplate `json:"resourceTemplates"`
}

// A template description for resources available on the server.
type ResourceTemplate struct {
	// A URI template (according to RFC 6570) that can be used to construct resource URIs.
	URITemplate string `json:"uriTemplate"`
	// A human-readable name for the type of resource this template refers to.
	Name string `json:"name"`
	// A description of what this template is for.
	Description string `json:"description,omitempty"`
	// The MIME type for all resources that match this template.
	MimeType string `json:"mimeType,omitempty"`
}

// Sent from the client to the server, to read a specific resource URI.
type ReadResourceRequest struct {
	jsonrpc.Request
	Params struct {
		// The URI of the resource to read.
		URI string `json:"uri"`
	} `json:"params"`
}

// The server's response to a resources/read request from the client.
type ReadResourceResult struct {
	jsonrpc.Result
	Contents []TextResourceContents `json:"contents"`
}

// The contents of a text resource.
type TextResourceContents struct {
	// The URI of this resource.
	URI string `json:"uri"`
	// The MIME type of this resource, if known.
	MimeType string `json:"mimeType,omitempty"`
	// The text of the item.
	Text string `json:"text"`
	// Extension of the schema: This was not in the original schema for this version.
	Metadata map[string]any `json:"_meta,omitempty"`
}

// processResourcesMethod answers a resources method. It reports false for any
// other method, and for every method while the server serves no resource.
func processResourcesMethod(ctx context.Context, id jsonrpc.RequestId, method string, primitiveMgr *primitives.PrimitiveManager, body []byte) (any, bool, error) {
	if primitiveMgr == nil || !primitiveMgr.HasResources() {
		return nil, false, nil
	}
	var res any
	var err error
	switch method {
	case mcputil.RESOURCES_LIST:
		res, err = resourcesListHandler(id, primitiveMgr, body)
	case mcputil.RESOURCES_READ:
		res, err = resourcesReadHandler(ctx, id, primitiveMgr, body)
	case mcputil.RESOURCES_TEMPLATES_LIST:
		res, err = resourcesTemplatesListHandler(id, body)
	default:
		return nil, false, nil
	}
	return res, true, err
}

// GenerateListResourcesResult generates the resources/list result.
func GenerateListResourcesResult(pMgr *primitives.PrimitiveManager) ListResourcesResult {
	list := pMgr.ListResources()
	resources := make([]Resource, 0, len(list))
	for _, r := range list {
		resources = append(resources, Resource{
			URI:         r.URI,
			Name:        r.Name,
			Description: r.Description,
			MimeType:    r.MimeType,
			Metadata:    r.Meta(),
		})
	}
	return ListResourcesResult{Resources: resources}
}

// resourcesListHandler handles the "resources/list" method. Every resource
// fits in one page, so a cursor is ignored and none is returned.
func resourcesListHandler(id jsonrpc.RequestId, primitiveMgr *primitives.PrimitiveManager, body []byte) (any, error) {
	var req ListResourcesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		err = fmt.Errorf("invalid mcp resources list request: %w", err)
		return jsonrpc.NewError(id, jsonrpc.INVALID_REQUEST, err.Error(), nil), err
	}
	return jsonrpc.JSONRPCResponse{
		Jsonrpc: jsonrpc.JSONRPC_VERSION,
		Id:      id,
		Result:  GenerateListResourcesResult(primitiveMgr),
	}, nil
}

// resourcesTemplatesListHandler handles the "resources/templates/list"
// method. The server has no resource template.
func resourcesTemplatesListHandler(id jsonrpc.RequestId, body []byte) (any, error) {
	var req ListResourceTemplatesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		err = fmt.Errorf("invalid mcp resources templates list request: %w", err)
		return jsonrpc.NewError(id, jsonrpc.INVALID_REQUEST, err.Error(), nil), err
	}
	return jsonrpc.JSONRPCResponse{
		Jsonrpc: jsonrpc.JSONRPC_VERSION,
		Id:      id,
		Result:  ListResourceTemplatesResult{ResourceTemplates: []ResourceTemplate{}},
	}, nil
}

// resourcesReadHandler handles the "resources/read" method.
func resourcesReadHandler(ctx context.Context, id jsonrpc.RequestId, primitiveMgr *primitives.PrimitiveManager, body []byte) (any, error) {
	var req ReadResourceRequest
	if err := json.Unmarshal(body, &req); err != nil {
		err = fmt.Errorf("invalid mcp resources/read request: %w", err)
		return jsonrpc.NewError(id, jsonrpc.INVALID_REQUEST, err.Error(), nil), err
	}
	uri := req.Params.URI
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("mcp.resource.uri", uri))

	r, ok := primitiveMgr.GetResource(uri)
	if !ok {
		err := fmt.Errorf("resource not found: %q", uri)
		return jsonrpc.NewError(id, jsonrpc.RESOURCE_NOT_FOUND, "Resource not found", map[string]string{"uri": uri}), err
	}
	return jsonrpc.JSONRPCResponse{
		Jsonrpc: jsonrpc.JSONRPC_VERSION,
		Id:      id,
		Result: ReadResourceResult{
			Contents: []TextResourceContents{{
				URI:      r.URI,
				MimeType: r.MimeType,
				Text:     r.Text,
				Metadata: r.Meta(),
			}},
		},
	}, nil
}
