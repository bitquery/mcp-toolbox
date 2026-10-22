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

package riskreport

// One report, the direct view: the target's summary, its 1-hop counterparties
// in the report asset with their labels, the score and the flags.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/util"
)

// maxHopsCap is the deepest search a caller may ask for; the deadline and the
// row budget end most searches sooner.
const maxHopsCap = 6

// noExposure is the answer when no counterparty carries an entity label. It
// never says the address is clean: only that our labels found nothing.
const noExposure = "no exposure found in our labels"

type assetOut struct {
	Asset    string `json:"asset"`
	Symbol   string `json:"symbol"`
	Contract string `json:"contract,omitempty"`
	Decimals int    `json:"decimals"`
	Received string `json:"received"`
	Sent     string `json:"sent"`
	Transfer int64  `json:"transfers"`
	TxCount  int64  `json:"tx_count"`
}

type ownLabels struct {
	Owner      string   `json:"owner,omitempty"`
	OwnerType  string   `json:"owner_type,omitempty"`
	OtherTypes []string `json:"other_types,omitempty"`
	Tags       []string `json:"tags"`
}

type summaryOut struct {
	Chain            string     `json:"chain"`
	Address          string     `json:"address"`
	Asset            assetOut   `json:"asset"`
	Balance          string     `json:"balance"`
	Received         string     `json:"received"`
	Sent             string     `json:"sent"`
	TxCount          int64      `json:"tx_count"`
	FirstActivity    string     `json:"first_activity"`
	LastActivity     string     `json:"last_activity"`
	FirstActivityAny string     `json:"first_activity_any"`
	LastActivityAny  string     `json:"last_activity_any"`
	Assets           []assetOut `json:"assets"`
	AssetsTruncated  bool       `json:"assets_truncated,omitempty"`
	Labels           ownLabels  `json:"labels"`
	CoverageTier     string     `json:"coverage_tier"`
	AttributedPct    float64    `json:"attributed_pct"`
}

type scoreOut struct {
	Value         float64  `json:"value"`
	ReceivedScore *float64 `json:"received_score"`
	SentScore     *float64 `json:"sent_score"`
	DirectFloor   float64  `json:"direct_floor"`
	// The counterparty that set the direct floor, so a score is never
	// unexplained when the connections hold nothing above min_amount_usd.
	DirectFloorFrom map[string]any `json:"direct_floor_from,omitempty"`
	Formula         string         `json:"formula"`
	WeightsUsed     map[string]any `json:"weights_used"`
	TopDrivers      []driver       `json:"top_drivers"`
}

type connectionOut struct {
	Entity     string `json:"entity"`
	EntityType string `json:"entity_type"`
	// SameEntity is set when this connection names the target's own entity
	// (its entity equals summary.labels.owner): a self-transfer. It is kept in
	// the output and the breakdown for transparency but left out of the score.
	SameEntity       bool     `json:"same_entity,omitempty"`
	Subtype          string   `json:"subtype,omitempty"`
	Licence          string   `json:"licence,omitempty"`
	Received         string   `json:"received"`
	ReceivedHops     *int     `json:"received_shortest_hops"`
	Sent             string   `json:"sent"`
	SentHops         *int     `json:"sent_shortest_hops"`
	OtherTypes       []string `json:"other_types,omitempty"`
	LabelValue       string   `json:"label_value"`
	LabelSource      string   `json:"label_source"`
	LabelSourceClass string   `json:"label_source_class"`
	LabelRecordedAt  string   `json:"label_recorded_at"`
	// The other sources that give this address the same entity type.
	OtherSources []string `json:"other_sources,omitempty"`
	Addresses    []string `json:"addresses"`
	EvidencePath []string `json:"evidence_path"`

	recvRaw, sentRaw *big.Int
}

// unnamedOut is a 1-hop counterparty with no entity label: its amounts are
// unattributed. The largest are listed one by one, the rest summed.
type unnamedOut struct {
	Address      string   `json:"address"`
	Received     string   `json:"received"`
	ReceivedHops *int     `json:"received_shortest_hops"`
	Sent         string   `json:"sent"`
	SentHops     *int     `json:"sent_shortest_hops"`
	EvidencePath []string `json:"evidence_path"`
	// Cut is why the branch ended here (view all): hub, max_hops, budget,
	// history_incomplete or unspent.
	Cut string `json:"cut,omitempty"`
	// Note qualifies the cut: a hub found without an estimate of its size.
	Note string `json:"note,omitempty"`

	recvRaw, sentRaw *big.Int
}

type unnamedMore struct {
	Addresses int    `json:"addresses"`
	Received  string `json:"received"`
	Sent      string `json:"sent"`
}

type breakdownRow struct {
	EntityType string  `json:"entity_type"`
	Amount     string  `json:"amount"`
	Pct        float64 `json:"pct"`
}

type flagOut struct {
	Type        string         `json:"type"`
	Detail      map[string]any `json:"detail"`
	EvidenceTxs []string       `json:"evidence_txs"`
}

type statementCost struct {
	Name      string `json:"name"`
	RowsRead  int64  `json:"rows_read"`
	BytesRead int64  `json:"bytes_read"`
	ElapsedMS int64  `json:"elapsed_ms"`
}

type costOut struct {
	Statements int             `json:"statements"`
	RowsRead   int64           `json:"rows_read"`
	BytesRead  int64           `json:"bytes_read"`
	Detail     []statementCost `json:"detail"`
}

type cutOut struct {
	Reason       string `json:"reason"`
	Direction    string `json:"direction,omitempty"`
	Counterparts int64  `json:"counterparties"`
	Amount       string `json:"amount,omitempty"`
	Note         string `json:"note"`
}

