package starlink

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Synthetic fixtures shaped like the live account API responses (verified
// via read-only GETs); all identifiers and prices are made up.

const changeOptionsFixture = `{"content":{
  "currentProduct":{"productId":"de-test-subscription","name":"Test Residential (contract)","description":"Test Residential","price":49.0,"customerFacingPrice":50,"priceTaxExclusive":49.0,"isoCurrencyCode":"EUR","isStandby":false},
  "pendingProduct":null,
  "changeOptions":[
    {"productResponse":{
      "productId":"de-test-plan-100","name":"Test 100 Mbps","description":"Test 100 Mbps","price":35.0,"customerFacingPrice":35,"isoCurrencyCode":"EUR","isStandby":false},
     "variants":[
      {"effectiveTimestamp":"2026-12-13T00:00:00+00:00","oneTimeAmount":0,"recurringAmount":35},
      {"effectiveTimestamp":null,"oneTimeAmount":8.66,"recurringAmount":35}
    ]},
    {"productResponse":{
      "productId":"de-test-standby","name":"Standby Mode","description":"Standby Mode","price":5.0,"customerFacingPrice":5,"isoCurrencyCode":"EUR","isStandby":true},
     "variants":[
      {"effectiveTimestamp":"2026-12-13T00:00:00+00:00","oneTimeAmount":0,"recurringAmount":5}
    ]},
    {"productResponse":{
      "productId":"de-test-subscription","description":"Test Residential","customerFacingPrice":50,"isoCurrencyCode":"EUR","isStandby":false},
     "variants":[
      {"effectiveTimestamp":null,"oneTimeAmount":0,"recurringAmount":50}
    ]},
    {"productResponse":{"description":"no id"},"variants":[]},
    {"productResponse":{
      "productId":"de-test-plan-200","name":"Test 200 Mbps","description":"Test 200 Mbps","price":55.0,"customerFacingPrice":55,"isoCurrencyCode":"EUR","isStandby":false},
     "variants":null}
  ]
},"isValid":true}`

func TestParseChangeOptions(t *testing.T) {
	opts, err := parseChangeOptions([]byte(changeOptionsFixture))
	if err != nil {
		t.Fatalf("parseChangeOptions: %v", err)
	}
	if opts.Current.ProductID != "de-test-subscription" || opts.Current.Description != "Test Residential" {
		t.Errorf("current product wrong: %+v", opts.Current)
	}
	if opts.Current.Price != 50 || opts.Current.Currency != "EUR" {
		t.Errorf("current price/currency wrong: %+v", opts.Current)
	}
	if opts.Pending != nil {
		t.Errorf("pending should be nil, got %+v", opts.Pending)
	}
	// The id-less option is dropped.
	if len(opts.Options) != 4 {
		t.Fatalf("want 4 options, got %d", len(opts.Options))
	}

	first := opts.Options[0]
	if first.Product.ProductID != "de-test-plan-100" {
		t.Errorf("option 1 product: %+v", first.Product)
	}
	if want := time.Date(2026, 12, 13, 0, 0, 0, 0, time.UTC); !first.ScheduledEffective.Equal(want) {
		t.Errorf("scheduled effective = %v, want %v", first.ScheduledEffective, want)
	}
	if first.ScheduledRecurring != 35 || first.ImmediateOneTime != 8.66 || first.ImmediateRecurring != 35 {
		t.Errorf("option 1 prices wrong: %+v", first)
	}

	if sched, ok := first.Schedule(); !ok || sched {
		t.Errorf("option with immediate variant must apply immediately: sched=%v ok=%v", sched, ok)
	}

	// Standby Mode (as on the live API) only offers a scheduled variant.
	standby := opts.Options[1]
	if !standby.Product.IsStandby || standby.HasImmediate || !standby.HasScheduled || standby.ScheduledRecurring != 5 {
		t.Errorf("standby option wrong: %+v", standby)
	}
	if sched, ok := standby.Schedule(); !ok || !sched {
		t.Errorf("scheduled-only option must be scheduled: sched=%v ok=%v", sched, ok)
	}

	// The current product is listed among the options; mark it.
	if cur := opts.Options[2]; !cur.IsCurrent || first.IsCurrent || standby.IsCurrent {
		t.Errorf("IsCurrent wrong: %+v", opts.Options)
	}

	// Option without variants must still parse with a usable product.
	noVariants := opts.Options[3]
	if noVariants.Product.ProductID != "de-test-plan-200" || noVariants.Product.Price != 55 {
		t.Errorf("variant-less option wrong: %+v", noVariants)
	}
	if !noVariants.ScheduledEffective.IsZero() {
		t.Errorf("variant-less option should have no scheduled variant: %+v", noVariants)
	}
	if _, ok := noVariants.Schedule(); ok {
		t.Error("variant-less option must not be applicable")
	}
}

func TestParseChangeOptionsPendingShapes(t *testing.T) {
	opts, err := parseChangeOptions([]byte(`{"content":{"currentProduct":{"productId":"a"},"pendingProduct":"de-next"}}`))
	if err != nil || opts.Pending == nil || opts.Pending.ProductID != "de-next" {
		t.Errorf("string pendingProduct: %+v %v", opts, err)
	}
	opts, err = parseChangeOptions([]byte(`{"content":{"currentProduct":{"productId":"a"},"pendingProduct":{"productId":"de-next","description":"Next"}}}`))
	if err != nil || opts.Pending == nil || opts.Pending.Description != "Next" {
		t.Errorf("object pendingProduct: %+v %v", opts, err)
	}
}

func TestParseChangeOptionsMalformed(t *testing.T) {
	if _, err := parseChangeOptions([]byte(`{"content":{}}`)); err == nil {
		t.Error("missing currentProduct should error")
	}
	if _, err := parseChangeOptions([]byte(`not json`)); err == nil {
		t.Error("non-JSON should error")
	}
	// Empty options array is valid.
	opts, err := parseChangeOptions([]byte(`{"content":{"currentProduct":{"productId":"x"},"changeOptions":[]}}`))
	if err != nil || len(opts.Options) != 0 {
		t.Errorf("empty options should parse: %v %v", opts, err)
	}
}

const subscriptionFixture = `{
  "subscriptionReferenceId": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
  "productId": "de-test-subscription",
  "productDescription": "Test Residential",
  "startDate": "2024-06-29T02:11:45.006694+00:00",
  "endDate": null,
  "subscriptionClass": "Standard",
  "active": true,
  "canChangeService": true,
  "canResumeService": false,
  "pendingActivation": false,
  "isPaused": false,
  "isSuspended": false,
  "isStandby": false
}`

func TestParseSubscription(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal([]byte(subscriptionFixture), &m); err != nil {
		t.Fatal(err)
	}
	sub := parseSubscription(m)
	if sub == nil {
		t.Fatal("nil subscription")
	}
	if sub.ReferenceID != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Errorf("reference id: %q", sub.ReferenceID)
	}
	if sub.ProductID != "de-test-subscription" || sub.ProductDescription != "Test Residential" {
		t.Errorf("product: %+v", sub)
	}
	wantStart := time.Date(2024, 6, 29, 2, 11, 45, int(0.006694*1e9), time.UTC)
	if !sub.StartDate.Equal(wantStart) {
		t.Errorf("start date = %v, want %v (fractional seconds must parse)", sub.StartDate, wantStart)
	}
	if !sub.EndDate.IsZero() {
		t.Errorf("null endDate must parse as zero, got %v", sub.EndDate)
	}
	if !sub.Active || sub.IsPaused || sub.IsSuspended || sub.IsStandby || sub.PendingActivation {
		t.Errorf("flags wrong: %+v", sub)
	}
	if !sub.CanChange || sub.CanResume {
		t.Errorf("capability flags wrong: %+v", sub)
	}
}

