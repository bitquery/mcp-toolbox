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

package http

// Bitquery: bitqueryLabels adds address labels to the result of an http tool.
// The statement of an http tool reads only its own source, and the labels live
// in another database (directory.labels on a clickhouse source), so the server
// looks them up after the data has come back:
//
//	bitqueryLabels:
//	  source: clickhouse-labels       # a clickhouse source holding directory.labels
//	  chains: [ethereum, bsc, polygon] # directory chain names a label is read from
//	  fields:
//	    - column: Top_Senders          # an array of objects
//	      key: counterparty            # the object key holding the address
//	    - column: Top_Receivers        # an array of positional arrays
//	      namesColumn: Top_Fields      # the sibling column naming the positions
//	      key: counterparty            # the name of the position holding the address
//	  addressParam: address            # optional: the tool's own address parameter
//	  addressColumn: Address_Labels    # the top-level column its labels go to
//	  priority: [sanctioned-entity, cex-hot-wallet]
//
// Each entry gets three values: labels (every label of the address as
// chain/label_type:label_value, sorted; null when nothing was looked up),
// label (one of them, picked by the priority list of label types; null when
// there is none) and label_status (labeled, none, no_coverage or
// unavailable). An object entry gets them as keys; a positional entry gets
// them appended at its end, and the names column of its row gets the three
// names appended, so no existing position moves. A name the entry or the
// names column already carries is never overwritten and never written twice:
// only the absent names are added (outputNames renames them). addressColumn
// holds the labels of the address parameter, in the same form.
//
// An address is looked up only when it is a run of ASCII letters and digits
// (a 0x address lowercased first, any other as given); anything else, such as
// a script placeholder, reads none without being sent.
//
// One call runs at most one statement per batch of addresses: the distinct
// addresses of the whole result go in as a literal list, never a subquery,
// each statement carries its own row and time limits, and the clickhouse
// source stamps it with the caller's billing query_id. Labels are read as
// stored; nothing is refreshed here.
//
// The step fails open. When a label statement fails or runs out of time, the
// data comes back unchanged apart from label_status "unavailable" on each
// entry, and the failure is logged.
//
// bitqueryLabels is a fork-only key: an image that does not know it refuses
// every configuration that uses it, so the image ships before the
// configuration.

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

// LabelsConfig is the YAML form of bitqueryLabels.
type LabelsConfig struct {
	// Source is the name of the clickhouse source that holds directory.labels.
	Source string `yaml:"source"`
	// Chains are the directory chain names the labels are read from.
	Chains []string `yaml:"chains"`
	// NoCoverage marks a chain the directory holds no labels for: no statement
	// is run, and every entry reads no_coverage.
	NoCoverage bool `yaml:"noCoverage"`
	// Fields are the result columns whose entries get labels.
	Fields []LabelsField `yaml:"fields"`
	// AddressParam names a string parameter of the tool; the labels of its value
	// go to the top-level column AddressColumn of every row.
	AddressParam  string `yaml:"addressParam"`
	AddressColumn string `yaml:"addressColumn"`
	// Priority lists label types, first preferred, for the label pick.
	Priority []string `yaml:"priority"`
	// OutputNames renames the three values written to each entry.
	OutputNames *LabelsOutputNames `yaml:"outputNames,omitempty"`
	// Batch is the most addresses one statement looks up (default 2000).
	Batch int `yaml:"batch"`
	// TimeoutSec bounds the whole lookup of a call (default 5).
	TimeoutSec int `yaml:"timeoutSec"`
	// MaxRowsToRead is each statement's max_rows_to_read (default 50,000,000).
	MaxRowsToRead int64 `yaml:"maxRowsToRead"`
}

// LabelsField is one result column whose entries get labels.
type LabelsField struct {
	// Column holds an array of entries.
	Column string `yaml:"column"`
	// Key is the object key that holds the address or, with NamesColumn, the
	// name of the position that holds it.
	Key string `yaml:"key"`
	// NamesColumn is the sibling column that names the positions of a
	// positional entry. Empty: the entries are objects.
	NamesColumn string `yaml:"namesColumn"`
}

// LabelsOutputNames are the names of the three values written to each entry.
type LabelsOutputNames struct {
	Labels string `yaml:"labels"`
	Label  string `yaml:"label"`
	Status string `yaml:"status"`
}

