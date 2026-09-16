// Copyright 2025 Google LLC
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

package clickhouse

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"time"

	clickhouse "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/goccy/go-yaml"
	"github.com/googleapis/mcp-toolbox/internal/bitquerylabels"
	"github.com/googleapis/mcp-toolbox/internal/sources"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
	"go.opentelemetry.io/otel/trace"
)

const SourceType string = "clickhouse"

// validate interface
var _ sources.SourceConfig = Config{}

func init() {
	if !sources.Register(SourceType, newConfig) {
		panic(fmt.Sprintf("source type %q already registered", SourceType))
	}
}

func newConfig(ctx context.Context, name string, decoder *yaml.Decoder) (sources.SourceConfig, error) {
	actual := Config{Name: name}
	if err := decoder.DecodeContext(ctx, &actual); err != nil {
		return nil, err
	}
	return actual, nil
}

type Config struct {
	Name     string `yaml:"name" validate:"required"`
	Type     string `yaml:"type" validate:"required"`
	Host     string `yaml:"host" validate:"required"`
	Port     string `yaml:"port" validate:"required"`
	Database string `yaml:"database" validate:"required"`
	User     string `yaml:"user" validate:"required"`
	Password string `yaml:"password"`
	Protocol string `yaml:"protocol"`
	Secure   bool   `yaml:"secure"`
	// Bitquery: when set (and naming a service or host), every clickhouse-sql tool
	// on this source first asks the labels-query-service to refresh the addresses
	// in its parameters, then runs its SQL. See internal/bitquerylabels.
	LabelsQueryService *bitquerylabels.Config `yaml:"bitqueryLabelsQueryService,omitempty"`
}

func (r Config) SourceConfigType() string {
	return SourceType
}

func (r Config) Initialize(ctx context.Context, tracer trace.Tracer) (sources.Source, error) {
	// Bitquery: validates the labels-query-service block without network I/O, so an
	// unreachable labels service never fails startup.
	labelsPreprocessor, err := bitquerylabels.New(ctx, r.Name, r.LabelsQueryService)
	if err != nil {
		return nil, err
	}

	pool, err := initClickHouseConnectionPool(ctx, tracer, r.Name, r.Host, r.Port, r.User, r.Password, r.Database, r.Protocol, r.Secure)
	if err != nil {
		return nil, fmt.Errorf("unable to create pool: %w", err)
	}

	err = pool.PingContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to connect successfully: %w", err)
	}

	s := &Source{
		Config:             r,
		Pool:               pool,
		labelsPreprocessor: labelsPreprocessor,
	}
	return s, nil
}

var _ sources.Source = &Source{}
var _ bitquerylabels.HookSource = &Source{}

type Source struct {
	Config
	Pool               *sql.DB
	labelsPreprocessor *bitquerylabels.Preprocessor
}

// BitqueryLabelsPreprocessor returns the labels pre-process hook, or nil when
// the source has no labels-query-service configured.
func (s *Source) BitqueryLabelsPreprocessor() *bitquerylabels.Preprocessor {
	return s.labelsPreprocessor
}

func (s *Source) SourceType() string {
	return SourceType
}

func (s *Source) ToConfig() sources.SourceConfig {
	return s.Config
}

func (s *Source) ClickHousePool() *sql.DB {
	return s.Pool
}

func (s *Source) RunSQL(ctx context.Context, statement string, params parameters.ParamValues) (any, error) {
	var sliceParams []any
	if params != nil {
		sliceParams = params.AsSlice()
	}

	// Bitquery: stamp a billing-attributable query_id (from the caller identity in
	// ctx) so the ClickHouse query_log -> api_v2 billing pipeline attributes cost per
	// payer. No-op when no identity is present (stdio / unauthenticated calls).
	if qid, ok := util.BitqueryClickhouseQueryID(ctx); ok {
		ctx = clickhouse.Context(ctx, clickhouse.WithQueryID(qid))
	}

	results, err := s.ClickHousePool().QueryContext(ctx, statement, sliceParams...)
	if err != nil {
		// Bitquery: over the HTTP protocol a connect/timeout failure prints the
		// request URL (internal host, port, database, query_id); log it and return an
		// address-free message. A ClickHouse error body loses the server version,
		// replica host and expression context (see util.BitqueryCleanDatabaseMessage).
		return nil, fmt.Errorf("unable to execute query: %w", util.BitqueryDatabaseFailure(ctx, s.Name, util.BitqueryTransportFailure(ctx, s.Name, err, 0)))
	}
	defer results.Close()

	cols, err := results.Columns()
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve rows column name: %w", err)
	}

	// create an array of values for each column, which can be re-used to scan each row
	rawValues := make([]any, len(cols))
	values := make([]any, len(cols))
	for i := range rawValues {
		values[i] = &rawValues[i]
	}

	colTypes, err := results.ColumnTypes()
	if err != nil {
		return nil, fmt.Errorf("unable to get column types: %w", err)
	}

	out := []any{}
	for results.Next() {
		err := results.Scan(values...)
		if err != nil {
			return nil, fmt.Errorf("unable to parse row: %w", err)
		}
		vMap := make(map[string]any)
		for i, name := range cols {
			// ClickHouse driver may return specific types that need handling
			switch colTypes[i].DatabaseTypeName() {
			case "String", "FixedString":
				if rawValues[i] != nil {
					// Handle potential []byte to string conversion if needed
					if b, ok := rawValues[i].([]byte); ok {
						vMap[name] = string(b)
					} else {
						vMap[name] = rawValues[i]
					}
				} else {
					vMap[name] = nil
				}
			default:
				vMap[name] = rawValues[i]
			}
		}
		out = append(out, vMap)
	}

	if err := results.Err(); err != nil {
		// Bitquery: a connection lost mid-stream names the peer IPs; an exception
		// sent mid-stream is a ClickHouse error like any other.
		return nil, fmt.Errorf("errors encountered by results.Scan: %w", util.BitqueryDatabaseFailure(ctx, s.Name, util.BitqueryTransportFailure(ctx, s.Name, err, 0)))
	}

	return out, nil
}

func validateConfig(protocol string) error {
	validProtocols := map[string]bool{"http": true, "https": true}

	if protocol != "" && !validProtocols[protocol] {
		return fmt.Errorf("invalid protocol: %s, must be one of: http, https", protocol)
	}
	return nil
}

func initClickHouseConnectionPool(ctx context.Context, tracer trace.Tracer, name, host, port, user, pass, dbname, protocol string, secure bool) (*sql.DB, error) {
	//nolint:all // Reassigned ctx
	ctx, span := sources.InitConnectionSpan(ctx, tracer, SourceType, name)
	defer span.End()

	if protocol == "" {
		protocol = "https"
	}

	if err := validateConfig(protocol); err != nil {
		return nil, err
	}

	encodedUser := url.QueryEscape(user)
	encodedPass := url.QueryEscape(pass)

	var dsn string
	scheme := protocol
	if protocol == "http" && secure {
		scheme = "https"
	}
	dsn = fmt.Sprintf("%s://%s:%s@%s:%s/%s", scheme, encodedUser, encodedPass, host, port, dbname)
	if scheme == "https" {
		dsn += "?secure=true&skip_verify=false"
	}

	pool, err := sql.Open("clickhouse", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}

	pool.SetMaxOpenConns(25)
	pool.SetMaxIdleConns(5)
	pool.SetConnMaxLifetime(5 * time.Minute)

	return pool, nil
}