type paramsOut struct {
	Method         string            `json:"method"`
	View           string            `json:"view"`
	Direction      string            `json:"direction"`
	MaxHops        int               `json:"max_hops,omitempty"`
	MaxHopsReached int               `json:"max_hops_reached"`
	Window         map[string]string `json:"window"`
	MinAmountUSD   float64           `json:"min_amount_usd"`
	DustUSD        float64           `json:"dust_usd"`
	USDConversion  string            `json:"usd_conversion"`
	FlagsStatus    map[string]string `json:"flags_status"`
	UnmappedTypes  []string          `json:"unmapped_label_types"`
	Complete       bool              `json:"complete"`
	Cuts           []cutOut          `json:"cuts"`
	Notes          []string          `json:"notes"`
	// The five invariants of view all, each "ok" or what it found.
	Invariants map[string]string `json:"invariants,omitempty"`
	// DataAsOf is the report's pin: every statement read the chain strictly
	// before it (also window.before_time).
	DataAsOf    string `json:"data_as_of"`
	GeneratedAt string `json:"generated_at"`
}

// reportHead is a compact leading summary: the few fields a reader (or a
// truncated json answer) needs first. They are duplicated from score and
// params, which stay the authoritative locations.
type reportHead struct {
	Score          float64           `json:"score"`
	DataAsOf       string            `json:"data_as_of"`
	Complete       bool              `json:"complete"`
	View           string            `json:"view"`
	MaxHopsReached int               `json:"max_hops_reached"`
	FlagsStatus    map[string]string `json:"flags_status"`
}

type reportOut struct {
	// AtAGlance leads the object so a truncated json answer still shows the key
	// facts; it is populated last, from the finished report.
	AtAGlance   reportHead      `json:"at_a_glance"`
	Exposure    string          `json:"exposure,omitempty"`
	Summary     summaryOut      `json:"summary"`
	Score       scoreOut        `json:"score"`
	Connections []connectionOut `json:"connections"`
	// Unnamed 1-hop counterparties (no entity label), largest first; the rest
	// are summed in UnnamedMore. Their amounts are the unattributed ones.
	UnnamedConnections []unnamedOut              `json:"unnamed_connections"`
	UnnamedMore        *unnamedMore              `json:"unnamed_more,omitempty"`
	Breakdown          map[string][]breakdownRow `json:"breakdown"`
	Unattributed       map[string]string         `json:"unattributed"`
	// View all: the unattributed amount of each direction by the cut that ended
	// its branches; they add up to unattributed.
	UnattributedByCut map[string]map[string]string `json:"unattributed_by_cut,omitempty"`
	Flags             []flagOut                    `json:"flags"`
	Params            paramsOut                    `json:"params"`
	Cost              costOut                      `json:"cost"`
	TimingMS          int64                        `json:"timing_ms"`
}

// request is a validated call.
type request struct {
	chain     string
	address   string
	asset     tronAsset
	assetSet  bool
	direction string
	view      string
	format    string
	maxHops   int
	w         window
	minUSD    float64
	weights   weights
}

func agentErr(format string, a ...any) util.ToolboxError {
	return util.NewAgentError(fmt.Sprintf(format, a...), nil)
}

func (t Tool) parse(p map[string]any) (request, util.ToolboxError) {
	str := func(k string) string {
		s, _ := p[k].(string)
		return strings.TrimSpace(s)
	}
	r := request{
		chain:     normalizeChain(str("chain")),
		address:   str("address"),
		direction: strings.ToLower(str("direction")),
		view:      strings.ToLower(str("view")),
		minUSD:    t.m.th.MinAmountUSD,
	}
	if r.direction == "" {
		r.direction = "both"
	}
	if r.view == "" {
		r.view = "direct"
	}
	if r.chain == "" {
		return r, agentErr("chain is required (tron)")
	}
	if !slices.Contains(supportedChains, r.chain) {
		return r, agentErr("chain %q is not supported; use tron", truncate(r.chain, 32))
	}
	if r.chain != "tron" {
		return r, agentErr("chain %s is not available yet in this report; use tron", r.chain)
	}
	if !tronAddressRe.MatchString(r.address) {
		return r, agentErr("address must be a Tron base58 address (T followed by 33 characters)")
	}
	if !slices.Contains([]string{"both", "received", "sent"}, r.direction) {
		return r, agentErr("direction must be one of: both, received, sent")
	}
	if r.view != "direct" && r.view != "all" {
		return r, agentErr("view must be direct or all")
	}
	switch f := strings.ToLower(str("format")); f {
	case "", "markdown":
		// markdown is the default: a concise human report, with the whole report
		// alongside it as structured content.
		r.format = "markdown"
	case "json":
		r.format = "json"
	case "csv":
		return r, agentErr("format csv is not available yet; use markdown or json")
	default:
		return r, agentErr("format must be markdown or json")
	}
	a, set, err := parseTronAsset(str("asset"))
	if err != nil {
		return r, util.NewAgentError(err.Error(), nil)
	}
	r.asset, r.assetSet = a, set
	if v, ok := p["min_amount_usd"]; ok && v != nil {
		f, ok := toFloat(v)
		if !ok || f < 0 {
			return r, agentErr("min_amount_usd must be a number of 0 or more")
		}
		r.minUSD = f
	}
	r.maxHops = t.m.th.MaxHops
	if v, ok := p["max_hops"]; ok && v != nil {
		n, ok := toFloat(v)
		if !ok || n < 1 {
			return r, agentErr("max_hops must be 1 or more")
		}
		if n > maxHopsCap || n != math.Trunc(n) {
			return r, agentErr("max_hops must be a whole number from 1 to %d", maxHopsCap)
		}
		r.maxHops = int(n)
	}
	var perr error
	if r.w.After, perr = parseTime(str("after_time")); perr != nil {
		return r, agentErr("after_time: %s", perr.Error())
	}
	if r.w.Before, perr = parseTime(str("before_time")); perr != nil {
		return r, agentErr("before_time: %s", perr.Error())
	}
	if str("before_time") != "" && r.w.Before.IsZero() {
		// The zero time stands for "not given"; a window ending there is empty.
		return r, agentErr("before_time must be later than 0001-01-01")
	}
	given := r.w.Before
	r.w = pinWindow(r.w, time.Now())
	if !r.w.After.Before(r.w.Before) {
		if !given.IsZero() && given.Equal(r.w.Before) {
			return r, agentErr("after_time must be earlier than before_time")
		}
		return r, agentErr("after_time must be in the past")
	}
	override, _ := p["weights"].(map[string]any)
	w, werr := t.m.mergeWeights(override)
	if werr != nil {
		return r, util.NewAgentError(werr.Error(), nil)
	}
	r.weights = w
	return r, nil
}