const (
	defaultLabelsBatch         = 2000
	defaultLabelsTimeoutSec    = 5
	defaultLabelsMaxRowsToRead = 50_000_000

	// labelsSourceType is the source type the label statement is written for.
	labelsSourceType = "clickhouse"

	// maxLabelsListBytes caps the literal address list of one statement, well
	// under ClickHouse's default max_query_size of 256 KiB, whatever Batch says.
	maxLabelsListBytes = 200_000

	labelStatusLabeled     = "labeled"
	labelStatusNone        = "none"
	labelStatusNoCoverage  = "no_coverage"
	labelStatusUnavailable = "unavailable"
)

var (
	// A directory chain name goes into the statement as a literal.
	labelsChainRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	// Only an address of this form is looked up: every chain's address alphabet
	// fits it, and nothing in it can end a quoted literal.
	labelsAddressRe = regexp.MustCompile(`^[0-9A-Za-z]{1,256}$`)
)

// labelsSQLSource is the labels source.
type labelsSQLSource interface {
	RunSQL(context.Context, string, parameters.ParamValues) (any, error)
}

type bitqueryLabels struct {
	tool          string
	sourceName    string
	noCoverage    bool
	fields        []LabelsField
	addressParam  string
	addressColumn string
	// unquote: the address parameter is escaped as a single-quoted literal, so
	// its parsed value arrives wrapped in quotes.
	unquote   bool
	rank      map[string]int
	names     [3]string
	batch     int
	timeout   time.Duration
	maxRows   int64
	chainList string
	// source is nil when the tools were initialized without sources (offline
	// flows that never invoke a tool); a call then reads unavailable.
	source labelsSQLSource
}

