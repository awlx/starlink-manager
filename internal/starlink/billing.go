package starlink

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

// This file talks to the subscription/billing endpoints of the Starlink
// account API (the same API the Starlink app and website use):
//
//   GET  /webagg/v1/public/subscriptions/change-options/{serviceLine}
//   POST /webagg/v1/public/subscriptions/line/{subscriptionReferenceId}/product/{productId}/update?schedule=false
//   PUT  /webagg/v1/public/subscriptions/line/{subscriptionReferenceId}/resume
//
// All three are authenticated like every other account-API call (Bearer
// access token via the shared session). Plan changes and resumes affect
// real billing; callers must confirm explicitly before invoking them.

// Subscription mirrors the roster's per-service-line subscription block.
type Subscription struct {
	ReferenceID        string
	ProductID          string
	ProductDescription string
	StartDate          time.Time
	EndDate            time.Time
	SubscriptionClass  string
	Active             bool
	IsPaused           bool
	IsSuspended        bool
	IsStandby          bool
	PendingActivation  bool
	CanChange          bool
	CanResume          bool
}

// ProductSummary describes one plan product (the current plan or a change
// option). Price is the customer-facing recurring monthly price.
type ProductSummary struct {
	ProductID        string  `json:"productId"`
	Name             string  `json:"name"`
	Description      string  `json:"description"`
	Price            float64 `json:"price"`
	Currency         string  `json:"currency"`
	IsStandby        bool    `json:"isStandby"`
}

// ChangeOption is one available plan change with its price variants. A
// switch usually has a scheduled variant (effective at the next billing
// cycle) and an immediate variant (effective now, with a prorated
// one-time charge); some options only offer one of the two.
type ChangeOption struct {
	Product ProductSummary `json:"product"`

	ScheduledEffective time.Time `json:"scheduledEffective,omitempty"`
	ScheduledRecurring float64   `json:"scheduledRecurring,omitempty"`
	ImmediateOneTime   float64   `json:"immediateOneTime,omitempty"`
	ImmediateRecurring float64   `json:"immediateRecurring,omitempty"`
}

// ChangeOptions is the parsed change-options response for one service line.
type ChangeOptions struct {
	ServiceLine string          `json:"serviceLine"`
	Current     ProductSummary  `json:"current"`
	Pending     *ProductSummary `json:"pending,omitempty"`
	Options     []ChangeOption  `json:"options"`
}

// billingCacheTTL is how long change-options responses are cached per
// service line. The options change rarely; this also protects the account
// API from per-request scraping through the admin UI.
const billingCacheTTL = 5 * time.Minute

// Billing reads plan/subscription data and performs confirmed plan changes
// and resumes. Mutations are logged (plan, service line, result — never
// credentials).
type Billing struct {
	session *Session
	apiBase string
	log     *slog.Logger

	mu    sync.Mutex
	cache map[string]billingCacheEntry
}

type billingCacheEntry struct {
	options   *ChangeOptions
	fetchedAt time.Time
}

// NewBilling creates a billing client.
func NewBilling(session *Session, apiBase string, log *slog.Logger) *Billing {
	return &Billing{
		session: session,
		apiBase: strings.TrimRight(apiBase, "/"),
		log:     log,
		cache:   map[string]billingCacheEntry{},
	}
}

// GetChangeOptions returns the available plan change options for a service
// line, using a short-lived cache.
func (b *Billing) GetChangeOptions(ctx context.Context, serviceLine string) (*ChangeOptions, error) {
	b.mu.Lock()
	if entry, ok := b.cache[serviceLine]; ok && time.Since(entry.fetchedAt) < billingCacheTTL {
		b.mu.Unlock()
		return entry.options, nil
	}
	b.mu.Unlock()

	opts, err := b.fetchChangeOptions(ctx, serviceLine)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.cache[serviceLine] = billingCacheEntry{options: opts, fetchedAt: time.Now()}
	b.mu.Unlock()
	return opts, nil
}

func (b *Billing) fetchChangeOptions(ctx context.Context, serviceLine string) (*ChangeOptions, error) {
	u := fmt.Sprintf("%s/webagg/v1/public/subscriptions/change-options/%s", b.apiBase, serviceLine)
	_, data, err := b.session.request(ctx, "GET", u, nil, "")
	if err != nil {
		return nil, fmt.Errorf("fetching change options for %s: %w", serviceLine, err)
	}
	opts, err := parseChangeOptions(data)
	if err != nil {
		return nil, fmt.Errorf("parsing change options for %s: %w", serviceLine, err)
	}
	opts.ServiceLine = serviceLine
	return opts, nil
}

// UpdatePlan changes the plan of a subscription line. With schedule=false
// (verified endpoint form) the change applies immediately with a prorated
// charge; with schedule=true it applies at the next billing cycle.
func (b *Billing) UpdatePlan(ctx context.Context, subRefID, productID string, schedule bool) error {
	u := fmt.Sprintf("%s/webagg/v1/public/subscriptions/line/%s/product/%s/update?schedule=%t",
		b.apiBase, subRefID, productID, schedule)
	_, _, err := b.session.request(ctx, "POST", u, []byte("{}"), "application/json")
	if err != nil {
		b.logEvent("billing plan change failed", "subscription", subRefID, "plan", productID, "scheduled", schedule, "err", err)
		return err
	}
	b.logEvent("billing plan change applied", "subscription", subRefID, "plan", productID, "scheduled", schedule)
	return nil
}

