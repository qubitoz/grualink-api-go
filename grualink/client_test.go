package grualink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Obviously fake: it only has the prefix the client checks.
const testKey = "glk_test_FAKE_KEY_FOR_UNIT_TESTS_ONLY"

var testInput = ServiceRequestInput{
	ExternalRef: "TEST-CONFIRMAR-0001",
	Contact:     Contact{Name: "Persona de Ejemplo", Phone: "+525500000000"},
	Pickup:      Place{Address: "Calle Ficticia 123, Ciudad Ejemplo", Lat: Float64(20.6767), Lng: Float64(-103.3475)},
	Destination: &Place{Address: "Taller Ficticio, Avenida Ejemplo 456", Lat: Float64(20.7214), Lng: Float64(-103.3918)},
}

type harness struct {
	t       *testing.T
	api     *fakeAPI
	sleeps  []time.Duration
	retries []RetryInfo
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return &harness{t: t, api: startFakeAPI(t)}
}

// client returns a client that talks to the fake API, records its waits instead of sleeping and
// uses a fixed random source.
func (h *harness) client(options ...Option) *Client {
	h.t.Helper()
	// One connection per request: the transport never resends a request on its own.
	transport := &http.Transport{DisableKeepAlives: true}
	h.t.Cleanup(transport.CloseIdleConnections)
	base := []Option{
		WithBaseURL(h.api.BaseURL),
		WithHTTPClient(&http.Client{Transport: transport}),
		withSleep(func(_ context.Context, delay time.Duration) error {
			h.sleeps = append(h.sleeps, delay)
			return nil
		}),
		withRandom(func() float64 { return 0.5 }),
		WithOnRetry(func(info RetryInfo) { h.retries = append(h.retries, info) }),
	}
	client, err := NewClient(testKey, append(base, options...)...)
	if err != nil {
		h.t.Fatalf("NewClient: %v", err)
	}
	return client
}

func ms(values ...int) []time.Duration {
	delays := make([]time.Duration, 0, len(values))
	for _, value := range values {
		delays = append(delays, time.Duration(value)*time.Millisecond)
	}
	return delays
}

func sameDelays(a, b []time.Duration) bool {
	return len(a) == len(b) && (len(a) == 0 || reflect.DeepEqual(a, b))
}

// mustFail checks that err is an error of this package of the given kind.
func mustFail(t *testing.T, err error, kind error) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error matching %q, got none", kind)
	}
	if !errors.Is(err, ErrGrualink) {
		t.Fatalf("expected an error of this package, got %T: %v", err, err)
	}
	if !errors.Is(err, kind) {
		t.Fatalf("expected an error matching %q, got %T: %v", kind, err, err)
	}
}

func asAPIError(t *testing.T, err error) *APIError {
	t.Helper()
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected an *APIError, got %T: %v", err, err)
	}
	return apiErr
}

func attemptsOf(t *testing.T, err error) int {
	t.Helper()
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Attempts
	}
	var plain *Error
	if errors.As(err, &plain) {
		return plain.Attempts
	}
	t.Fatalf("expected an error of this package, got %T: %v", err, err)
	return 0
}

var ctx = context.Background()

// ── Success ──────────────────────────────────────────────────────────────────────────────────────