func TestParseSubscriptionNil(t *testing.T) {
	if parseSubscription(nil) != nil {
		t.Error("nil map must yield nil subscription")
	}
}

func TestParseServiceLineSubscription(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal([]byte(`{
		"serviceLineNumber":"SL-TEST-9","nickname":"Test Dach","canPauseService":true,
		"subscription":`+subscriptionFixture+`
	}`), &m); err != nil {
		t.Fatal(err)
	}
	sl := parseServiceLine(m)
	if !sl.CanPauseService {
		t.Error("canPauseService not parsed")
	}
	if sl.Subscription == nil || sl.Subscription.ReferenceID != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Errorf("subscription not attached: %+v", sl.Subscription)
	}
	// A line without a subscription block keeps nil.
	var bare map[string]any
	_ = json.Unmarshal([]byte(`{"serviceLineNumber":"SL-TEST-10"}`), &bare)
	sl2 := parseServiceLine(bare)
	if sl2.Subscription != nil || sl2.CanPauseService {
		t.Errorf("missing fields should stay zero: %+v", sl2)
	}
}

func TestMapFloatOrSanitizes(t *testing.T) {
	m := map[string]any{"a": "NaN", "b": 35.5, "c": "12.5"}
	if got := mapFloatOr(m, "a"); got != 0 {
		t.Errorf("NaN string should be 0, got %v", got)
	}
	if got := mapFloatOr(m, "missing", "b"); got != 35.5 {
		t.Errorf("fallback: got %v", got)
	}
	if got := mapFloatOr(m, "c"); got != 12.5 {
		t.Errorf("string number: got %v", got)
	}
	if got := mapFloatOr(m); got != 0 {
		t.Errorf("absent: got %v", got)
	}
}