// Resume resumes a paused subscription line.
func (b *Billing) Resume(ctx context.Context, subRefID string) error {
	u := fmt.Sprintf("%s/webagg/v1/public/subscriptions/line/%s/resume", b.apiBase, subRefID)
	_, _, err := b.session.request(ctx, "PUT", u, nil, "")
	if err != nil {
		b.logEvent("billing resume failed", "subscription", subRefID, "err", err)
		return err
	}
	b.logEvent("billing resume applied", "subscription", subRefID)
	return nil
}

// logEvent logs a mutation outcome, tolerating a nil logger (tests).
func (b *Billing) logEvent(event string, args ...any) {
	if b.log == nil {
		return
	}
	b.log.Info(event, args...)
}

// ---------- parsing ----------

// parseChangeOptions parses the change-options response envelope:
// {"content":{"currentProduct":{...},"pendingProduct":null|"...","changeOptions":[{"productResponse":{...},"variants":[...]}]}}.
func parseChangeOptions(data []byte) (*ChangeOptions, error) {
	var env struct {
		Content struct {
			CurrentProduct map[string]any   `json:"currentProduct"`
			PendingProduct map[string]any   `json:"pendingProduct"`
			ChangeOptions  []map[string]any `json:"changeOptions"`
		} `json:"content"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, err
	}
	if env.Content.CurrentProduct == nil {
		return nil, fmt.Errorf("change-options response has no currentProduct")
	}
	out := &ChangeOptions{
		Current: parseProductSummary(env.Content.CurrentProduct),
	}
	if env.Content.PendingProduct != nil {
		p := parseProductSummary(env.Content.PendingProduct)
		out.Pending = &p
	}
	for _, raw := range env.Content.ChangeOptions {
		out.Options = append(out.Options, parseChangeOption(raw))
	}
	return out, nil
}

// parseProductSummary reads the fields the UI needs from a product block;
// unknown or null fields are tolerated.
func parseProductSummary(m map[string]any) ProductSummary {
	p := ProductSummary{
		ProductID:   mapString(m, "productId"),
		Name:        mapString(m, "name"),
		Description: mapString(m, "description"),
		Currency:    mapString(m, "isoCurrencyCode"),
		IsStandby:   mapBool(m, "isStandby"),
	}
	p.Price = mapFloatOr(m, "customerFacingPrice", "price")
	if p.Description == "" {
		p.Description = p.Name
	}
	return p
}

// parseChangeOption reads one change option: the product plus its price
// variants. The scheduled variant carries an effectiveTimestamp (next
// billing cycle); the immediate variant does not.
func parseChangeOption(m map[string]any) ChangeOption {
	out := ChangeOption{}
	if product, ok := lookup(m, "productResponse"); ok {
		if pm, ok := product.(map[string]any); ok {
			out.Product = parseProductSummary(pm)
		}
	}
	variants, ok := lookup(m, "variants")
	if !ok {
		return out
	}
	vs, ok := variants.([]any)
	if !ok {
		return out
	}
	for _, v := range vs {
		vm, ok := v.(map[string]any)
		if !ok {
			continue
		}
		effective := mapTime(vm, "effectiveTimestamp")
		recurring := mapFloatOr(vm, "recurringAmount")
		oneTime := mapFloatOr(vm, "oneTimeAmount")
		if !effective.IsZero() {
			if out.ScheduledEffective.IsZero() {
				out.ScheduledEffective = effective
				out.ScheduledRecurring = recurring
			}
		} else {
			if out.ImmediateOneTime == 0 && out.ImmediateRecurring == 0 {
				out.ImmediateOneTime = oneTime
				out.ImmediateRecurring = recurring
			}
		}
	}
	return out
}

// parseSubscription reads the roster's per-line subscription block.
func parseSubscription(m map[string]any) *Subscription {
	if m == nil {
		return nil
	}
	sub := &Subscription{
		ReferenceID:        mapString(m, "subscriptionReferenceId"),
		ProductID:          mapString(m, "productId"),
		ProductDescription: mapString(m, "productDescription"),
		SubscriptionClass:  mapString(m, "subscriptionClass"),
		Active:             mapBool(m, "active"),
		IsPaused:           mapBool(m, "isPaused"),
		IsSuspended:        mapBool(m, "isSuspended"),
		IsStandby:          mapBool(m, "isStandby"),
		PendingActivation:  mapBool(m, "pendingActivation"),
		CanChange:          mapBool(m, "canChangeService"),
		CanResume:          mapBool(m, "canResumeService"),
		StartDate:          mapTime(m, "startDate"),
		EndDate:            mapTime(m, "endDate"),
	}
	if sub.ProductDescription == "" {
		sub.ProductDescription = sub.ProductID
	}
	return sub
}

// mapFloatOr returns the first present, finite numeric field (NaN/Inf are
// treated as missing), falling back through the alias list.
func mapFloatOr(m map[string]any, keys ...string) float64 {
	for _, key := range keys {
		v, ok := lookup(m, key)
		if !ok || v == nil {
			continue
		}
		switch val := v.(type) {
		case float64:
			if isFinite(val) {
				return val
			}
		case string:
			f, err := strconv.ParseFloat(val, 64)
			if err == nil && isFinite(f) {
				return f
			}
		}
	}
	return 0
}

func isFinite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}
