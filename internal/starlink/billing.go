package starlink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
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
	// PendingProductID is a plan change scheduled for the next billing
	// cycle (roster "delayedProductId"), or "" when none is pending.
	PendingProductID string
	// StandbyPending reports a scheduled switch into standby mode.
	StandbyPending bool
}

// ProductSummary describes one plan product (the current plan or a change
// option). Price is the customer-facing recurring monthly price.
type ProductSummary struct {
	ProductID   string  `json:"productId"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Price       float64 `json:"price"`
	Currency    string  `json:"currency"`
	IsStandby   bool    `json:"isStandby"`
}

// ChangeOption is one available plan change with its price variants. A
// switch usually has a scheduled variant (effective at the next billing
// cycle) and an immediate variant (effective now, with a prorated
// one-time charge); some options only offer one of the two.
type ChangeOption struct {
	Product ProductSummary `json:"product"`
	// IsCurrent marks the line's current product, which the account API
	// lists among the options; it is not a change and is never applied.
	IsCurrent bool `json:"isCurrent"`
	// HasImmediate/HasScheduled report which variants exist. Standby Mode,
	// for example, only offers a scheduled (next billing cycle) variant.
	HasImmediate bool `json:"hasImmediate"`
	HasScheduled bool `json:"hasScheduled"`

	ScheduledEffective time.Time `json:"scheduledEffective,omitzero"`
	ScheduledRecurring float64   `json:"scheduledRecurring"`
	ImmediateOneTime   float64   `json:"immediateOneTime"`
	ImmediateRecurring float64   `json:"immediateRecurring"`
}

// Schedule reports how the option must be applied: false (immediate) when
// an immediate variant exists, true (next billing cycle) when only a
// scheduled variant is offered. ok is false when neither exists.
func (o *ChangeOption) Schedule() (schedule, ok bool) {
	switch {
	case o.HasImmediate:
		return false, true
	case o.HasScheduled:
		return true, true
	}
	return false, false
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
	return b.FreshChangeOptions(ctx, serviceLine)
}

// FreshChangeOptions bypasses the cache (and refreshes it). Mutations
// validate against these live options, so an option withdrawn or repriced
// within the cache TTL cannot be applied from a stale list.
func (b *Billing) FreshChangeOptions(ctx context.Context, serviceLine string) (*ChangeOptions, error) {
	opts, err := b.fetchChangeOptions(ctx, serviceLine)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.cache[serviceLine] = billingCacheEntry{options: opts, fetchedAt: time.Now()}
	b.mu.Unlock()
	return opts, nil
}

// Invalidate drops the cached options of a service line (after a
// mutation the current plan and the offered options change).
func (b *Billing) Invalidate(serviceLine string) {
	b.mu.Lock()
	delete(b.cache, serviceLine)
	b.mu.Unlock()
}

func (b *Billing) fetchChangeOptions(ctx context.Context, serviceLine string) (*ChangeOptions, error) {
	u := fmt.Sprintf("%s/webagg/v1/public/subscriptions/change-options/%s", b.apiBase, url.PathEscape(serviceLine))
	_, data, err := b.session.request(ctx, "GET", u, nil, "")
	if err != nil {
		return nil, fmt.Errorf("fetching change options for %s: %w", serviceLine, err)
	}
	if err := checkWebaggEnvelope(data); err != nil {
		return nil, fmt.Errorf("change options for %s: %w", serviceLine, err)
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
// charge; with schedule=true it applies at the next billing cycle. The
// returned effective date is taken from the API response when it reports
// one; nil means the date is unconfirmed.
func (b *Billing) UpdatePlan(ctx context.Context, subRefID, productID string, schedule bool) (*time.Time, error) {
	if subRefID == "" || productID == "" {
		return nil, fmt.Errorf("plan change needs a subscription reference and a product")
	}
	u := fmt.Sprintf("%s/webagg/v1/public/subscriptions/line/%s/product/%s/update?schedule=%t",
		b.apiBase, url.PathEscape(subRefID), url.PathEscape(productID), schedule)
	resp, data, err := b.session.request(ctx, "POST", u, []byte("{}"), "application/json")
	if err != nil {
		b.logEvent("billing plan change failed", "subscription", subRefID, "plan", productID, "scheduled", schedule, "err", err)
		return nil, err
	}
	// Only a plain 200 confirms the mutation: redirects (e.g. an
	// expired-cookie SSO redirect) and other non-200s are failures.
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("plan change answered HTTP %d (body: %s)", resp.StatusCode, truncateForLog(data))
		b.logEvent("billing plan change failed", "subscription", subRefID, "plan", productID, "scheduled", schedule, "err", err)
		return nil, err
	}
	if err := checkMutationEnvelope(data); err != nil {
		b.logEvent("billing plan change rejected by the account API", "subscription", subRefID, "plan", productID, "scheduled", schedule, "err", err)
		return nil, err
	}
	effective := parseUpdateEffectiveDate(data)
	b.logEvent("billing plan change applied", "subscription", subRefID, "plan", productID, "scheduled", schedule)
	return effective, nil
}

// Resume resumes a paused subscription line. Like UpdatePlan it only
// accepts an HTTP 200 with a valid webagg envelope as confirmation.
func (b *Billing) Resume(ctx context.Context, subRefID string) error {
	if subRefID == "" {
		return fmt.Errorf("resume needs a subscription reference")
	}
	u := fmt.Sprintf("%s/webagg/v1/public/subscriptions/line/%s/resume", b.apiBase, url.PathEscape(subRefID))
	resp, data, err := b.session.request(ctx, "PUT", u, nil, "")
	if err != nil {
		b.logEvent("billing resume failed", "subscription", subRefID, "err", err)
		return err
	}
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("resume answered HTTP %d (body: %s)", resp.StatusCode, truncateForLog(data))
		b.logEvent("billing resume failed", "subscription", subRefID, "err", err)
		return err
	}
	if err := checkMutationEnvelope(data); err != nil {
		b.logEvent("billing resume rejected by the account API", "subscription", subRefID, "err", err)
		return err
	}
	b.logEvent("billing resume applied", "subscription", subRefID)
	return nil
}

// checkMutationEnvelope is the strict variant used for mutations: a
// non-empty body must be a JSON object (an HTML login page reached through
// a followed redirect, or any other non-JSON payload, is a failure) and
// must satisfy the webagg envelope rules. An empty body is accepted —
// some endpoints answer bare 200s.
func checkMutationEnvelope(data []byte) error {
	body := bytes.TrimSpace(data)
	if len(body) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return fmt.Errorf("mutation response is not JSON (body: %s)", truncateForLog(data))
	}
	return checkWebaggEnvelope(data)
}

// checkWebaggEnvelope validates a webagg response body: the account API
// wraps results in {"errors":[...], "isValid":bool, "content":...} and a
// 200 status alone does not prove success — a body can carry business
// errors or isValid=false. An empty body is accepted (some endpoints
// answer bare 200s); anything parsed must not carry errors.
func checkWebaggEnvelope(data []byte) error {
	body := bytes.TrimSpace(data)
	if len(body) == 0 {
		return nil
	}
	var env struct {
		Errors  []json.RawMessage `json:"errors"`
		IsValid *bool             `json:"isValid"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		// Not a webagg envelope; the caller's own parsing decides.
		return nil
	}
	if len(env.Errors) > 0 {
		return fmt.Errorf("account API reported an error: %s", firstWebaggError(env.Errors))
	}
	if env.IsValid != nil && !*env.IsValid {
		return errors.New("account API answered with isValid=false")
	}
	return nil
}