// pinWindow fixes the window's end once, at the start of the report: the
// caller's before_time when it is in the past, else now (to the second). Every
// statement then reads strictly before the same moment, so the assets
// statement and the counterparty statement, which run seconds apart, total
// the same transfers, and a transfer that lands while the report runs is in
// none of them. Residual: a row ingested late, with a Block_Time below the pin,
// can still be seen by a later statement and not by an earlier one.
func pinWindow(w window, now time.Time) window {
	pin := now.UTC().Truncate(time.Second)
	if w.Before.IsZero() || w.Before.After(pin) {
		w.Before = pin
	}
	return w
}

// timeLayouts are the spellings the after_time / before_time pattern admits:
// a date, or a date and a time with a T or a space, minutes or seconds (a
// fraction is read after the seconds), and no zone, Z, +HH:MM or +HHMM.
var timeLayouts = func() []string {
	out := []string{time.DateOnly}
	for _, sep := range []string{"T", " "} {
		for _, clock := range []string{"15:04:05", "15:04"} {
			for _, zone := range []string{"", "Z07:00", "Z0700"} {
				out = append(out, "2006-01-02"+sep+clock+zone)
			}
		}
	}
	return out
}()

// parseTime reads YYYY-MM-DD, YYYY-MM-DD HH:MM[:SS], ISO-8601 with an optional
// fraction and offset, or 10-digit unix seconds. A fraction rounds up to the
// next second: Block_Time holds whole seconds, so ">= x.5" is ">= x+1" and
// "< x.5" is "< x+1", and the statements print whole seconds. "" is an open
// end.
func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if len(s) == 10 && strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) < 0 {
		var sec int64
		fmt.Sscanf(s, "%d", &sec)
		return time.Unix(sec, 0).UTC(), nil
	}
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			t = t.UTC()
			if whole := t.Truncate(time.Second); whole.Before(t) {
				return whole.Add(time.Second), nil
			}
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("use YYYY-MM-DD, YYYY-MM-DD HH:MM[:SS], ISO-8601 or 10-digit unix seconds")
}

// usdRaw converts a USD amount to raw units of an asset at a constant price.
func usdRaw(usd, price float64, decimals int) *big.Int {
	if price <= 0 || usd <= 0 {
		return new(big.Int)
	}
	f := new(big.Float).SetPrec(200).SetFloat64(usd / price)
	f.Mul(f, new(big.Float).SetPrec(200).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)))
	out, _ := f.Int(nil)
	return out
}

// formatUnits writes a raw integer amount with the asset's decimals.
func formatUnits(raw *big.Int, decimals int) string {
	if raw == nil {
		return "0"
	}
	neg := raw.Sign() < 0
	s := new(big.Int).Abs(raw).String()
	if decimals > 0 {
		if len(s) <= decimals {
			s = strings.Repeat("0", decimals-len(s)+1) + s
		}
		s = s[:len(s)-decimals] + "." + s[len(s)-decimals:]
	}
	if neg {
		s = "-" + s
	}
	return s
}

func ratio(a, b *big.Int) float64 {
	if b == nil || b.Sign() == 0 || a == nil {
		return 0
	}
	f, _ := new(big.Float).Quo(new(big.Float).SetInt(a), new(big.Float).SetInt(b)).Float64()
	return f
}

func intPtr(n int) *int { return &n }

func (t Tool) run(ctx context.Context, p map[string]any) (any, util.ToolboxError) {
	start := time.Now()
	if t.chains == nil || t.labels == nil {
		return nil, util.NewClientServerError("the risk report is not initialized with its sources", 500, nil)
	}
	req, terr := t.parse(p)
	if terr != nil {
		return nil, terr
	}
	src, ok := t.chains[req.chain]
	if !ok {
		return nil, agentErr("chain %s is not configured for this report", req.chain)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(t.m.th.DeadlineSeconds)*time.Second)
	defer cancel()

	ex := &executor{src: src, labels: t.labels, th: t.m.th}
	rep, err := t.direct(ctx, ex, req)
	if err != nil {
		return nil, util.ProcessGeneralError(err)
	}
	rep.Cost = ex.cost
	rep.TimingMS = time.Since(start).Milliseconds()
	// format json returns the report as the single JSON document, exactly as
	// before. format markdown (the default) returns a concise human report as the
	// text content — small enough for a client's response cap — with the whole
	// report alongside it as structured content for a UI to render.
	if req.format == "json" {
		return rep, nil
	}
	b, err := json.Marshal(rep)
	if err != nil {
		return nil, util.ProcessGeneralError(err)
	}
	var structured map[string]any
	if err := json.Unmarshal(b, &structured); err != nil {
		return nil, util.ProcessGeneralError(err)
	}
	return tools.StructuredResult{Content: []any{markdownReport(rep)}, Structured: structured}, nil
}