func TestCreateServiceRequestSendsTheKeyTheIdempotencyKeyAndTheBody(t *testing.T) {
	h := newHarness(t)
	h.api.reply(fakeResponse{Status: 201, JSON: sampleServiceRequest})
	result, err := h.client().CreateServiceRequest(ctx, testInput, CreateServiceRequestOptions{IdempotencyKey: "order-0001"})
	if err != nil {
		t.Fatal(err)
	}
	sr := result.ServiceRequest
	if sr.ID != sampleServiceRequestID || sr.Status != StatusQuoted || sr.Mode != "test" {
		t.Errorf("unexpected service request: %+v", sr)
	}
	if sr.ExternalRef == nil || *sr.ExternalRef != "TEST-CONFIRMAR-0001" {
		t.Errorf("unexpected external_ref: %v", sr.ExternalRef)
	}
	if sr.Quote.EstimatedPrice == nil || sr.Quote.EstimatedPrice.Total != 1002.62 || len(sr.Quote.EstimatedPrice.Taxes) != 1 {
		t.Errorf("unexpected quote: %+v", sr.Quote)
	}
	if sr.ResolvedAt != nil || sr.Service != nil || sr.Assignment != nil || sr.Timeline != nil {
		t.Errorf("null fields must stay nil: %+v", sr)
	}
	if result.Replayed || result.IdempotencyKey != "order-0001" {
		t.Errorf("replayed=%v idempotencyKey=%q", result.Replayed, result.IdempotencyKey)
	}

	requests := h.api.recorded()
	if len(requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(requests))
	}
	sent := requests[0]
	if sent.Method != "POST" || sent.Path != "/api/v1/service-requests" {
		t.Errorf("sent %s %s", sent.Method, sent.Path)
	}
	if got := sent.Header.Get("Authorization"); got != "Bearer "+testKey {
		t.Errorf("unexpected Authorization header")
	}
	if got := sent.Header.Get("Idempotency-Key"); got != "order-0001" {
		t.Errorf("Idempotency-Key = %q", got)
	}
	if got := sent.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	var body ServiceRequestInput
	if err := json.Unmarshal([]byte(sent.Body), &body); err != nil || !reflect.DeepEqual(body, testInput) {
		t.Errorf("the body does not match the input: %s (%v)", sent.Body, err)
	}
	// Wire names, and nothing for the fields that were not set.
	for _, name := range []string{`"external_ref"`, `"contact"`, `"pickup"`, `"destination"`, `"lat"`, `"lng"`} {
		if !strings.Contains(sent.Body, name) {
			t.Errorf("the body lacks %s: %s", name, sent.Body)
		}
	}
	for _, name := range []string{`"vehicle"`, `"service"`, `"notes"`, "null"} {
		if strings.Contains(sent.Body, name) {
			t.Errorf("the body must not contain %s: %s", name, sent.Body)
		}
	}
}