// initLabels validates bitqueryLabels against the tool's parameters and, when
// the server supplied its sources, checks that the labels source exists and is
// a clickhouse source, so a typo fails at startup.
func (cfg Config) initLabels(ctx context.Context, params parameters.Parameters) (*bitqueryLabels, error) {
	c := cfg.Labels
	if c == nil {
		return nil, nil
	}
	fail := func(format string, args ...any) (*bitqueryLabels, error) {
		return nil, fmt.Errorf("tool %q: bitqueryLabels: %s", cfg.Name, fmt.Sprintf(format, args...))
	}
	l := &bitqueryLabels{
		tool:          cfg.Name,
		sourceName:    c.Source,
		noCoverage:    c.NoCoverage,
		fields:        slices.Clone(c.Fields),
		addressParam:  c.AddressParam,
		addressColumn: c.AddressColumn,
		rank:          map[string]int{},
		names:         [3]string{"labels", "label", "label_status"},
		batch:         c.Batch,
		timeout:       time.Duration(c.TimeoutSec) * time.Second,
		maxRows:       c.MaxRowsToRead,
	}

	if len(c.Fields) == 0 && c.AddressParam == "" {
		return fail("fields is empty")
	}
	columns := map[string]bool{}
	for _, f := range c.Fields {
		if f.Column == "" || f.Key == "" {
			return fail("every field needs column and key")
		}
		if columns[f.Column] {
			return fail("column %q is listed twice", f.Column)
		}
		columns[f.Column] = true
		if f.NamesColumn == f.Column {
			return fail("column %q cannot name its own positions", f.Column)
		}
	}
	for _, f := range c.Fields {
		if f.NamesColumn != "" && columns[f.NamesColumn] {
			return fail("namesColumn %q is also a labelled column", f.NamesColumn)
		}
	}

	if (c.AddressParam == "") != (c.AddressColumn == "") {
		return fail("addressParam and addressColumn go together")
	}
	if c.AddressParam != "" {
		var param parameters.Parameter
		for _, p := range params {
			if p.GetName() == c.AddressParam {
				param = p
				break
			}
		}
		if param == nil {
			return fail("addressParam %q is not a parameter of the tool", c.AddressParam)
		}
		if param.GetType() != "string" {
			return fail("addressParam %q must be a string parameter, not %s", c.AddressParam, param.GetType())
		}
		if sp, ok := param.(*parameters.StringParameter); ok && sp.Escape != nil {
			if *sp.Escape != "single-quotes" {
				return fail("addressParam %q may declare only the single-quotes escape", c.AddressParam)
			}
			l.unquote = true
		}
		if columns[c.AddressColumn] {
			return fail("addressColumn %q is also a labelled column", c.AddressColumn)
		}
		for _, f := range c.Fields {
			if f.NamesColumn == c.AddressColumn {
				return fail("addressColumn %q is also a namesColumn", c.AddressColumn)
			}
		}
	}

	for i, t := range c.Priority {
		if t == "" {
			return fail("priority has an empty label type")
		}
		if _, dup := l.rank[t]; dup {
			return fail("priority lists %q twice", t)
		}
		l.rank[t] = i
	}

	if n := c.OutputNames; n != nil {
		for i, v := range []string{n.Labels, n.Label, n.Status} {
			if v != "" {
				l.names[i] = v
			}
		}
	}
	if l.names[0] == l.names[1] || l.names[0] == l.names[2] || l.names[1] == l.names[2] {
		return fail("outputNames must be three different names")
	}

	if c.Batch < 0 || c.TimeoutSec < 0 || c.MaxRowsToRead < 0 {
		return fail("batch, timeoutSec and maxRowsToRead cannot be negative")
	}
	if l.batch == 0 {
		l.batch = defaultLabelsBatch
	}
	if l.timeout == 0 {
		l.timeout = defaultLabelsTimeoutSec * time.Second
	}
	if l.maxRows == 0 {
		l.maxRows = defaultLabelsMaxRowsToRead
	}

	if !c.NoCoverage {
		if c.Source == "" {
			return fail("source is required unless noCoverage is set")
		}
		if len(c.Chains) == 0 {
			return fail("chains is empty")
		}
		quoted := make([]string, 0, len(c.Chains))
		for _, ch := range c.Chains {
			if !labelsChainRe.MatchString(ch) {
				return fail("chain %q is not a directory chain name", ch)
			}
			q := "'" + ch + "'"
			if slices.Contains(quoted, q) {
				return fail("chain %q is listed twice", ch)
			}
			quoted = append(quoted, q)
		}
		l.chainList = strings.Join(quoted, ", ")
	}

	if lookup, ok := tools.SourceLookupFromContext(ctx); ok && c.Source != "" {
		s, found := lookup.GetSource(c.Source)
		if !found {
			return fail("unable to retrieve labels source %q", c.Source)
		}
		sqlSource, canRun := s.(labelsSQLSource)
		if s.SourceType() != labelsSourceType || !canRun {
			return fail("labels source %q is not a clickhouse source", c.Source)
		}
		l.source = sqlSource
	}
	return l, nil
}

// labelsSlot is one entry that gets labels.
type labelsSlot struct {
	entries []any // the field's array
	index   int
	address string // normalized; "" when the entry has no address to look up
	// appendNames is nil for an object entry and holds which of the three
	// values a positional entry gets appended, in order.
	appendNames []int
}

// labelsCall gathers what one call writes.
type labelsCall struct {
	l         *bitqueryLabels
	slots     []labelsSlot
	addresses []string
	seen      map[string]bool
	// rowAddress is the normalized address parameter ("": none to look up).
	rowAddress string
	warnings   []string
}