// direct builds the direct view of one Tron address.
func (t Tool) direct(ctx context.Context, ex *executor, req request) (*reportOut, error) {
	m, th := t.m, t.m.th
	rep := &reportOut{
		Breakdown:          map[string][]breakdownRow{},
		Unattributed:       map[string]string{},
		Flags:              []flagOut{},
		Connections:        []connectionOut{},
		UnnamedConnections: []unnamedOut{},
	}
	rep.Params = paramsOut{
		Method:         "direct view: 1-hop counterparties in the report asset, attributed by their own labels",
		View:           req.view,
		Direction:      req.direction,
		MaxHopsReached: 1,
		Window:         map[string]string{},
		MinAmountUSD:   req.minUSD,
		DustUSD:        th.DustUSD,
		FlagsStatus:    map[string]string{},
		UnmappedTypes:  []string{},
		Complete:       true,
		Cuts:           []cutOut{},
		Notes:          []string{"exposure beyond the first hop was not assessed in this view"},
		GeneratedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	if req.view == "all" {
		rep.Params.Method = "view all: counterparties in the report asset to max_hops hops; received funds traced backward and sent funds forward by the balance-aware rule, each branch ending at an entity label or a cut"
		rep.Params.MaxHops = req.maxHops
		rep.Params.Notes = []string{fmt.Sprintf("exposure deeper than %d hops was not assessed", req.maxHops)}
	}
	if !req.w.After.IsZero() {
		rep.Params.Window["after_time"] = req.w.After.Format(time.RFC3339)
	}
	// The pin (parse sets it on every call): the end of the window and the
	// moment the data is as of.
	rep.Params.Window["before_time"] = req.w.Before.UTC().Format(time.RFC3339)
	rep.Params.DataAsOf = req.w.Before.UTC().Format(time.RFC3339)

	// 1. Every asset of the address.
	assets, err := ex.assets(ctx, req.address, req.w)
	if err != nil {
		return nil, err
	}
	asset, row, ok := pickAsset(assets, req, m.assetUSD["tron"])
	if !ok {
		if req.assetSet {
			return nil, fmt.Errorf("the address has no transfers of asset %s in the window", req.asset.Key)
		}
		return nil, fmt.Errorf("the address has no transfers in the window")
	}
	// The report asset's USD price: the configured constant if there is one
	// (USDT = 1), else one read from the chain's price store under the pin, the
	// way price_at reads it. A price lets min_amount_usd and the dust and
	// look-alike thresholds, and the direct floor, apply in USD; without one none
	// of them is applied (the native dust fallback aside).
	price := m.assetUSD["tron"][asset.Key]
	priceFromStore := false
	if price <= 0 {
		p, perr := ex.price(ctx, asset, req.w.Before)
		if perr != nil {
			// The deadline or the row budget stops the whole report, as any other
			// statement does; a store that is merely unavailable must not, so the
			// report falls back to the no-price path and says so.
			if classifyLimit(ctx, perr) == stopLimit {
				return nil, perr
			}
			rep.Params.Notes = append(rep.Params.Notes, "a USD price for the report asset could not be read, so min_amount_usd and the dust and look-alike thresholds are not applied to it")
		} else if p > 0 {
			price, priceFromStore = p, true
		}
	}
	minRaw := usdRaw(req.minUSD, price, asset.Decimals)
	dustRaw := usdRaw(th.DustUSD, price, asset.Decimals)
	capRaw := usdRaw(th.LookalikeAmountCapUSD, price, asset.Decimals)
	switch {
	case price > 0 && priceFromStore:
		rep.Params.USDConversion = fmt.Sprintf("store: 1 %s = %g USD (as of %s)", asset.Symbol, round(price, 2), rep.Params.DataAsOf)
	case price > 0:
		rep.Params.USDConversion = fmt.Sprintf("constant: 1 %s = %g USD", asset.Symbol, price)
	default:
		rep.Params.USDConversion = "none: no USD price for this asset, so min_amount_usd and the dust and look-alike thresholds are not applied to it"
		minRaw, capRaw = new(big.Int), new(big.Int)
		if asset.Native {
			dustRaw = big.NewInt(th.NativeDustUnits)
		}
	}

	// 2. The 1-hop counterparties, in the report asset and in every other one.
	edges, other, err := ex.counterparties(ctx, cpInput{Addr: req.address, Asset: asset, W: req.w, FirstIn: row.firstIn, MinRaw: minRaw, DustRaw: dustRaw,
		NativeDust: th.NativeDustUnits, EarlyDays: th.PassThroughDays, Limit: th.CounterpartyLimit})
	if err != nil {
		return nil, err
	}
	for _, d := range []direction{received, sent} {
		if tot := edges.total[d]; tot.counterparties > int64(countDir(edges.rows, d)) {
			// View all lists every cut, with its amount, after the search.
			if req.view != "all" {
				rep.Params.Cuts = append(rep.Params.Cuts, cutOut{Reason: "counterparty_limit", Direction: d.String(), Counterparts: tot.counterparties - int64(countDir(edges.rows, d)),
					Note: "the smallest counterparties were not looked up; their amounts are counted as unattributed"})
			}
		}
		if n := other.total[d]; n > int64(countContacts(other.rows, d)) {
			rep.Params.Cuts = append(rep.Params.Cuts, cutOut{Reason: "other_asset_counterparty_limit", Direction: d.String(), Counterparts: n - int64(countContacts(other.rows, d)),
				Note: "counterparties in other assets with the fewest transfers were not looked up for labels or poisoning"})
		}
	}

	// 4. Labels of the target and every counterparty.
	addrs := []string{req.address}
	for _, c := range edges.rows {
		addrs = append(addrs, c.Address)
	}
	for _, c := range other.rows {
		addrs = append(addrs, c.Address)
	}
	labels, err := ex.labelRows(ctx, req.chain, addrs)
	if err != nil {
		return nil, err
	}
	// The addresses whose labels were read (one without a label row is not in
	// the map).
	fetched := map[string]bool{}
	for _, a := range addrs {
		fetched[a] = true
	}
	classified := map[string]addressLabels{}
	unmapped := map[string]bool{}
	classOf := func(a string) addressLabels {
		if c, ok := classified[a]; ok {
			return c
		}
		c := m.classify(req.chain, labels[a])
		for _, u := range c.Unmapped {
			unmapped[u] = true
		}
		classified[a] = c
		return c
	}

	// Summary.
	rep.Summary = summaryOut{
		Chain:            req.chain,
		Address:          req.address,
		Asset:            row.out(),
		Received:         row.out().Received,
		Sent:             row.out().Sent,
		TxCount:          row.txs,
		FirstActivity:    row.first,
		LastActivity:     row.last,
		FirstActivityAny: minNonEmpty(assets.rows, func(a assetRow) string { return a.firstAll }),
		LastActivityAny:  maxNonEmpty(assets.rows, func(a assetRow) string { return a.lastAll }),
		CoverageTier:     m.coverage[req.chain],
		AssetsTruncated:  assets.truncated,
	}
	if rep.Summary.CoverageTier == "" {
		rep.Summary.CoverageTier = "unknown"
	}
	bal := new(big.Int).Sub(row.recvAll, row.sentAll)
	rep.Summary.Balance = formatUnits(bal, asset.Decimals)
	// The window always ends at the pin, as the balance does; they differ only
	// when the window has a start.
	if !req.w.After.IsZero() {
		rep.Params.Notes = append(rep.Params.Notes, "balance is over the whole history up to before_time; received, sent, tx_count and activity are in the window")
	}
	for _, a := range assets.rows {
		rep.Summary.Assets = append(rep.Summary.Assets, a.out())
	}
	own := classOf(req.address)
	rep.Summary.Labels.Tags = []string{}
	if at, ok := m.attribute(own, req.weights, received); ok {
		rep.Summary.Labels.Owner = displayEntity(at, req.address)
		rep.Summary.Labels.OwnerType = at.Label.Type
		rep.Summary.Labels.OtherTypes = at.OtherTypes
	}
	for _, r := range append(slices.Clone(own.Attributes), own.Evidence...) {
		tag := r.LabelType + ":" + r.Value
		if !slices.Contains(rep.Summary.Labels.Tags, tag) {
			rep.Summary.Labels.Tags = append(rep.Summary.Labels.Tags, tag)
		}
	}

	// Connections, per (entity type, entity).
	dirs := []direction{received, sent}
	if req.direction == "received" {
		dirs = []direction{received}
	} else if req.direction == "sent" {
		dirs = []direction{sent}
	}
	amounts := map[direction]dirAmounts{}
	attributed := map[direction]*big.Int{}
	byTypeRaw := map[direction]map[string]*big.Int{}
	// sameEntityByType is the part of byTypeRaw that reached the target's own
	// entity. It is subtracted from the score (not the breakdown).
	sameEntityByType := map[direction]map[string]*big.Int{}
	conns := map[string]*connectionOut{}
	var connOrder []string
	directFloor := 0.0
	var floorFrom map[string]any
	unnamed := map[string]*unnamedOut{}
	sanctionsTypes := th.DirectSanctionsTypes
	// The target's own entity (set above from its labels): a counterparty that
	// names it is a self-transfer. same_entity connections are removed from the
	// direction's total before shares are taken, so the score reflects only the
	// flow that reached OTHER entities (removing a self leg raises the remaining
	// connections' shares, by design); they stay in the connections and the
	// breakdown. An entity is (type, name) — the same word under two types is two
	// entities (an exchange and a hacker of the same name) — so both must match;
	// matching the name alone would drop a same-named different-type
	// counterparty's flow from the score, under-reporting risk.
	owner, ownerType := rep.Summary.Labels.Owner, rep.Summary.Labels.OwnerType
	sameEntity := func(at attribution, addr string) bool {
		return owner != "" && at.Label.Type == ownerType && displayEntity(at, addr) == owner
	}
	for _, d := range dirs {
		attributed[d] = new(big.Int)
		byTypeRaw[d] = map[string]*big.Int{}
		sameEntityByType[d] = map[string]*big.Int{}
	}
	// addConn attributes an amount that reached an entity at a hop, with the
	// path's transfers (hop 1 first).
	addConn := func(at attribution, cl addressLabels, addr string, d direction, raw *big.Int, hops int, evidence []string) {
		attributed[d].Add(attributed[d], raw)
		if byTypeRaw[d][at.Label.Type] == nil {
			byTypeRaw[d][at.Label.Type] = new(big.Int)
		}
		byTypeRaw[d][at.Label.Type].Add(byTypeRaw[d][at.Label.Type], raw)

		name := displayEntity(at, addr)
		self := sameEntity(at, addr)
		if self {
			if sameEntityByType[d][at.Label.Type] == nil {
				sameEntityByType[d][at.Label.Type] = new(big.Int)
			}
			sameEntityByType[d][at.Label.Type].Add(sameEntityByType[d][at.Label.Type], raw)
		}
		key := at.Label.Type + "\x00" + name
		conn, ok := conns[key]
		if !ok {
			class := m.sourceClass(at.Label.Row.Source)
			if at.SourceClass != "" {
				class = at.SourceClass
			}
			conn = &connectionOut{Entity: name, EntityType: at.Label.Type, SameEntity: self,
				Subtype: at.Label.Subtype, LabelValue: at.Label.Row.Value,
				LabelSource: at.Label.Row.Source, LabelSourceClass: class, LabelRecordedAt: at.Label.Row.RecordedAt,
				recvRaw: new(big.Int), sentRaw: new(big.Int), Addresses: []string{}, EvidencePath: []string{}}
			if at.Label.Type == licenceUnknownType {
				conn.Licence = "unknown"
			}
			conns[key] = conn
			connOrder = append(connOrder, key)
		}
		for _, src := range sourcesOf(cl, at.Label.Type) {
			if src != conn.LabelSource && !slices.Contains(conn.OtherSources, src) {
				conn.OtherSources = append(conn.OtherSources, src)
			}
		}
		for _, o := range at.OtherTypes {
			if !slices.Contains(conn.OtherTypes, o) {
				conn.OtherTypes = append(conn.OtherTypes, o)
			}
		}
		if !slices.Contains(conn.Addresses, addr) {
			conn.Addresses = append(conn.Addresses, addr)
		}
		if d == received {
			conn.recvRaw.Add(conn.recvRaw, raw)
			if conn.ReceivedHops == nil || *conn.ReceivedHops > hops {
				conn.ReceivedHops = intPtr(hops)
			}
		} else {
			conn.sentRaw.Add(conn.sentRaw, raw)
			if conn.SentHops == nil || *conn.SentHops > hops {
				conn.SentHops = intPtr(hops)
			}
		}
		// The connection's path is the first one found: the search goes hop by
		// hop, so it is a shortest one.
		if len(conn.EvidencePath) == 0 && len(evidence) > 0 {
			conn.EvidencePath = evidence
		}
	}
	// View all: the unlabelled 1-hop counterparties start the search.
	var hop1 []*searchEntry
	listed := map[direction]*big.Int{received: new(big.Int), sent: new(big.Int)}
	for _, c := range edges.rows {
		if !slices.Contains(dirs, c.Dir) {
			continue
		}
		listed[c.Dir].Add(listed[c.Dir], c.RawAboveMin)
		cl := classOf(c.Address)
		at, ok := m.attribute(cl, req.weights, c.Dir)
		if !ok {
			if req.view == "all" {
				if c.RawAboveMin.Sign() > 0 {
					e := &searchEntry{dir: c.Dir, node: c.Address, amount: new(big.Int).Set(c.RawAboveMin), depth: 1, tx: firstOf(c.TxAboveDust, c.Tx)}
					if t, err := time.Parse(time.DateTime, c.Last); err == nil && c.Dir == received {
						e.bound = t.UTC()
					}
					hop1 = append(hop1, e)
				}
				continue
			}
			if c.RawAboveMin.Sign() > 0 {
				u, seen := unnamed[c.Address]
				if !seen {
					u = &unnamedOut{Address: c.Address, recvRaw: new(big.Int), sentRaw: new(big.Int), EvidencePath: []string{}}
					unnamed[c.Address] = u
				}
				if c.Dir == received {
					u.recvRaw.Add(u.recvRaw, c.RawAboveMin)
					u.ReceivedHops = intPtr(1)
				} else {
					u.sentRaw.Add(u.sentRaw, c.RawAboveMin)
					u.SentHops = intPtr(1)
				}
				if ev := firstOf(c.TxAboveDust, c.Tx); ev != "" && len(u.EvidencePath) == 0 {
					u.EvidencePath = []string{ev}
				}
			}
			continue
		}
		aboveDust := c.RawMax != nil && c.RawMax.Cmp(dustRaw) > 0
		if aboveDust {
			// A self-transfer (the counterparty names the target's own entity) does
			// not set the floor, as it is left out of the score.
			sameEnt := sameEntity(at, c.Address)
			if f := req.weights.factor(at.Label.Type, c.Dir); f > directFloor && !sameEnt {
				directFloor = f
				floorFrom = map[string]any{"counterparty": c.Address, "direction": c.Dir.String(), "entity": displayEntity(at, c.Address),
					"entity_type": at.Label.Type, "amount": formatUnits(c.Raw, asset.Decimals), "transfers": c.Transfers, "evidence_txs": c.TxAboveDust}
			}
			if hit, ok := m.firstTypeIn(cl, sanctionsTypes); ok {
				// The flag names the sanctions row itself, even when another
				// type of the same address wins the attribution.
				named := attribution{Label: hit, Entity: m.entityKey(hit.Row.LabelType, hit.Row.Value)}
				rep.Flags = append(rep.Flags, flagOut{Type: "direct_sanctions", EvidenceTxs: c.TxAboveDust, Detail: map[string]any{
					"counterparty": c.Address, "direction": c.Dir.String(), "entity": displayEntity(named, c.Address), "entity_type": hit.Type,
					"label_value": hit.Row.Value, "label_source": hit.Row.Source, "label_source_class": m.sourceClass(hit.Row.Source),
					// When the label was recorded, beside the transfer times: a transfer
					// before a designation is not a breach, and the labels source holds
					// the insert time, not the designation date.
					"label_recorded_at": hit.Row.RecordedAt,
					"amount":            formatUnits(c.Raw, asset.Decimals), "transfers": c.Transfers, "first": c.First, "last": c.Last,
				}})
			}
		} else if maxWeight(cl, req.weights) >= 100 {
			rep.Flags = append(rep.Flags, flagOut{Type: "risky_dust", EvidenceTxs: c.Tx, Detail: map[string]any{
				"counterparty": c.Address, "direction": c.Dir.String(), "entity": displayEntity(at, c.Address), "entity_type": at.Label.Type,
				"amount": formatUnits(c.Raw, asset.Decimals), "transfers": c.Transfers,
			}})
		}
		if c.RawAboveMin.Sign() == 0 {
			continue
		}
		var ev []string
		if tx := firstOf(c.TxAboveDust, c.Tx); tx != "" {
			ev = []string{tx}
		}
		addConn(at, cl, c.Address, c.Dir, c.RawAboveMin, 1, ev)
	}

	// View all: the search beyond hop 1.
	if req.view == "all" {
		srch := newSearch()
		srch.m, srch.ex, srch.req, srch.asset, srch.target = m, ex, req, asset, req.address
		srch.minRaw, srch.maxHops = minRaw, req.maxHops
		srch.branchMin = usdRaw(th.BranchMinUSD, price, asset.Decimals)
		srch.classOf, srch.attribute = classOf, addConn
		srch.loadLabels = func(ctx context.Context, addrs []string) error {
			var todo []string
			for _, a := range addrs {
				if !fetched[a] {
					todo = append(todo, a)
				}
			}
			if len(todo) == 0 {
				return nil
			}
			got, err := ex.labelRows(ctx, req.chain, todo)
			if err != nil {
				return err
			}
			for a, rows := range got {
				labels[a] = rows
			}
			for _, a := range todo {
				fetched[a] = true
			}
			return nil
		}
		sctx, cancel := searchContext(ctx, th.DeadlineSeconds)
		err := srch.run(sctx, hop1)
		cancel()
		if err != nil {
			return nil, err
		}
		rep.Params.MaxHopsReached = srch.maxReached
		unnamed = srch.unnamed
		if err := reconcile(rep, srch, dirs, edges, row, listed, attributed, asset.Decimals); err != nil {
			return nil, err
		}
	}
	for _, k := range connOrder {
		c := conns[k]
		c.Received = formatUnits(c.recvRaw, asset.Decimals)
		c.Sent = formatUnits(c.sentRaw, asset.Decimals)
		rep.Connections = append(rep.Connections, *c)
	}
	rep.UnnamedConnections, rep.UnnamedMore = unnamedRows(unnamed, th.UnnamedLimit, asset.Decimals)
	sort.SliceStable(rep.Connections, func(i, j int) bool {
		ti := new(big.Int).Add(rep.Connections[i].recvRaw, rep.Connections[i].sentRaw)
		tj := new(big.Int).Add(rep.Connections[j].recvRaw, rep.Connections[j].sentRaw)
		if c := ti.Cmp(tj); c != 0 {
			return c > 0
		}
		return rep.Connections[i].Entity < rep.Connections[j].Entity
	})

	// Score and breakdown.
	totalBase, totalAttributed := new(big.Int), new(big.Int)
	for _, d := range dirs {
		base := edges.total[d].rawAboveMin
		un := new(big.Int).Sub(base, attributed[d])
		if un.Sign() < 0 {
			un = new(big.Int)
		}
		totalBase.Add(totalBase, base)
		totalAttributed.Add(totalAttributed, attributed[d])
		rep.Unattributed[d.String()] = formatUnits(un, asset.Decimals)
		da := dirAmounts{byType: map[string]float64{}}
		da.unattributed, _ = new(big.Float).SetInt(un).Float64()
		var rows []breakdownRow
		for typ, v := range byTypeRaw[d] {
			// same_entity amounts are removed from the direction's total before
			// shares are taken (so the score is that of the flow that reached other
			// entities; removing a self leg raises the remaining connections'
			// shares, by design). The breakdown keeps the whole attributed amount.
			sv := new(big.Int).Set(v)
			if se := sameEntityByType[d][typ]; se != nil {
				sv.Sub(sv, se)
			}
			f, _ := new(big.Float).SetInt(sv).Float64()
			da.byType[typ] = f
			rows = append(rows, breakdownRow{EntityType: typ, Amount: formatUnits(v, asset.Decimals), Pct: round(100*ratio(v, base), 2)})
		}
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].Pct != rows[j].Pct {
				return rows[i].Pct > rows[j].Pct
			}
			return rows[i].EntityType < rows[j].EntityType
		})
		if rows == nil {
			rows = []breakdownRow{}
		}
		rep.Breakdown[d.String()] = rows
		amounts[d] = da
	}
	rep.Summary.AttributedPct = round(100*ratio(totalAttributed, totalBase), 2)
	rep.Score = scoreOut{Formula: formulaText, WeightsUsed: req.weights.used(), DirectFloor: round(directFloor, 2), DirectFloorFrom: floorFrom}
	best := directFloor
	for _, d := range dirs {
		if s, ok := req.weights.dirScore(amounts[d], d); ok {
			v := round(s, 2)
			if d == received {
				rep.Score.ReceivedScore = &v
			} else {
				rep.Score.SentScore = &v
			}
			if s > best {
				best = s
			}
		}
	}
	rep.Score.Value = round(best, 1)
	rep.Score.TopDrivers = req.weights.topDrivers(amounts, 5)
	if rep.Score.TopDrivers == nil {
		rep.Score.TopDrivers = []driver{}
	}
	if totalAttributed.Sign() == 0 && directFloor == 0 {
		rep.Exposure = noExposure
	}

	// Risk in other assets: a labelled 1-hop counterparty that is not native dust only.
	for _, c := range other.rows {
		if !slices.Contains(dirs, c.Dir) || c.Transfers <= c.NativeDust {
			continue
		}
		cl := classOf(c.Address)
		at, ok := m.attribute(cl, req.weights, c.Dir)
		if !ok {
			continue
		}
		if f := req.weights.factor(at.Label.Type, c.Dir); f >= th.OtherAssetRiskMin {
			rep.Flags = append(rep.Flags, flagOut{Type: "other_asset_direct_risk", EvidenceTxs: c.Tx, Detail: map[string]any{
				"counterparty": c.Address, "direction": c.Dir.String(), "entity": displayEntity(at, c.Address), "entity_type": at.Label.Type,
				"weight_x_level": round(f, 2), "transfers": c.Transfers, "first": c.First,
			}})
		}
	}

	// Poisoning.
	looks := findLookalikes(poisoningInput{Target: req.address, Asset: edges.rows, Other: other.rows, DustRaw: dustRaw, CapRaw: capRaw,
		MinMatch: th.LookalikeMinSuffix, PrefixSkip: tronPrefixSkip, Decimals: asset.Decimals})
	if len(looks) > 0 {
		rep.Flags = append(rep.Flags, poisoningFlag(looks))
	}

	// Pass-through.
	first := edges.firstReceipt
	pt := passThroughRule(edges.rows, first, th.PassThroughOutShare, th.PassThroughEndBalanceShare)
	if pt.Fires {
		rep.Flags = append(rep.Flags, flagOut{Type: "pass_through", EvidenceTxs: []string{}, Detail: map[string]any{
			"first_receipt": first, "received": formatUnits(pt.Received, asset.Decimals), "sent": formatUnits(pt.Sent, asset.Decimals),
			"out_within_days": th.PassThroughDays, "out_within_pct": pt.OutEarlyPct, "end_balance": formatUnits(pt.EndBalance, asset.Decimals),
		}})
	}
	if !edges.complete() {
		rep.Params.Notes = append(rep.Params.Notes, "pass_through is measured on the counterparties returned; with a counterparty cut it can read low")
	}

	status := func(typ string) string {
		for _, f := range rep.Flags {
			if f.Type == typ {
				return "found"
			}
		}
		return "not_found"
	}
	for _, typ := range []string{"poisoning", "pass_through", "direct_sanctions", "risky_dust", "other_asset_direct_risk"} {
		rep.Params.FlagsStatus[typ] = status(typ)
	}
	rep.Params.FlagsStatus["frozen"] = "not_evaluated"
	rep.Params.Notes = append(rep.Params.Notes, "frozen is not evaluated: the stablecoin freeze table is not connected yet")

	for u := range unmapped {
		rep.Params.UnmappedTypes = append(rep.Params.UnmappedTypes, u)
	}
	sort.Strings(rep.Params.UnmappedTypes)

	// The leading summary, from the finished report.
	rep.AtAGlance = reportHead{
		Score:          rep.Score.Value,
		DataAsOf:       rep.Params.DataAsOf,
		Complete:       rep.Params.Complete,
		View:           rep.Params.View,
		MaxHopsReached: rep.Params.MaxHopsReached,
		FlagsStatus:    rep.Params.FlagsStatus,
	}
	return rep, nil
}

