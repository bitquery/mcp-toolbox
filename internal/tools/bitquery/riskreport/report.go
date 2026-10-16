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
	"fmt"
	"math/big"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/googleapis/mcp-toolbox/internal/util"
)

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
	Entity           string   `json:"entity"`
	EntityType       string   `json:"entity_type"`
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
	Note         string `json:"note"`
}

type paramsOut struct {
	Method         string            `json:"method"`
	View           string            `json:"view"`
	Direction      string            `json:"direction"`
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
	GeneratedAt    string            `json:"generated_at"`
}

type reportOut struct {
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
	Flags              []flagOut                 `json:"flags"`
	Params             paramsOut                 `json:"params"`
	Cost               costOut                   `json:"cost"`
	TimingMS           int64                     `json:"timing_ms"`
}

// request is a validated call.
type request struct {
	chain     string
	address   string
	asset     tronAsset
	assetSet  bool
	direction string
	view      string
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
	if r.view != "direct" {
		if r.view == "all" {
			return r, agentErr("view all (connections beyond the first hop) is not available yet; use view direct")
		}
		return r, agentErr("view must be direct")
	}
	if f := strings.ToLower(str("format")); f != "" && f != "json" {
		if f == "csv" {
			return r, agentErr("format csv is not available yet; use format json")
		}
		return r, agentErr("format must be json")
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
	if v, ok := p["max_hops"]; ok && v != nil {
		if n, ok := toFloat(v); !ok || n < 1 {
			return r, agentErr("max_hops must be 1 or more")
		}
	}
	var perr error
	if r.w.After, perr = parseTime(str("after_time")); perr != nil {
		return r, agentErr("after_time: %s", perr.Error())
	}
	if r.w.Before, perr = parseTime(str("before_time")); perr != nil {
		return r, agentErr("before_time: %s", perr.Error())
	}
	if !r.w.After.IsZero() && !r.w.Before.IsZero() && !r.w.After.Before(r.w.Before) {
		return r, agentErr("after_time must be earlier than before_time")
	}
	override, _ := p["weights"].(map[string]any)
	w, werr := t.m.mergeWeights(override)
	if werr != nil {
		return r, util.NewAgentError(werr.Error(), nil)
	}
	r.weights = w
	return r, nil
}

// parseTime reads YYYY-MM-DD, YYYY-MM-DD HH:MM[:SS], ISO-8601 with an optional
// fraction and offset, or 10-digit unix seconds. "" is an open end.
func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if len(s) == 10 && strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) < 0 {
		var sec int64
		fmt.Sscanf(s, "%d", &sec)
		return time.Unix(sec, 0).UTC(), nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04", time.DateOnly} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
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
	return rep, nil
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
	if !req.w.After.IsZero() {
		rep.Params.Window["after_time"] = req.w.After.Format(time.RFC3339)
	}
	if !req.w.Before.IsZero() {
		rep.Params.Window["before_time"] = req.w.Before.Format(time.RFC3339)
	}

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
	price := m.assetUSD["tron"][asset.Key]
	minRaw := usdRaw(req.minUSD, price, asset.Decimals)
	dustRaw := usdRaw(th.DustUSD, price, asset.Decimals)
	capRaw := usdRaw(th.LookalikeAmountCapUSD, price, asset.Decimals)
	if price > 0 {
		rep.Params.USDConversion = fmt.Sprintf("constant: 1 %s = %g USD", asset.Symbol, price)
	} else {
		rep.Params.USDConversion = "none: no USD price for this asset, so min_amount_usd and the dust and look-alike thresholds are not applied to it"
		minRaw, capRaw = new(big.Int), new(big.Int)
		if asset.Native {
			dustRaw = big.NewInt(th.NativeDustUnits)
		}
	}

	// 2. The 1-hop counterparties, in the report asset and in every other one.
	edges, other, err := ex.counterparties(ctx, cpInput{Addr: req.address, Asset: asset, W: req.w, MinRaw: minRaw, DustRaw: dustRaw,
		NativeDust: th.NativeDustUnits, EarlyDays: th.PassThroughDays, Limit: th.CounterpartyLimit})
	if err != nil {
		return nil, err
	}
	for _, d := range []direction{received, sent} {
		if tot := edges.total[d]; tot.counterparties > int64(countDir(edges.rows, d)) {
			rep.Params.Complete = false
			rep.Params.Cuts = append(rep.Params.Cuts, cutOut{Reason: "counterparty_limit", Direction: d.String(), Counterparts: tot.counterparties - int64(countDir(edges.rows, d)),
				Note: "the smallest counterparties were not looked up; their amounts are counted as unattributed"})
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
	if !req.w.open() {
		rep.Params.Notes = append(rep.Params.Notes, "balance is over the whole history; received, sent, tx_count and activity are in the window")
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
	conns := map[string]*connectionOut{}
	var connOrder []string
	directFloor := 0.0
	var floorFrom map[string]any
	unnamed := map[string]*unnamedOut{}
	sanctionsTypes := th.DirectSanctionsTypes
	for _, d := range dirs {
		attributed[d] = new(big.Int)
		byTypeRaw[d] = map[string]*big.Int{}
	}
	for _, c := range edges.rows {
		if !slices.Contains(dirs, c.Dir) {
			continue
		}
		cl := classOf(c.Address)
		at, ok := m.attribute(cl, req.weights, c.Dir)
		if !ok {
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
			if f := req.weights.factor(at.Label.Type, c.Dir); f > directFloor {
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
		attributed[c.Dir].Add(attributed[c.Dir], c.RawAboveMin)
		if byTypeRaw[c.Dir][at.Label.Type] == nil {
			byTypeRaw[c.Dir][at.Label.Type] = new(big.Int)
		}
		byTypeRaw[c.Dir][at.Label.Type].Add(byTypeRaw[c.Dir][at.Label.Type], c.RawAboveMin)

		name := displayEntity(at, c.Address)
		key := at.Label.Type + "\x00" + name
		conn, ok := conns[key]
		if !ok {
			conn = &connectionOut{Entity: name, EntityType: at.Label.Type, Subtype: at.Label.Subtype, LabelValue: at.Label.Row.Value,
				LabelSource: at.Label.Row.Source, LabelSourceClass: m.sourceClass(at.Label.Row.Source), LabelRecordedAt: at.Label.Row.RecordedAt,
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
		if !slices.Contains(conn.Addresses, c.Address) {
			conn.Addresses = append(conn.Addresses, c.Address)
		}
		if c.Dir == received {
			conn.recvRaw.Add(conn.recvRaw, c.RawAboveMin)
			conn.ReceivedHops = intPtr(1)
		} else {
			conn.sentRaw.Add(conn.sentRaw, c.RawAboveMin)
			conn.SentHops = intPtr(1)
		}
		if ev := firstOf(c.TxAboveDust, c.Tx); ev != "" && len(conn.EvidencePath) == 0 {
			conn.EvidencePath = []string{ev}
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
			f, _ := new(big.Float).SetInt(v).Float64()
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
		MinMatch: th.LookalikeMinSuffix, Decimals: asset.Decimals})
	if len(looks) > 0 {
		var ev []string
		for _, l := range looks {
			for _, tx := range l.EvidenceTxs {
				if len(ev) < 50 && !slices.Contains(ev, tx) {
					ev = append(ev, tx)
				}
			}
		}
		rep.Flags = append(rep.Flags, flagOut{Type: "poisoning", EvidenceTxs: ev, Detail: map[string]any{"lookalikes": looks, "count": len(looks)}})
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
	return rep, nil
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
		return rows[i].Address < rows[j].Address
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