// End-to-end: GetChangeOptions through the fake API, including the 5-minute
// cache (a second call must not hit the server again).
func TestBillingGetChangeOptionsCached(t *testing.T) {
	f, ts := newFakeAPI(t)
	f.changeOptionsBody = changeOptionsFixture
	s := newTestSession(t, ts, &fakeClock{now: time.Now()})
	b := NewBilling(s, ts.URL, testLogger(t))
	ctx := context.Background()

	opts, err := b.GetChangeOptions(ctx, "SL-TEST-1")
	if err != nil {
		t.Fatalf("GetChangeOptions: %v", err)
	}
	if opts.ServiceLine != "SL-TEST-1" {
		t.Errorf("service line not stamped: %q", opts.ServiceLine)
	}
	if len(opts.Options) != 4 {
		t.Errorf("options: %d", len(opts.Options))
	}
	opts2, err := b.GetChangeOptions(ctx, "SL-TEST-1")
	if err != nil {
		t.Fatalf("second GetChangeOptions: %v", err)
	}
	if !reflect.DeepEqual(opts, opts2) {
		t.Error("cached options differ")
	}
	f.mu.Lock()
	count := f.changeOptionsCount
	f.mu.Unlock()
	if count != 1 {
		t.Errorf("change-options endpoint hit %d times within TTL, want 1", count)
	}
}

func TestBillingGetChangeOptionsError(t *testing.T) {
	f, ts := newFakeAPI(t)
	f.changeOptionsStatus = 403
	f.changeOptionsBody = `{"error":"forbidden"}`
	s := newTestSession(t, ts, &fakeClock{now: time.Now()})
	b := NewBilling(s, ts.URL, testLogger(t))
	if _, err := b.GetChangeOptions(context.Background(), "SL-TEST-1"); err == nil {
		t.Error("403 must surface as error")
	} else if !strings.Contains(err.Error(), "SL-TEST-1") {
		t.Errorf("error should name the service line: %v", err)
	}
}