// lookalikeListCap bounds the look-alikes carried in the poisoning flag's
// detail. A poisoned address can have thousands; listing them all buries the
// report and blows a client's response cap. The largest by transfer count are
// kept, the rest only counted, and the trimming is on the report object itself
// so a json report is bounded too.
const lookalikeListCap = 20

// poisoningEvidenceCap bounds the flag-level evidence_txs. Each kept look-alike
// still carries its own evidence_txs, so the flag lists a few examples rather
// than repeating every look-alike's transactions.
const poisoningEvidenceCap = 5

// poisoningFlag builds the poisoning flag from the look-alikes, keeping the
// largest lookalikeListCap by transfers and counting the rest
// (lookalikes_total / lookalikes_omitted, as unnamed_more does).
func poisoningFlag(looks []lookalike) flagOut {
	total := len(looks)
	kept := slices.Clone(looks)
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].Transfers != kept[j].Transfers {
			return kept[i].Transfers > kept[j].Transfers
		}
		return kept[i].Address < kept[j].Address
	})
	omitted := 0
	if len(kept) > lookalikeListCap {
		omitted = len(kept) - lookalikeListCap
		kept = kept[:lookalikeListCap]
	}
	var ev []string
	for _, l := range kept {
		for _, tx := range l.EvidenceTxs {
			if len(ev) >= poisoningEvidenceCap {
				break
			}
			if !slices.Contains(ev, tx) {
				ev = append(ev, tx)
			}
		}
		if len(ev) >= poisoningEvidenceCap {
			break
		}
	}
	return flagOut{Type: "poisoning", EvidenceTxs: ev, Detail: map[string]any{
		"lookalikes": kept, "count": total, "lookalikes_total": total, "lookalikes_omitted": omitted,
	}}
}

