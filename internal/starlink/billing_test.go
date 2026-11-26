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
      {"effectiveTimestamp":null,"oneTimeAmount":0,"recurringAmount":5}
    ]},
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
	if len(opts.Options) != 3 {
		t.Fatalf("want 3 options, got %d", len(opts.Options))
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

	standby := opts.Options[1]
	if !standby.Product.IsStandby || standby.ScheduledEffective.IsZero() == false {
		t.Errorf("standby option wrong: %+v", standby)
	}
	if standby.ImmediateOneTime != 0 || standby.ImmediateRecurring != 5 {
		t.Errorf("standby prices wrong: %+v", standby)
	}

	// Option without variants must still parse with a usable product.
	noVariants := opts.Options[2]
	if noVariants.Product.ProductID != "de-test-plan-200" || noVariants.Product.Price != 55 {
		t.Errorf("variant-less option wrong: %+v", noVariants)
	}
	if !noVariants.ScheduledEffective.IsZero() {
		t.Errorf("variant-less option should have no scheduled variant: %+v", noVariants)
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
	if len(opts.Options) != 3 {
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

	if err := b.UpdatePlan(ctx, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "de-test-plan-100", false); err != nil {
		t.Fatalf("UpdatePlan: %v", err)
	}
	if err := b.UpdatePlan(ctx, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "de-test-standby", true); err != nil {
		t.Fatalf("UpdatePlan scheduled: %v", err)
	}
	if err := b.Resume(ctx, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	// Errors on mutations surface (fake returns 200; use a failing session
	// path by pointing at a dead URL).
	dead := NewBilling(s, "http://127.0.0.1:1", testLogger(t))
	if err := dead.UpdatePlan(ctx, "ref", "plan", false); err == nil {
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
	_ = b.UpdatePlan(ctx, "sub-1", "plan-a", false)
	_ = b.UpdatePlan(ctx, "sub-1", "plan-b", true)
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