// Mutations must reach the verified endpoint shapes.
func TestBillingMutations(t *testing.T) {
	_, ts := newFakeAPI(t)
	s := newTestSession(t, ts, &fakeClock{now: time.Now()})
	b := NewBilling(s, ts.URL, testLogger(t))
	ctx := context.Background()

	if _, err := b.UpdatePlan(ctx, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "de-test-plan-100", false); err != nil {
		t.Fatalf("UpdatePlan: %v", err)
	}
	if _, err := b.UpdatePlan(ctx, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "de-test-standby", true); err != nil {
		t.Fatalf("UpdatePlan scheduled: %v", err)
	}
	if err := b.Resume(ctx, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	// Errors on mutations surface (fake returns 200; use a failing session
	// path by pointing at a dead URL).
	dead := NewBilling(s, "http://127.0.0.1:1", testLogger(t))
	if _, err := dead.UpdatePlan(ctx, "ref", "plan", false); err == nil {
		t.Error("UpdatePlan against unreachable host must error")
	}
	if err := dead.Resume(ctx, "ref"); err == nil {
		t.Error("Resume against unreachable host must error")
	}
}

func TestBillingMutationPaths(t *testing.T) {
	f, ts := newFakeAPI(t)
	s := newTestSession(t, ts, &fakeClock{now: time.Now()})
	b := NewBilling(s, ts.URL, testLogger(t))
	ctx := context.Background()
	_, _ = b.UpdatePlan(ctx, "sub-1", "plan-a", false)
	_, _ = b.UpdatePlan(ctx, "sub-1", "plan-b", true)
	_ = b.Resume(ctx, "sub-1")

	f.mu.Lock()
	got := append([]string(nil), f.mutations...)
	f.mu.Unlock()
	want := []string{
		"POST /webagg/v1/public/subscriptions/line/sub-1/product/plan-a/update?schedule=false",
		"POST /webagg/v1/public/subscriptions/line/sub-1/product/plan-b/update?schedule=true",
		"PUT /webagg/v1/public/subscriptions/line/sub-1/resume",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mutation paths\n got: %v\nwant: %v", got, want)
	}
}

// Identifiers are path-escaped so they cannot rewrite the upstream path,
// and empty identifiers never reach the API.
func TestBillingEscapesIdentifiers(t *testing.T) {
	f, ts := newFakeAPI(t)
	s := newTestSession(t, ts, &fakeClock{now: time.Now()})
	b := NewBilling(s, ts.URL, testLogger(t))
	ctx := context.Background()
	_, _ = b.UpdatePlan(ctx, "../x", "a/b?c", false)
	if _, err := b.UpdatePlan(ctx, "", "plan", false); err == nil {
		t.Error("empty subscription reference must error")
	}
	if _, err := b.UpdatePlan(ctx, "sub", "", false); err == nil {
		t.Error("empty product must error")
	}
	if err := b.Resume(ctx, ""); err == nil {
		t.Error("empty resume reference must error")
	}
	f.mu.Lock()
	got := append([]string(nil), f.mutations...)
	f.mu.Unlock()
	want := []string{"POST /webagg/v1/public/subscriptions/line/..%2Fx/product/a%2Fb%3Fc/update?schedule=false"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("escaped mutation paths\n got: %v\nwant: %v", got, want)
	}
}

func TestBillingFreshBypassesCache(t *testing.T) {
	f, ts := newFakeAPI(t)
	f.changeOptionsBody = changeOptionsFixture
	s := newTestSession(t, ts, &fakeClock{now: time.Now()})
	b := NewBilling(s, ts.URL, testLogger(t))
	ctx := context.Background()
	for _, fn := range []func(context.Context, string) (*ChangeOptions, error){b.GetChangeOptions, b.FreshChangeOptions, b.GetChangeOptions} {
		if _, err := fn(ctx, "SL-TEST-1"); err != nil {
			t.Fatal(err)
		}
	}
	b.Invalidate("SL-TEST-1")
	if _, err := b.GetChangeOptions(ctx, "SL-TEST-1"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	count := f.changeOptionsCount
	f.mu.Unlock()
	if count != 3 {
		t.Errorf("hits = %d, want 3 (initial, fresh, after invalidate)", count)
	}
}

// ---------- response interpretation ----------

// A mutation must succeed only on HTTP 200 with a sane body. The fake
// mirrors the expired-cookie SSO redirect: 302 to a login page; the HTTP
// client follows it and lands on a 200 HTML page, which must NOT be
// reported as an applied mutation.
func TestBillingMutationRedirectIsFailure(t *testing.T) {
	f, ts := newFakeAPI(t)
	f.mutationStatus = 302
	f.mutationLoc = "/login"
	s := newTestSession(t, ts, &fakeClock{now: time.Now()})
	b := NewBilling(s, ts.URL, testLogger(t))
	ctx := context.Background()

	if _, err := b.UpdatePlan(ctx, "sub-1", "plan-a", false); err == nil {
		t.Error("UpdatePlan on SSO redirect (200 HTML after following) must fail")
	} else if !strings.Contains(err.Error(), "not JSON") && !strings.Contains(err.Error(), "HTTP 302") && !strings.Contains(err.Error(), "HTTP 200") {
		t.Errorf("redirect failure should name the cause, got: %v", err)
	}
	f.mu.Lock()
	errs := f.resumeCount + f.changeCount
	f.mu.Unlock()
	if errs < 1 {
		t.Error("mutation must have been attempted")
	}
	if err := b.Resume(ctx, "sub-1"); err == nil {
		t.Error("Resume on SSO redirect must fail")
	}
}

// A 200 envelope carrying business errors or isValid=false is a rejected
// mutation, not a success.
func TestBillingMutationErrorEnvelopeIsFailure(t *testing.T) {
	_, ts := newFakeAPI(t)
	s := newTestSession(t, ts, &fakeClock{now: time.Now()})
	ctx := context.Background()

	cases := []string{
		`{"errors":[{"code":"ERR-1","message":"plan not changeable"}],"isValid":false,"content":null}`,
		`{"errors":[],"isValid":false,"content":{"subscription":{}}}`,
	}
	for i, body := range cases {
		f, ts := newFakeAPI(t)
		f.mutationBody = body
		b := NewBilling(s, ts.URL, testLogger(t))
		if _, err := b.UpdatePlan(ctx, "sub-1", "plan-a", false); err == nil {
			t.Errorf("case %d: UpdatePlan with error envelope must fail", i)
		} else if i == 0 && !strings.Contains(err.Error(), "plan not changeable") {
			t.Errorf("case %d: first error message should surface, got: %v", i, err)
		}
		if err := b.Resume(ctx, "sub-1"); err == nil {
			t.Errorf("case %d: Resume with error envelope must fail", i)
		}
		ts.Close()
		_ = f
	}
}

// A valid 200 envelope confirms the mutation.
func TestBillingMutationSuccessEnvelope(t *testing.T) {
	f, ts := newFakeAPI(t)
	f.mutationBody = `{"errors":[],"isValid":true,"content":{"subscription":{"subscriptionReferenceId":"sub-1","productId":"plan-a"}}}`
	s := newTestSession(t, ts, &fakeClock{now: time.Now()})
	b := NewBilling(s, ts.URL, testLogger(t))
	ctx := context.Background()
	if _, err := b.UpdatePlan(ctx, "sub-1", "plan-a", false); err != nil {
		t.Errorf("valid envelope must succeed, got %v", err)
	}
	if err := b.Resume(ctx, "sub-1"); err != nil {
		t.Errorf("valid envelope must succeed, got %v", err)
	}
}

// The update response may confirm the effective date; prefer it over the
// pre-mutation option list.
func TestParseUpdateEffectiveDate(t *testing.T) {
	if got := parseUpdateEffectiveDate([]byte(`{"content":{"subscription":{"delayedProductId":"x","effectiveTimestamp":"2027-01-01T00:00:00+00:00"}}}`)); got == nil || !got.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("nested effectiveTimestamp: got %v", got)
	}
	if got := parseUpdateEffectiveDate([]byte(`{"content":{"effectiveTimestamp":"2027-02-01T00:00:00+00:00"}}`)); got == nil {
		t.Error("top-level effectiveTimestamp should be found")
	}
	if got := parseUpdateEffectiveDate([]byte(`{"content":{"subscription":{}}}`)); got != nil {
		t.Errorf("no date: got %v, want nil", got)
	}
	if got := parseUpdateEffectiveDate([]byte(`{"content":null}`)); got != nil {
		t.Errorf("null content: got %v, want nil", got)
	}
	if got := parseUpdateEffectiveDate([]byte(`not json`)); got != nil {
		t.Errorf("non-JSON: got %v, want nil", got)
	}
}

func TestCheckWebaggEnvelope(t *testing.T) {
	if err := checkWebaggEnvelope([]byte(`{"errors":[],"isValid":true}`)); err != nil {
		t.Errorf("clean envelope: %v", err)
	}
	if err := checkWebaggEnvelope([]byte(`{"errors":[{"message":"boom"}],"isValid":true}`)); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("errors array must fail with message, got %v", err)
	}
	var bad bool
	if err := checkWebaggEnvelope([]byte(`{"isValid":false}`)); err == nil {
		t.Error("isValid=false must fail")
	} else {
		bad = err != nil
	}
	_ = bad
	if err := checkWebaggEnvelope([]byte(``)); err != nil {
		t.Errorf("empty body accepted: %v", err)
	}
	if err := checkWebaggEnvelope([]byte(`<html>not webagg</html>`)); err != nil {
		t.Errorf("non-envelope body is left to caller parsing: %v", err)
	}
	if err := checkMutationEnvelope([]byte(`<html>login page</html>`)); err == nil || !strings.Contains(err.Error(), "not JSON") {
		t.Errorf("HTML mutation body must fail, got %v", err)
	}
	if err := checkMutationEnvelope([]byte(``)); err != nil {
		t.Errorf("empty mutation body accepted: %v", err)
	}
}