// firstWebaggError renders the first error entry, tolerating both object
// errors ({code, message/description}) and plain strings.
func firstWebaggError(errs []json.RawMessage) string {
	for _, raw := range errs {
		var obj struct {
			Message     string `json:"message"`
			Description string `json:"description"`
			Code        string `json:"code"`
		}
		if err := json.Unmarshal(raw, &obj); err == nil {
			switch {
			case obj.Message != "":
				return obj.Message
			case obj.Description != "":
				return obj.Description
			case obj.Code != "":
				return "code " + obj.Code
			}
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && s != "" {
			return s
		}
	}
	return "unknown error"
}

// parseUpdateEffectiveDate scans the update response content for an
// effective timestamp so the UI can report the API-confirmed date instead
// of the date from the pre-mutation option list. Returns nil when the
// response does not confirm a date.
func parseUpdateEffectiveDate(data []byte) *time.Time {
	var env struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &env); err != nil || len(env.Content) == 0 {
		return nil
	}
	var found time.Time
	scanEffectiveDate(bytes.TrimSpace(env.Content), &found, 0)
	if found.IsZero() {
		return nil
	}
	return &found
}

// scanEffectiveDate walks a decoded JSON structure looking for an
// "effectiveTimestamp" string; depth-bounded to stay cheap.
func scanEffectiveDate(raw []byte, found *time.Time, depth int) {
	if !found.IsZero() || depth > 6 {
		return
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err == nil {
		if tsRaw, ok := m["effectiveTimestamp"]; ok {
			var ts string
			if err := json.Unmarshal(tsRaw, &ts); err == nil && ts != "" {
				if t, err := time.Parse(time.RFC3339, ts); err == nil {
					*found = t
					return
				}
			}
		}
		for _, v := range m {
			scanEffectiveDate(v, found, depth+1)
		}
		return
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		for _, v := range arr {
			scanEffectiveDate(v, found, depth+1)
		}
	}
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
			PendingProduct any              `json:"pendingProduct"`
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
	// pendingProduct is null, a product block, or (per the app) a bare
	// product id; tolerate all three instead of failing the whole response.
	switch pp := env.Content.PendingProduct.(type) {
	case map[string]any:
		p := parseProductSummary(pp)
		out.Pending = &p
	case string:
		if pp != "" {
			out.Pending = &ProductSummary{ProductID: pp, Description: pp}
		}
	}
	for _, raw := range env.Content.ChangeOptions {
		opt := parseChangeOption(raw)
		if opt.Product.ProductID == "" {
			continue // unusable: cannot be addressed or validated
		}
		opt.IsCurrent = out.Current.ProductID != "" && opt.Product.ProductID == out.Current.ProductID
		out.Options = append(out.Options, opt)
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
			if !out.HasScheduled {
				out.HasScheduled = true
				out.ScheduledEffective = effective
				out.ScheduledRecurring = recurring
			}
		} else if !out.HasImmediate {
			out.HasImmediate = true
			out.ImmediateOneTime = oneTime
			out.ImmediateRecurring = recurring
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
		PendingProductID:   mapString(m, "delayedProductId"),
		StandbyPending:     mapBool(m, "isStandbyPending"),
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