// enrich adds labels to a decoded result in place and returns it. A result
// that is not rows (a string, say) comes back untouched.
func (l *bitqueryLabels) enrich(ctx context.Context, resp any, paramsMap map[string]any) any {
	var rows []map[string]any
	switch r := resp.(type) {
	case []any:
		for _, x := range r {
			if row, ok := x.(map[string]any); ok {
				rows = append(rows, row)
			}
		}
	case map[string]any:
		// A one-row answer whose format header was lost decodes to the row itself.
		rows = []map[string]any{r}
	}
	if len(rows) == 0 {
		return resp
	}

	c := &labelsCall{l: l, seen: map[string]bool{}}
	if l.addressParam != "" {
		v, _ := paramsMap[l.addressParam].(string)
		if l.unquote && len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
			v = v[1 : len(v)-1]
		}
		c.rowAddress = c.add(v)
	}
	for _, row := range rows {
		c.collect(row)
	}

	status := labelStatusNone
	var found map[string][]labelsEntry
	switch {
	case l.noCoverage:
		status = labelStatusNoCoverage
	case len(c.addresses) > 0:
		var err error
		found, err = l.lookup(ctx, c.addresses)
		if err != nil {
			status = labelStatusUnavailable
			if logger, lerr := util.LoggerFromContext(ctx); lerr == nil {
				logger.WarnContext(ctx, "labels lookup failed, result returned without labels",
					"tool", l.tool, "source", l.sourceName, "addresses", len(c.addresses), "err", err.Error())
			}
		}
	}

	for _, s := range c.slots {
		vals := l.values(status, s.address, found)
		entry := s.entries[s.index]
		if s.appendNames == nil {
			obj := entry.(map[string]any)
			for i, name := range l.names {
				if _, taken := obj[name]; taken {
					c.warn(fmt.Sprintf("an entry already has %q", name))
					continue
				}
				obj[name] = vals[i]
			}
			continue
		}
		arr := slices.Clip(entry.([]any))
		for _, i := range s.appendNames {
			arr = append(arr, vals[i])
		}
		s.entries[s.index] = arr
	}

	if l.addressColumn != "" {
		labels := l.values(status, c.rowAddress, found)[0]
		for _, row := range rows {
			if _, taken := row[l.addressColumn]; taken {
				c.warn(fmt.Sprintf("the result already has %q", l.addressColumn))
				continue
			}
			row[l.addressColumn] = labels
		}
	}

	if len(c.warnings) > 0 {
		if logger, err := util.LoggerFromContext(ctx); err == nil {
			logger.WarnContext(ctx, "labels: values left as they were", "tool", l.tool, "reasons", c.warnings)
		}
	}
	return resp
}

func (c *labelsCall) warn(msg string) {
	if !slices.Contains(c.warnings, msg) {
		c.warnings = append(c.warnings, msg)
	}
}

// add normalizes an address and queues it for the lookup once. It returns ""
// for a value that is not looked up.
func (c *labelsCall) add(v any) string {
	s, _ := v.(string)
	if len(s) >= 2 && (s[:2] == "0x" || s[:2] == "0X") {
		s = strings.ToLower(s)
	}
	if !labelsAddressRe.MatchString(s) {
		return ""
	}
	if !c.seen[s] {
		c.seen[s] = true
		c.addresses = append(c.addresses, s)
	}
	return s
}

// collect queues the entries of one row. The positional fields that share a
// names column are taken together or not at all, so that every entry of the
// row keeps exactly as many positions as its names column names.
func (c *labelsCall) collect(row map[string]any) {
	plans := map[string][]int{}
	skip := map[string]bool{}
	for _, f := range c.l.fields {
		if f.NamesColumn == "" || skip[f.NamesColumn] {
			continue
		}
		names, ok := row[f.NamesColumn].([]any)
		if !ok || !slices.Contains(names, any(f.Key)) {
			skip[f.NamesColumn] = true
			c.warn(fmt.Sprintf("column %q does not name position %q", f.NamesColumn, f.Key))
			continue
		}
		if _, done := plans[f.NamesColumn]; done {
			continue
		}
		plan := []int{}
		for i, name := range c.l.names {
			if slices.Contains(names, any(name)) {
				c.warn(fmt.Sprintf("column %q already names %q", f.NamesColumn, name))
				continue
			}
			plan = append(plan, i)
		}
		plans[f.NamesColumn] = plan
	}

	for _, f := range c.l.fields {
		entries, ok := row[f.Column].([]any)
		if !ok {
			continue
		}
		if f.NamesColumn == "" {
			for i, e := range entries {
				if obj, ok := e.(map[string]any); ok {
					c.slots = append(c.slots, labelsSlot{entries: entries, index: i, address: c.add(obj[f.Key])})
				}
			}
			continue
		}
		plan, ok := plans[f.NamesColumn]
		if !ok || skip[f.NamesColumn] {
			continue
		}
		names := row[f.NamesColumn].([]any)
		pos := slices.Index(names, any(f.Key))
		for i, e := range entries {
			arr, ok := e.([]any)
			if !ok {
				continue
			}
			var addr any
			if pos < len(arr) {
				addr = arr[pos]
			}
			c.slots = append(c.slots, labelsSlot{entries: entries, index: i, address: c.add(addr), appendNames: plan})
		}
	}

	for column, plan := range plans {
		if skip[column] || len(plan) == 0 {
			continue
		}
		names := slices.Clip(row[column].([]any))
		for _, i := range plan {
			names = append(names, c.l.names[i])
		}
		row[column] = names
	}
}

