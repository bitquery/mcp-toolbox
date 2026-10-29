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

// Bitquery-specific: a public MCP server card, so MCP directories can read the
// server's name, auth scheme and tool list without signing in.
//
// Two documents, one source. Both are built per request from the live
// registry — the default group's tools/list manifest at 2025-06-18 — so the
// card can never list a different tool set than /mcp serves.
//
//   - GET|HEAD /.well-known/mcp/server-card.json — the "Static Server Card"
//     Smithery reads (SEP-1649 shape): serverInfo, authentication, tools.
//   - GET|HEAD /mcp/server-card — the SEP-2127 card (Final, extension track):
//     identity and remotes, no tools by design. Served only when its identity
//     is configured, because a card missing a required field is invalid.
//
// The identity comes from the environment, not from a cfg key: a cfg key
// unknown to an older image makes it refuse the whole cfg.
//
// Both routes are registered on the root router, ahead of the /mcp mount, so
// no MCP middleware (auth, toolset lookup) runs on them; a static route wins
// over the mount's wildcard in chi.

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	mcputil "github.com/googleapis/mcp-toolbox/internal/server/mcp/util"
	v20250618 "github.com/googleapis/mcp-toolbox/internal/server/mcp/v20250618"
)

const (
	bitqueryServerCardPath        = "/.well-known/mcp/server-card.json"
	bitquerySEP2127CardPath       = "/mcp/server-card"
	bitquerySEP2127SchemaURL      = "https://static.modelcontextprotocol.io/schemas/v1/server-card.schema.json"
	bitquerySEP2127CardMediaType  = "application/mcp-server-card+json"
	bitqueryServerCardCacheHeader = "public, max-age=300"

	// Display name: the Smithery card's serverInfo.name, the SEP-2127 card's title.
	envServerCardName = "BITQUERY_SERVER_CARD_NAME"
	// Reverse-DNS identifier with exactly one "/" (SEP-2127 name), e.g. io.bitquery/mcp-server.
	envServerCardID = "BITQUERY_SERVER_CARD_ID"
	// One line, at most 100 characters (SEP-2127).
	envServerCardDescription = "BITQUERY_SERVER_CARD_DESCRIPTION"
	// The public streamable-HTTP URL clients connect to (SEP-2127 remotes[0].url).
	envServerCardURL = "BITQUERY_SERVER_CARD_URL"
	// The release that serves this card; defaults to the binary's version.
	envServerCardVersion = "BITQUERY_SERVER_CARD_VERSION"
	// Optional SEP-2127 websiteUrl.
	envServerCardWebsiteURL = "BITQUERY_SERVER_CARD_WEBSITE_URL"
	// Comma-separated auth schemes for the Smithery card, e.g. "oauth2". Empty:
	// "oauth2" when the toolbox's own MCP auth is on, else no authentication block.
	// Set it when an edge in front of the toolbox does the authentication.
	envServerCardAuthSchemes = "BITQUERY_SERVER_CARD_AUTH_SCHEMES"
)

// SEP-2127: reverse-DNS, exactly one "/", 3-200 characters.
var bitquerySEP2127NameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

type bitqueryServerCardIdentity struct {
	Name        string
	ID          string
	Description string
	URL         string
	Version     string
	WebsiteURL  string
	AuthSchemes []string
}

func bitqueryServerCardIdentityFromEnv(defaultVersion string, mcpAuthEnabled bool) bitqueryServerCardIdentity {
	get := func(k string) string { return strings.TrimSpace(os.Getenv(k)) }
	id := bitqueryServerCardIdentity{
		Name:        get(envServerCardName),
		ID:          get(envServerCardID),
		Description: get(envServerCardDescription),
		URL:         get(envServerCardURL),
		Version:     get(envServerCardVersion),
		WebsiteURL:  get(envServerCardWebsiteURL),
	}
	if id.Name == "" {
		id.Name = v20250618.SERVER_NAME
	}
	if id.Version == "" {
		id.Version = defaultVersion
	}
	for _, s := range strings.Split(get(envServerCardAuthSchemes), ",") {
		if s = strings.TrimSpace(s); s != "" {
			id.AuthSchemes = append(id.AuthSchemes, s)
		}
	}
	if len(id.AuthSchemes) == 0 && mcpAuthEnabled {
		id.AuthSchemes = []string{"oauth2"}
	}
	return id
}

// sep2127Problem says why the SEP-2127 card cannot be served, or "" when it can.
func (id bitqueryServerCardIdentity) sep2127Problem() string {
	switch {
	case id.ID == "" || id.Description == "" || id.URL == "":
		return fmt.Sprintf("%s, %s and %s must all be set", envServerCardID, envServerCardDescription, envServerCardURL)
	case len(id.ID) < 3 || len(id.ID) > 200 || !bitquerySEP2127NameRe.MatchString(id.ID):
		return fmt.Sprintf("%s must be reverse-DNS with exactly one '/', 3-200 characters", envServerCardID)
	case len([]rune(id.Description)) > 100:
		return fmt.Sprintf("%s is longer than 100 characters", envServerCardDescription)
	case !strings.HasPrefix(id.URL, "https://") && !strings.HasPrefix(id.URL, "http://"):
		return fmt.Sprintf("%s must be an http(s) URL", envServerCardURL)
	case len(id.Name) > 100:
		return fmt.Sprintf("%s is longer than 100 characters (the SEP-2127 title)", envServerCardName)
	}
	return ""
}

type bitqueryCardServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type bitqueryCardAuthentication struct {
	Required bool     `json:"required"`
	Schemes  []string `json:"schemes"`
}

// bitqueryCardTool is the tools/list object without `_meta`: whatever a tool
// carries there is for a connected client, not for a public index.
type bitqueryCardTool struct {
	Name        string                     `json:"name"`
	Title       string                     `json:"title,omitempty"`
	Description string                     `json:"description,omitempty"`
	InputSchema v20250618.InputSchema      `json:"inputSchema"`
	Annotations *v20250618.ToolAnnotations `json:"annotations,omitempty"`
}

type bitqueryStaticCard struct {
	ServerInfo     bitqueryCardServerInfo      `json:"serverInfo"`
	Authentication *bitqueryCardAuthentication `json:"authentication,omitempty"`
	Tools          []bitqueryCardTool          `json:"tools"`
}

type bitquerySEP2127Remote struct {
	Type                      string   `json:"type"`
	URL                       string   `json:"url"`
	SupportedProtocolVersions []string `json:"supportedProtocolVersions,omitempty"`
}

type bitquerySEP2127Card struct {
	Schema      string                  `json:"$schema"`
	Name        string                  `json:"name"`
	Version     string                  `json:"version"`
	Description string                  `json:"description"`
	Title       string                  `json:"title,omitempty"`
	WebsiteURL  string                  `json:"websiteUrl,omitempty"`
	Remotes     []bitquerySEP2127Remote `json:"remotes,omitempty"`
}

func (s *Server) bitqueryStaticServerCard(id bitqueryServerCardIdentity) ([]byte, error) {
	g, ok := s.PrimitiveMgr.GetGroup("")
	if !ok {
		return nil, fmt.Errorf("default toolset not found")
	}
	list, err := v20250618.GenerateListToolsResult(s.PrimitiveMgr, g, nil)
	if err != nil {
		return nil, err
	}
	card := bitqueryStaticCard{
		ServerInfo: bitqueryCardServerInfo{Name: id.Name, Version: id.Version},
		Tools:      make([]bitqueryCardTool, 0, len(list.Tools)),
	}
	if len(id.AuthSchemes) > 0 {
		card.Authentication = &bitqueryCardAuthentication{Required: true, Schemes: id.AuthSchemes}
	}
	for _, t := range list.Tools {
		card.Tools = append(card.Tools, bitqueryCardTool{
			Name:        t.Name,
			Title:       t.Title,
			Description: t.Description,
			InputSchema: t.ToolInputSchema,
			Annotations: t.Annotations,
		})
	}
	return json.Marshal(card)
}

func (s *Server) bitquerySEP2127ServerCard(id bitqueryServerCardIdentity) ([]byte, error) {
	card := bitquerySEP2127Card{
		Schema:      bitquerySEP2127SchemaURL,
		Name:        id.ID,
		Version:     id.Version,
		Description: id.Description,
		Title:       id.Name,
		WebsiteURL:  id.WebsiteURL,
		Remotes: []bitquerySEP2127Remote{{
			Type:                      "streamable-http",
			URL:                       id.URL,
			SupportedProtocolVersions: mcputil.GetSupportedVersions(s.enableDraftSpecs),
		}},
	}
	return json.Marshal(card)
}

// writeBitqueryCard answers GET and HEAD with the public, cacheable headers the
// SEP-2127 discovery document asks for, and 304 on a matching If-None-Match.
func writeBitqueryCard(w http.ResponseWriter, r *http.Request, body []byte, contentType string) {
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, HEAD")
	h.Set("Access-Control-Allow-Headers", "Content-Type, If-None-Match")
	h.Set("Access-Control-Expose-Headers", "ETag")
	h.Set("Cache-Control", bitqueryServerCardCacheHeader)
	h.Set("ETag", etag)
	for _, v := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		if v = strings.TrimSpace(v); v == etag || v == "*" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// registerBitqueryServerCard adds the card routes to the root router. Call it
// before /mcp is mounted.
func registerBitqueryServerCard(ctx context.Context, r chi.Router, s *Server, mcpAuthEnabled bool) {
	id := bitqueryServerCardIdentityFromEnv(s.version, mcpAuthEnabled)

	static := func(w http.ResponseWriter, req *http.Request) {
		body, err := s.bitqueryStaticServerCard(id)
		if err != nil {
			s.logger.ErrorContext(req.Context(), "unable to build the server card", "error", err)
			http.Error(w, "server card unavailable", http.StatusInternalServerError)
			return
		}
		writeBitqueryCard(w, req, body, "application/json")
	}
	r.Get(bitqueryServerCardPath, static)
	r.Head(bitqueryServerCardPath, static)

	if problem := id.sep2127Problem(); problem != "" {
		s.logger.DebugContext(ctx, "SEP-2127 server card not served: "+problem)
		return
	}
	sep := func(w http.ResponseWriter, req *http.Request) {
		body, err := s.bitquerySEP2127ServerCard(id)
		if err != nil {
			s.logger.ErrorContext(req.Context(), "unable to build the SEP-2127 server card", "error", err)
			http.Error(w, "server card unavailable", http.StatusInternalServerError)
			return
		}
		writeBitqueryCard(w, req, body, bitquerySEP2127CardMediaType)
	}
	r.Get(bitquerySEP2127CardPath, sep)
	r.Head(bitquerySEP2127CardPath, sep)
}