const licenceUnknownType = "Exchange (licence unknown)"

func displayEntity(at attribution, address string) string {
	if at.Entity != "" {
		return at.Entity
	}
	return at.Label.Type + " (unnamed) " + address
}

// firstTypeIn returns the address's label row of the first listed type it
// carries, preferring the most specific source within that type.
func (m *method) firstTypeIn(a addressLabels, types []string) (typedLabel, bool) {
	for _, t := range types {
		var best typedLabel
		found := false
		for _, l := range a.Types {
			if l.Type == t && (!found || m.rowBefore(l, best)) {
				best, found = l, true
			}
		}
		if found {
			return best, true
		}
	}
	return typedLabel{}, false
}

func maxWeight(a addressLabels, w weights) float64 {
	best := 0.0
	for _, l := range a.Types {
		if tw, ok := w.types[l.Type]; ok && tw.Weight > best {
			best = tw.Weight
		}
	}
	return best
}

func firstOf(lists ...[]string) string {
	for _, l := range lists {
		if len(l) > 0 {
			return l[0]
		}
	}
	return ""
}

func countDir(cps []counterparty, d direction) int {
	n := 0
	for _, c := range cps {
		if c.Dir == d {
			n++
		}
	}
	return n
}

func countContacts(cs []contact, d direction) int {
	n := 0
	for _, c := range cs {
		if c.Dir == d {
			n++
		}
	}
	return n
}