// values are the labels, the pick and the status of one address. The labels
// are null when nothing was looked up, and an empty list when the lookup found
// none.
func (l *bitqueryLabels) values(status, address string, found map[string][]labelsEntry) [3]any {
	if status == labelStatusNoCoverage || status == labelStatusUnavailable {
		return [3]any{nil, nil, status}
	}
	entries := found[address]
	if len(entries) == 0 {
		return [3]any{[]any{}, nil, labelStatusNone}
	}
	texts := make([]string, 0, len(entries))
	for _, e := range entries {
		if t := e.text(); !slices.Contains(texts, t) {
			texts = append(texts, t)
		}
	}
	slices.Sort(texts)
	labels := make([]any, len(texts))
	for i, t := range texts {
		labels[i] = t
	}
	return [3]any{labels, l.pick(entries).text(), labelStatusLabeled}
}

// labelsEntry is one label of an address.
type labelsEntry struct {
	chain, labelType, value string
}

func (e labelsEntry) text() string {
	return e.chain + "/" + e.labelType + ":" + e.value
}

// pick returns the label whose type comes first in the priority list; a type
// the list does not name comes after every listed one, in name order. Labels
// of one type are taken by value, then by chain, lexicographically.
func (l *bitqueryLabels) pick(entries []labelsEntry) labelsEntry {
	rank := func(t string) int {
		if r, ok := l.rank[t]; ok {
			return r
		}
		return len(l.rank)
	}
	return slices.MinFunc(entries, func(a, b labelsEntry) int {
		return cmp.Or(
			cmp.Compare(rank(a.labelType), rank(b.labelType)),
			strings.Compare(a.labelType, b.labelType),
			strings.Compare(a.value, b.value),
			strings.Compare(a.chain, b.chain),
		)
	})
}

// lookup reads the labels of the addresses, one statement per batch, all
// within the step's timeout.
func (l *bitqueryLabels) lookup(ctx context.Context, addresses []string) (map[string][]labelsEntry, error) {
	if l.source == nil {
		return nil, fmt.Errorf("the labels source is not initialized")
	}
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	out := map[string][]labelsEntry{}
	for rest := addresses; len(rest) > 0; {
		n, size := 0, 0
		for n < len(rest) && n < l.batch && (n == 0 || size+len(rest[n])+4 <= maxLabelsListBytes) {
			size += len(rest[n]) + 4
			n++
		}
		batch := rest[:n]
		rest = rest[n:]
		res, err := l.source.RunSQL(ctx, l.statement(ctx, batch), nil)
		if err != nil {
			return nil, err
		}
		rows, ok := res.([]any)
		if !ok {
			return nil, fmt.Errorf("unexpected answer to the labels statement")
		}
		for _, x := range rows {
			r, ok := x.(map[string]any)
			if !ok {
				continue
			}
			addr := labelsString(r["address"])
			out[addr] = append(out[addr], labelsEntry{
				chain:     labelsString(r["chain"]),
				labelType: labelsString(r["label_type"]),
				value:     labelsString(r["label_value"]),
			})
		}
	}
	return out, nil
}

// statement reads the labels of a batch of validated addresses. Its
// max_execution_time is the time left before the step's deadline.
func (l *bitqueryLabels) statement(ctx context.Context, addresses []string) string {
	secs := int(l.timeout / time.Second)
	if dl, ok := ctx.Deadline(); ok {
		secs = int(math.Ceil(time.Until(dl).Seconds()))
	}
	quoted := make([]string, len(addresses))
	for i, a := range addresses {
		quoted[i] = "'" + a + "'"
	}
	return fmt.Sprintf(`SELECT address, chain, label_type, label_value
FROM directory.labels
WHERE address IN (%s) AND chain IN (%s)
GROUP BY address, chain, label_type, label_value
SETTINGS max_execution_time = %d, max_rows_to_read = %d, read_overflow_mode = 'throw', timeout_overflow_mode = 'throw'`,
		strings.Join(quoted, ", "), l.chainList, max(1, secs), l.maxRows)
}

func labelsString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		return string(s)
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}