func TestCreateServiceRequestGeneratesAnIdempotencyKey(t *testing.T) {
	h := newHarness(t)
	h.api.reply(fakeResponse{Status: 201, JSON: sampleServiceRequest})
	c := h.client()
	first, err := c.CreateServiceRequest(ctx, testInput, CreateServiceRequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.CreateServiceRequest(ctx, testInput, CreateServiceRequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	uuidV4 := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuidV4.MatchString(first.IdempotencyKey) {
		t.Errorf("the generated key is not a UUID v4: %q", first.IdempotencyKey)
	}
	if first.IdempotencyKey == second.IdempotencyKey {
		t.Errorf("two calls generated the same key")
	}
	if got := h.api.recorded()[0].Header.Get("Idempotency-Key"); got != first.IdempotencyKey {
		t.Errorf("sent %q, reported %q", got, first.IdempotencyKey)
	}
}

func TestARepeatedRequestIsReportedAsReplayed(t *testing.T) {
	h := newHarness(t)
	h.api.reply(
		fakeResponse{Status: 201, JSON: sampleServiceRequest},
		fakeResponse{Status: 200, JSON: sampleServiceRequest, Headers: map[string]string{"Idempotent-Replayed": "true"}},
		// A 200 without the header is not a replay.
		fakeResponse{Status: 200, JSON: sampleServiceRequest},
	)
	c := h.client()
	options := CreateServiceRequestOptions{IdempotencyKey: "order-0002"}
	first, err := c.CreateServiceRequest(ctx, testInput, options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.CreateServiceRequest(ctx, testInput, options)
	if err != nil {
		t.Fatal(err)
	}
	third, err := c.CreateServiceRequest(ctx, testInput, options)
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed || !second.Replayed || third.Replayed {
		t.Errorf("replayed: first=%v second=%v third=%v", first.Replayed, second.Replayed, third.Replayed)
	}
	if !reflect.DeepEqual(first.ServiceRequest, second.ServiceRequest) {
		t.Errorf("the replay returned a different service request")
	}
	requests := h.api.recorded()
	if requests[0].Body != requests[1].Body {
		t.Errorf("the same bytes must be sent both times")
	}
}

func TestGetServiceRequestAndFindServiceRequestsByExternalRef(t *testing.T) {
	h := newHarness(t)
	h.api.reply(fakeResponse{Status: 200, JSON: sampleServiceRequest})
	one, err := h.client().GetServiceRequest(ctx, sampleServiceRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if one.Status != StatusQuoted {
		t.Errorf("status = %q", one.Status)
	}

	h.api.reply(fakeResponse{Status: 200, JSON: map[string]any{"object": "list", "data": []any{sampleServiceRequest}}})
	list, err := h.client().FindServiceRequestsByExternalRef(ctx, "SIN 2026/001 ñ")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != sampleServiceRequestID {
		t.Errorf("unexpected list: %+v", list)
	}

	h.api.reply(fakeResponse{Status: 200, JSON: map[string]any{"object": "list", "data": []any{}}})
	empty, err := h.client().FindServiceRequestsByExternalRef(ctx, "NONE")
	if err != nil || empty == nil || len(empty) != 0 {
		t.Errorf("expected an empty list, got %v (%v)", empty, err)
	}

	requests := h.api.recorded()
	if requests[0].Method != "GET" || requests[0].Path != "/api/v1/service-requests/"+sampleServiceRequestID {
		t.Errorf("sent %s %s", requests[0].Method, requests[0].Path)
	}
	if requests[1].Path != "/api/v1/service-requests?external_ref=SIN+2026%2F001+%C3%B1" {
		t.Errorf("sent %s", requests[1].Path)
	}
	if requests[0].Header.Get("Authorization") != "Bearer "+testKey {
		t.Errorf("the key was not sent")
	}
}

func TestHealthIsSentWithoutTheKey(t *testing.T) {
	h := newHarness(t)
	h.api.reply(fakeResponse{Status: 200, JSON: map[string]string{"status": "ok", "version": "v1"}})
	health, err := h.client().Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if health.Status != "ok" || health.Version != "v1" {
		t.Errorf("unexpected health: %+v", health)
	}
	sent := h.api.recorded()[0]
	if sent.Path != "/api/v1/health" {
		t.Errorf("sent %s", sent.Path)
	}
	if _, present := sent.Header["Authorization"]; present {
		t.Errorf("health must be sent without the Authorization header")
	}
}

// ── Every documented error ───────────────────────────────────────────────────────────────────────

func TestEveryDocumentedErrorIsTypedAndNotRetried(t *testing.T) {
	table := []struct {
		status int
		code   string
		kind   error
	}{
		{400, "invalid_request", ErrInvalidRequest},
		{400, "invalid_json", ErrInvalidRequest},
		{400, "key_in_query", ErrInvalidRequest},
		{400, "idempotency_key_required", ErrInvalidRequest},
		{400, "invalid_idempotency_key", ErrInvalidRequest},
		{401, "unauthorized", ErrAuthentication},
		{403, "insufficient_scope", ErrPermission},
		{403, "customer_unavailable", ErrPermission},
		{403, "live_mode_unavailable", ErrPermission},
		{403, "browser_not_allowed", ErrPermission},
		{404, "not_found", ErrNotFound},
		{405, "method_not_allowed", ErrInvalidRequest},
		{409, "idempotency_conflict", ErrIdempotencyConflict},
		{413, "payload_too_large", ErrInvalidRequest},
		{415, "unsupported_media_type", ErrInvalidRequest},
		{422, "validation_error", ErrValidation},
		{429, "too_many_open_requests", ErrRateLimit},
	}
	kinds := []error{
		ErrConfig, ErrConnection, ErrTimeout, ErrInvalidRequest, ErrAuthentication, ErrPermission, ErrNotFound,
		ErrIdempotencyConflict, ErrValidation, ErrRateLimit, ErrServer,
	}
	for _, row := range table {
		t.Run(fmt.Sprintf("%d_%s", row.status, row.code), func(t *testing.T) {
			h := newHarness(t)
			h.api.reply(apiErrorResponse(row.status, row.code, "Mensaje de ejemplo.", nil, nil))
			_, err := h.client().CreateServiceRequest(ctx, testInput, CreateServiceRequestOptions{})
			mustFail(t, err, row.kind)
			mustFail(t, err, ErrAPI)
			for _, other := range kinds {
				if other != row.kind && errors.Is(err, other) {
					t.Errorf("the error also matches %q", other)
				}
			}
			apiErr := asAPIError(t, err)
			if apiErr.Status != row.status || apiErr.Code != row.code || apiErr.Message != "Mensaje de ejemplo." {
				t.Errorf("unexpected error: %+v", apiErr)
			}
			if apiErr.Attempts != 1 || apiErr.RetryAfterSeconds != nil || len(apiErr.Details) != 0 {
				t.Errorf("unexpected error: %+v", apiErr)
			}
			if got := len(h.api.recorded()); got != 1 {
				t.Errorf("expected 1 request, got %d", got)
			}
			if len(h.sleeps) != 0 {
				t.Errorf("the client waited: %v", h.sleeps)
			}
		})
	}
}

func TestValidationErrorExposesTheInvalidFields(t *testing.T) {
	h := newHarness(t)
	details := []ErrorDetail{{Field: "contact.phone", Issue: "invalid_format"}, {Field: "pickup", Issue: "invalid_value"}}
	h.api.reply(apiErrorResponse(422, "validation_error", "Hay campos inválidos.", details, nil))
	_, err := h.client().CreateServiceRequest(ctx, testInput, CreateServiceRequestOptions{})
	mustFail(t, err, ErrValidation)
	if got := asAPIError(t, err).Details; !reflect.DeepEqual(got, details) {
		t.Errorf("details = %+v", got)
	}
	if !strings.Contains(err.Error(), "422") || !strings.Contains(err.Error(), "validation_error") {
		t.Errorf("the text of the error lacks the status or the code: %s", err)
	}
}

func TestAnErrorBodyThatIsNotJSONBecomesUnexpectedResponse(t *testing.T) {
	h := newHarness(t)
	h.api.reply(fakeResponse{Status: 404, Text: "<html>proxy page</html>"})
	_, err := h.client().GetServiceRequest(ctx, "abc")
	mustFail(t, err, ErrNotFound)
	apiErr := asAPIError(t, err)
	if apiErr.Code != CodeUnexpectedResponse {
		t.Errorf("code = %q", apiErr.Code)
	}
	if strings.Contains(err.Error(), "proxy page") {
		t.Errorf("the body was echoed: %s", err)
	}

	// The documented shape with the wrong types is not trusted either.
	h.api.reply(fakeResponse{Status: 400, JSON: map[string]any{"error": map[string]any{"code": 7, "message": "x"}}})
	_, err = h.client().GetServiceRequest(ctx, "abc")
	mustFail(t, err, ErrInvalidRequest)
	if got := asAPIError(t, err).Code; got != CodeUnexpectedResponse {
		t.Errorf("code = %q", got)
	}
}

func TestASuccessBodyThatIsNotTheExpectedJSONIsAnError(t *testing.T) {
	h := newHarness(t)
	h.api.reply(fakeResponse{Status: 200, Text: "<html>not json</html>"})
	_, err := h.client().GetServiceRequest(ctx, sampleServiceRequestID)
	mustFail(t, err, ErrAPI)
	apiErr := asAPIError(t, err)
	if apiErr.Status != 200 || apiErr.Code != CodeUnexpectedResponse || apiErr.Attempts != 1 {
		t.Errorf("unexpected error: %+v", apiErr)
	}
	if got := len(h.api.recorded()); got != 1 {
		t.Errorf("expected 1 request, got %d", got)
	}
}

func TestARedirectIsNotFollowed(t *testing.T) {
	h := newHarness(t)
	h.api.reply(fakeResponse{Status: 302, Headers: map[string]string{"Location": h.api.BaseURL + "/elsewhere"}})
	_, err := h.client().GetServiceRequest(ctx, sampleServiceRequestID)
	mustFail(t, err, ErrAPI)
	if got := asAPIError(t, err).Status; got != 302 {
		t.Errorf("status = %d", got)
	}
	// The key did not travel to the new address.
	if got := len(h.api.recorded()); got != 1 {
		t.Errorf("expected 1 request, got %d", got)
	}

	// Also when the caller brings an HTTP client that would follow it.
	h.api.reply(fakeResponse{Status: 307, Headers: map[string]string{"Location": h.api.BaseURL + "/elsewhere"}})
	own := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	_, err = h.client(WithHTTPClient(own)).CreateServiceRequest(ctx, testInput, CreateServiceRequestOptions{})
	mustFail(t, err, ErrAPI)
	if got := len(h.api.recorded()); got != 2 {
		t.Errorf("expected 2 requests, got %d", got)
	}
	if own.CheckRedirect != nil {
		t.Errorf("the HTTP client of the caller was modified")
	}
}

// ── Retries ──────────────────────────────────────────────────────────────────────────────────────

func TestRateLimitedWaitsAtLeastRetryAfterAndResendsTheSameKeyAndBody(t *testing.T) {
	h := newHarness(t)
	h.api.reply(
		apiErrorResponse(429, "rate_limited", "Demasiadas peticiones.", nil, map[string]string{"Retry-After": "7"}),
		fakeResponse{Status: 201, JSON: sampleServiceRequest},
	)
	result, err := h.client().CreateServiceRequest(ctx, testInput, CreateServiceRequestOptions{IdempotencyKey: "order-0003"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ServiceRequest.ID != sampleServiceRequestID {
		t.Errorf("id = %q", result.ServiceRequest.ID)
	}
	requests := h.api.recorded()
	if len(requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(requests))
	}
	// 7 s from the header plus half (random = 0.5) of the first backoff step (500 ms).
	if !sameDelays(h.sleeps, ms(7250)) {
		t.Errorf("sleeps = %v", h.sleeps)
	}
	if len(h.retries) != 1 || h.retries[0].Attempt != 1 || h.retries[0].Delay != 7250*time.Millisecond {
		t.Fatalf("retries = %+v", h.retries)
	}
	mustFail(t, h.retries[0].Err, ErrRateLimit)
	if seconds := asAPIError(t, h.retries[0].Err).RetryAfterSeconds; seconds == nil || *seconds != 7 {
		t.Errorf("RetryAfterSeconds = %v", seconds)
	}
	for i, request := range requests {
		if got := request.Header.Get("Idempotency-Key"); got != "order-0003" {
			t.Errorf("request %d: Idempotency-Key = %q", i, got)
		}
	}
	if requests[0].Body != requests[1].Body || requests[0].Body == "" {
		t.Errorf("the retry did not resend the same bytes")
	}
}

func TestRetryAfterIsCappedAndAcceptsADate(t *testing.T) {
	h := newHarness(t)
	h.api.reply(
		apiErrorResponse(429, "rate_limited", "Demasiadas peticiones.", nil, map[string]string{"Retry-After": "3600"}),
		fakeResponse{Status: 200, JSON: sampleServiceRequest},
	)
	if _, err := h.client().GetServiceRequest(ctx, sampleServiceRequestID); err != nil {
		t.Fatal(err)
	}
	// Never more than 60 s, plus the jitter.
	if !sameDelays(h.sleeps, ms(60250)) {
		t.Errorf("sleeps = %v", h.sleeps)
	}

	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	for value, want := range map[string]int{
		"5": 5, " 12 ": 12, "0": 0,
		"Wed, 18 Nov 2026 10:00:09 GMT": 9,
		"Wed, 18 Nov 2026 09:00:00 GMT": 0,
	} {
		if got := parseRetryAfter(value, now); got == nil || *got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %d", value, got, want)
		}
	}
	for _, value := range []string{"", "soon", "-1", "1.5", "1234567890"} {
		if got := parseRetryAfter(value, now); got != nil {
			t.Errorf("parseRetryAfter(%q) = %d, want nil", value, *got)
		}
	}
}

func TestServerErrorsAreRetriedWithExponentialBackoffAndJitter(t *testing.T) {
	h := newHarness(t)
	h.api.reply(
		apiErrorResponse(500, "internal_error", "Error interno.", nil, nil),
		fakeResponse{Status: 502, Text: "<html>bad gateway</html>"},
		apiErrorResponse(503, "internal_error", "Error interno.", nil, nil),
		fakeResponse{Status: 201, JSON: sampleServiceRequest},
	)
	result, err := h.client().CreateServiceRequest(ctx, testInput, CreateServiceRequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.ServiceRequest.ID != sampleServiceRequestID {
		t.Errorf("id = %q", result.ServiceRequest.ID)
	}
	requests := h.api.recorded()
	if len(requests) != 4 {
		t.Fatalf("expected 4 requests, got %d", len(requests))
	}
	// random = 0.5 over ceilings of 500, 1000 and 2000 ms.
	if !sameDelays(h.sleeps, ms(250, 500, 1000)) {
		t.Errorf("sleeps = %v", h.sleeps)
	}
	for i, request := range requests {
		if request.Header.Get("Idempotency-Key") != result.IdempotencyKey || request.Body != requests[0].Body {
			t.Errorf("request %d did not resend the same key and bytes", i)
		}
	}
}

func TestTheBackoffIsCappedAndTheJitterComesFromTheRandomSource(t *testing.T) {
	h := newHarness(t)
	h.api.reply(apiErrorResponse(500, "internal_error", "Error interno.", nil, nil))
	_, err := h.client(WithMaxRetries(6), withRandom(func() float64 { return 1 })).GetServiceRequest(ctx, sampleServiceRequestID)
	mustFail(t, err, ErrServer)
	if !sameDelays(h.sleeps, ms(500, 1000, 2000, 4000, 8000, 8000)) {
		t.Errorf("sleeps = %v", h.sleeps)
	}

	h.sleeps = nil
	custom := h.client(WithMaxRetries(3), WithRetryDelays(100*time.Millisecond, 250*time.Millisecond), withRandom(func() float64 { return 0 }))
	_, err = custom.GetServiceRequest(ctx, sampleServiceRequestID)
	mustFail(t, err, ErrServer)
	if !sameDelays(h.sleeps, ms(0, 0, 0)) {
		t.Errorf("sleeps = %v", h.sleeps)
	}
}

func TestItGivesUpAfterMaxRetriesAndReportsTheAttempts(t *testing.T) {
	h := newHarness(t)
	h.api.reply(apiErrorResponse(500, "internal_error", "Error interno.", nil, nil))
	_, err := h.client(WithMaxRetries(2)).CreateServiceRequest(ctx, testInput, CreateServiceRequestOptions{})
	mustFail(t, err, ErrServer)
	apiErr := asAPIError(t, err)
	if apiErr.Code != "internal_error" || apiErr.Attempts != 3 {
		t.Errorf("unexpected error: %+v", apiErr)
	}
	if got := len(h.api.recorded()); got != 3 {
		t.Errorf("expected 3 requests, got %d", got)
	}
	if len(h.sleeps) != 2 {
		t.Errorf("sleeps = %v", h.sleeps)
	}
}

func TestZeroMaxRetriesDisablesRetries(t *testing.T) {
	h := newHarness(t)
	h.api.reply(apiErrorResponse(500, "internal_error", "Error interno.", nil, nil))
	_, err := h.client(WithMaxRetries(0)).GetServiceRequest(ctx, sampleServiceRequestID)
	mustFail(t, err, ErrServer)
	if attemptsOf(t, err) != 1 || len(h.api.recorded()) != 1 || len(h.sleeps) != 0 {
		t.Errorf("attempts=%d requests=%d sleeps=%v", attemptsOf(t, err), len(h.api.recorded()), h.sleeps)
	}
}

func TestANetworkErrorIsRetriedAndSurfacesAsConnectionError(t *testing.T) {
	h := newHarness(t)
	h.api.reply(fakeResponse{Destroy: true}, fakeResponse{Status: 200, JSON: sampleServiceRequest})
	found, err := h.client().GetServiceRequest(ctx, sampleServiceRequestID)
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != sampleServiceRequestID {
		t.Errorf("id = %q", found.ID)
	}
	if len(h.retries) != 1 {
		t.Fatalf("retries = %+v", h.retries)
	}
	mustFail(t, h.retries[0].Err, ErrConnection)

	h.api.reply(fakeResponse{Destroy: true})
	_, err = h.client(WithMaxRetries(1)).CreateServiceRequest(ctx, testInput, CreateServiceRequestOptions{})
	mustFail(t, err, ErrConnection)
	if errors.Is(err, ErrAPI) || errors.Is(err, ErrTimeout) {
		t.Errorf("a network error must only be a connection error: %v", err)
	}
	if attemptsOf(t, err) != 2 {
		t.Errorf("attempts = %d", attemptsOf(t, err))
	}
	if got := len(h.api.recorded()); got != 4 {
		t.Errorf("expected 4 requests, got %d", got)
	}
}

func TestASlowAnswerHitsTheTimeoutIsRetriedAndSurfacesAsTimeoutError(t *testing.T) {
	h := newHarness(t)
	h.api.reply(fakeResponse{Status: 200, JSON: sampleServiceRequest, Delay: 2 * time.Second})
	_, err := h.client(WithTimeout(150*time.Millisecond), WithMaxRetries(1)).GetServiceRequest(ctx, sampleServiceRequestID)
	mustFail(t, err, ErrTimeout)
	if errors.Is(err, ErrConnection) {
		t.Errorf("a timeout must not be a connection error: %v", err)
	}
	if attemptsOf(t, err) != 2 {
		t.Errorf("attempts = %d", attemptsOf(t, err))
	}
	if got := len(h.api.recorded()); got != 2 {
		t.Errorf("expected 2 requests, got %d", got)
	}
	if len(h.sleeps) != 1 {
		t.Errorf("sleeps = %v", h.sleeps)
	}
}

func TestTheContextBoundsTheWholeCall(t *testing.T) {
	h := newHarness(t)

	// A context that is already cancelled: one attempt, no retry.
	h.api.reply(fakeResponse{Status: 200, JSON: sampleServiceRequest})
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := h.client().GetServiceRequest(cancelled, sampleServiceRequestID)
	mustFail(t, err, ErrConnection)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the error does not wrap context.Canceled: %v", err)
	}
	if attemptsOf(t, err) != 1 || len(h.sleeps) != 0 {
		t.Errorf("attempts=%d sleeps=%v", attemptsOf(t, err), h.sleeps)
	}

	// A deadline shorter than the answer: a timeout that is not retried.
	h.api.reply(fakeResponse{Status: 200, JSON: sampleServiceRequest, Delay: 2 * time.Second})
	short, cancelShort := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancelShort()
	_, err = h.client().GetServiceRequest(short, sampleServiceRequestID)
	mustFail(t, err, ErrTimeout)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the error does not wrap context.DeadlineExceeded: %v", err)
	}
	if attemptsOf(t, err) != 1 || len(h.sleeps) != 0 {
		t.Errorf("attempts=%d sleeps=%v", attemptsOf(t, err), h.sleeps)
	}

	// Cancelled while waiting to retry: the real wait stops at once.
	h.api.reply(apiErrorResponse(429, "rate_limited", "Demasiadas peticiones.", nil, map[string]string{"Retry-After": "30"}))
	waiting, cancelWaiting := context.WithCancel(ctx)
	defer cancelWaiting()
	started := time.Now()
	c := h.client(withSleep(sleepContext), WithOnRetry(func(RetryInfo) { cancelWaiting() }))
	_, err = c.GetServiceRequest(waiting, sampleServiceRequestID)
	mustFail(t, err, ErrConnection)
	if !errors.Is(err, context.Canceled) || attemptsOf(t, err) != 1 {
		t.Errorf("unexpected error: %v (attempts %d)", err, attemptsOf(t, err))
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Errorf("the wait was not interrupted: %s", elapsed)
	}
}

// ── Local validation: nothing is sent ────────────────────────────────────────────────────────────

func TestInvalidConfigurationAndArgumentsFailBeforeAnyRequest(t *testing.T) {
	h := newHarness(t)
	construct := func(key string, options ...Option) error {
		_, err := NewClient(key, options...)
		return err
	}
	configurations := map[string]error{
		"empty key":         construct(""),
		"blank key":         construct("   "),
		"not a key":         construct("not-a-key"),
		"prefix only":       construct("glk_test_"),
		"space in the key":  construct("glk_test_FAKE KEY"),
		"http base":         construct(testKey, WithBaseURL("http://example.com/api/v1")),
		"key in the query":  construct(testKey, WithBaseURL("https://example.com/api/v1?key="+testKey)),
		"credentials":       construct(testKey, WithBaseURL("https://user:pass@example.com/api/v1")),
		"fragment":          construct(testKey, WithBaseURL("https://example.com/api/v1#x")),
		"not a URL":         construct(testKey, WithBaseURL("example")),
		"zero timeout":      construct(testKey, WithTimeout(0)),
		"negative retries":  construct(testKey, WithMaxRetries(-1)),
		"zero retry delays": construct(testKey, WithRetryDelays(0, time.Second)),
	}
	for name, err := range configurations {
		if err == nil || !errors.Is(err, ErrConfig) || !errors.Is(err, ErrGrualink) {
			t.Errorf("%s: expected a configuration error, got %v", name, err)
		}
	}
	for _, baseURL := range []string{"https://example.com/api/v1/", "http://localhost:8080/api/v1", "http://127.0.0.1:1/api/v1", "http://[::1]:1/api/v1"} {
		if err := construct(testKey, WithBaseURL(baseURL)); err != nil {
			t.Errorf("%s must be accepted: %v", baseURL, err)
		}
	}
	live, err := NewClient("  glk_live_FAKE_KEY_FOR_UNIT_TESTS_ONLY  ")
	if err != nil || live.Mode() != "live" {
		t.Errorf("a live key must be accepted and trimmed: %v", err)
	}

	c := h.client()
	if c.Mode() != "test" {
		t.Errorf("mode = %q", c.Mode())
	}
	for _, idempotencyKey := range []string{"with space", "ñ", "tab\tinside", strings.Repeat("x", 256)} {
		_, err := c.CreateServiceRequest(ctx, testInput, CreateServiceRequestOptions{IdempotencyKey: idempotencyKey})
		mustFail(t, err, ErrConfig)
	}
	tooLarge := testInput
	tooLarge.Notes = strings.Repeat("x", 17*1024)
	_, err = c.CreateServiceRequest(ctx, tooLarge, CreateServiceRequestOptions{})
	mustFail(t, err, ErrConfig)
	_, err = c.GetServiceRequest(ctx, " ")
	mustFail(t, err, ErrConfig)
	_, err = c.FindServiceRequestsByExternalRef(ctx, "")
	mustFail(t, err, ErrConfig)
	if attemptsOf(t, err) != 0 {
		t.Errorf("attempts = %d", attemptsOf(t, err))
	}
	if got := len(h.api.recorded()); got != 0 {
		t.Errorf("expected no request, got %d", got)
	}

	// The longest valid key is accepted.
	h.api.reply(fakeResponse{Status: 201, JSON: sampleServiceRequest})
	if _, err := c.CreateServiceRequest(ctx, testInput, CreateServiceRequestOptions{IdempotencyKey: strings.Repeat("x", 255)}); err != nil {
		t.Errorf("a key of 255 characters must be accepted: %v", err)
	}
}

func TestABadKeyIsNeverEchoedByTheConfigurationError(t *testing.T) {
	almost := "glk_prod_FAKE_BUT_SHOULD_NOT_BE_ECHOED"
	_, err := NewClient(almost)
	mustFail(t, err, ErrConfig)
	for _, text := range []string{err.Error(), fmt.Sprintf("%v %+v %#v", err, err, err)} {
		if strings.Contains(text, almost) {
			t.Errorf("the configuration error echoes the key: %s", text)
		}
	}
}

// ── The key never leaks ──────────────────────────────────────────────────────────────────────────

type leakyTransport struct{}

func (leakyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("connect failed for Bearer %s", testKey)
}

func TestTheAPIKeyNeverAppearsInErrorsLogsOrTheClientItself(t *testing.T) {
	h := newHarness(t)
	var output bytes.Buffer
	logger := log.New(&output, "", log.LstdFlags)
	structured := slog.New(slog.NewJSONHandler(&output, nil))
	text := slog.New(slog.NewTextHandler(&output, nil))
	logRetry := WithOnRetry(func(info RetryInfo) {
		logger.Printf("retrying: %v %+v %#v", info, info, info)
		structured.Warn("retrying", "info", info, "error", info.Err)
	})

	c := h.client(WithMaxRetries(1), logRetry)
	var failures []error

	// A server that (wrongly) echoes the Authorization header back in every field of the error.
	echo := func(status int) fakeResponse {
		return fakeResponse{Fn: func(request recordedRequest) fakeResponse {
			sent := request.Header.Get("Authorization")
			details := []ErrorDetail{{Field: "field " + sent, Issue: "issue " + sent}}
			return apiErrorResponse(status, "code "+sent, "message "+sent, details, nil)
		}}
	}
	for _, status := range []int{400, 401, 403, 404, 409, 422, 429, 500} {
		h.api.reply(echo(status))
		_, err := c.CreateServiceRequest(ctx, testInput, CreateServiceRequestOptions{})
		mustFail(t, err, ErrAPI)
		failures = append(failures, err)
	}
	h.api.reply(fakeResponse{Destroy: true})
	_, err := c.GetServiceRequest(ctx, sampleServiceRequestID)
	mustFail(t, err, ErrConnection)
	failures = append(failures, err)

	h.api.reply(fakeResponse{Status: 200, JSON: sampleServiceRequest, Delay: 2 * time.Second})
	_, err = h.client(WithTimeout(100*time.Millisecond), WithMaxRetries(0)).GetServiceRequest(ctx, sampleServiceRequestID)
	mustFail(t, err, ErrTimeout)
	failures = append(failures, err)

	// A transport that fails with an error mentioning the key.
	leaky := h.client(WithHTTPClient(&http.Client{Transport: leakyTransport{}}), WithMaxRetries(1), logRetry)
	_, err = leaky.GetServiceRequest(ctx, sampleServiceRequestID)
	mustFail(t, err, ErrConnection)
	failures = append(failures, err)

	if len(failures) != 11 {
		t.Fatalf("expected 11 failures, got %d", len(failures))
	}
	var everything strings.Builder
	for _, client := range []*Client{c, leaky} {
		logger.Printf("client: %v %+v %#v %s", client, client, client, client)
		logger.Printf("client value: %v %+v %#v", *client, *client, *client)
		structured.Info("client", "client", client, "value", *client)
		text.Info("client", "client", client, "value", *client)
		encoded, _ := json.Marshal(client)
		everything.Write(encoded)
	}
	for _, failure := range failures {
		logger.Printf("failure: %v %+v %#v %s %q", failure, failure, failure, failure, failure)
		structured.Error("failure", "error", failure)
		text.Error("failure", "error", failure)
		encoded, _ := json.Marshal(failure)
		everything.Write(encoded)
		for inner := failure; inner != nil; inner = errors.Unwrap(inner) {
			everything.WriteString(inner.Error())
		}
		if apiErr := (*APIError)(nil); errors.As(failure, &apiErr) {
			fmt.Fprintf(&everything, "%+v %#v", *apiErr, apiErr.Details)
		}
	}
	everything.WriteString(output.String())
	all := everything.String()

	sent := false
	for _, request := range h.api.recorded() {
		sent = sent || request.Header.Get("Authorization") == "Bearer "+testKey
	}
	if !sent {
		t.Errorf("the key was never really sent")
	}
	if output.Len() < 2000 || len(h.api.recorded()) < 10 {
		t.Errorf("the output was not really captured (%d bytes)", output.Len())
	}
	if strings.Contains(all, testKey) {
		t.Errorf("the API key leaked")
	}
	if strings.Contains(all, testKey[9:]) {
		t.Errorf("part of the API key leaked")
	}
	if !strings.Contains(all, redactedMarker) {
		t.Errorf("echoed keys must be replaced by a marker")
	}
	if !strings.Contains(all, "connect failed for Bearer "+redactedMarker) {
		t.Errorf("the transport error must be kept with the key replaced")
	}
}