// After a mutation the account-data caches must be cleared: the billing
// change-options cache of the affected line and the roster cache, exactly
// as the web mutation handler does. Both must hit the server again on the
// next read.
func TestAccountCachesClearedAfterMutation(t *testing.T) {
	f, ts := newFakeAPI(t)
	f.changeOptionsBody = changeOptionsFixture
	clock := &fakeClock{now: time.Now()}
	s := newTestSession(t, ts, clock)
	b := NewBilling(s, ts.URL, testLogger(t))
	roster := NewRoster(s, ts.URL, time.Minute, testLogger(t))
	ctx := context.Background()

	// Warm both caches.
	if _, err := b.GetChangeOptions(ctx, "SL-TEST-1"); err != nil {
		t.Fatalf("GetChangeOptions: %v", err)
	}
	if _, err := roster.Get(ctx); err != nil {
		t.Fatalf("roster.Get: %v", err)
	}
	f.mu.Lock()
	opts0, roster0 := f.changeOptionsCount, f.rosterCount
	f.mu.Unlock()
	if opts0 != 1 || roster0 != 1 {
		t.Fatalf("setup: caches not warmed (options=%d roster=%d)", opts0, roster0)
	}

	// Mutate and clear, mirroring the web mutation handler.
	if _, err := b.UpdatePlan(ctx, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "de-test-plan-100", false); err != nil {
		t.Fatalf("UpdatePlan: %v", err)
	}
	b.Invalidate("SL-TEST-1")
	roster.Invalidate()

	// Both caches must refetch immediately despite the TTL.
	if _, err := b.GetChangeOptions(ctx, "SL-TEST-1"); err != nil {
		t.Fatalf("GetChangeOptions after mutation: %v", err)
	}
	if _, err := roster.Get(ctx); err != nil {
		t.Fatalf("roster.Get after mutation: %v", err)
	}
	f.mu.Lock()
	opts1, roster1 := f.changeOptionsCount, f.rosterCount
	f.mu.Unlock()
	if opts1 != 2 {
		t.Errorf("change-options fetched %d times after mutation, want 2", opts1)
	}
	if roster1 != 2 {
		t.Errorf("roster fetched %d times after mutation, want 2", roster1)
	}

	// A resume clears too.
	if err := b.Resume(ctx, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	b.Invalidate("SL-TEST-1")
	roster.Invalidate()
	if _, err := b.GetChangeOptions(ctx, "SL-TEST-1"); err != nil {
		t.Fatalf("GetChangeOptions after resume: %v", err)
	}
	f.mu.Lock()
	opts2 := f.changeOptionsCount
	f.mu.Unlock()
	if opts2 != 3 {
		t.Errorf("change-options fetched %d times after resume, want 3", opts2)
	}
}

// Invalidate must also work for a line that was never cached.
func TestBillingInvalidateUnknownLine(t *testing.T) {
	b := NewBilling(nil, "https://api.example.test", testLogger(t))
	b.Invalidate("SL-NEVER-CACHED") // must not panic
}