func minNonEmpty(rows []assetRow, f func(assetRow) string) string {
	out := ""
	for _, r := range rows {
		if v := f(r); v != "" && (out == "" || v < out) {
			out = v
		}
	}
	return out
}

func maxNonEmpty(rows []assetRow, f func(assetRow) string) string {
	out := ""
	for _, r := range rows {
		if v := f(r); v > out {
			out = v
		}
	}
	return out
}

// unnamedRows lists the largest unnamed counterparties and sums the rest.
func unnamedRows(u map[string]*unnamedOut, limit, decimals int) ([]unnamedOut, *unnamedMore) {
	rows := make([]*unnamedOut, 0, len(u))
	for _, r := range u {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		ti := new(big.Int).Add(rows[i].recvRaw, rows[i].sentRaw)
		tj := new(big.Int).Add(rows[j].recvRaw, rows[j].sentRaw)
		if c := ti.Cmp(tj); c != 0 {
			return c > 0
		}
		if rows[i].Address != rows[j].Address {
			return rows[i].Address < rows[j].Address
		}
		return rows[i].Cut < rows[j].Cut
	})
	out := []unnamedOut{}
	var more *unnamedMore
	moreRecv, moreSent := new(big.Int), new(big.Int)
	for i, r := range rows {
		if i < limit {
			r.Received = formatUnits(r.recvRaw, decimals)
			r.Sent = formatUnits(r.sentRaw, decimals)
			out = append(out, *r)
			continue
		}
		moreRecv.Add(moreRecv, r.recvRaw)
		moreSent.Add(moreSent, r.sentRaw)
		if more == nil {
			more = &unnamedMore{}
		}
		more.Addresses++
	}
	if more != nil {
		more.Received = formatUnits(moreRecv, decimals)
		more.Sent = formatUnits(moreSent, decimals)
	}
	return out, more
}
