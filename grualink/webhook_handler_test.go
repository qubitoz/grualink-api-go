package grualink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	webhookSecret   = "whsec_TEST_ONLY_FAKE_SECRET_0001"
	previousSecret  = "whsec_TEST_ONLY_FAKE_SECRET_0000"
	webhookNow      = int64(1795000000)
	webhookTestPath = "/webhooks/grualink"
)

// receiver is a WebhookHandler mounted on a local server, with what it received.
type receiver struct {
	t   *testing.T
	url string

	mu       sync.Mutex
	received []WebhookEvent
	results  []WebhookResult
	failNext bool
	panics   bool
	clock    int64
	counter  int
}

func startReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{t: t, clock: webhookNow}
	handler := NewWebhookHandler(WebhookHandlerOptions{
		Secrets: []string{webhookSecret},
		NowSeconds: func() int64 {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.clock
		},
		OnEvent: func(_ context.Context, event WebhookEvent) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.failNext {
				r.failNext = false
				if r.panics {
					panic("database is down")
				}
				return errors.New("database is down")
			}
			r.received = append(r.received, event)
			return nil
		},
		OnResult: func(result WebhookResult, _ *http.Request) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.results = append(r.results, result)
		},
	})
	mux := http.NewServeMux()
	mux.Handle(webhookTestPath, handler)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	r.url = server.URL + webhookTestPath
	return r
}

func (r *receiver) setClock(now int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clock = now
}

func (r *receiver) failNextEvent(panics bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failNext, r.panics = true, panics
}

func (r *receiver) events() []WebhookEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]WebhookEvent(nil), r.received...)
}

func (r *receiver) reasons() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	reasons := make([]string, 0, len(r.results))
	for _, result := range r.results {
		reasons = append(reasons, result.Reason)
	}
	return reasons
}

type delivery struct {
	id        string
	eventType string
	body      string
}

func (r *receiver) delivery() delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counter++
	id := fmt.Sprintf("bbbbbbbb-0000-4000-8000-%012d", r.counter)
	eventType := "service_request.confirmed"
	// Deliberately not what encoding/json would produce, and with non-ASCII text.
	body := `{"id": "` + id + `", "data": {"previous_status": "quoted", "service_request": {"id": "11111111-2222-4333-8444-555555555555", "status": "confirmed", "external_ref": "TEST-CONFIRMAR-ÑU"}}, "mode": "test", "type": "` + eventType + `", "object": "event", "created_at": "2026-11-18T10:05:01.000Z"}`
	return delivery{id: id, eventType: eventType, body: body}
}

func signedHeaders(d delivery, secrets []string, timestamp int64) map[string]string {
	return map[string]string{
		"Content-Type":         "application/json; charset=utf-8",
		"X-GruaLink-Event":     d.eventType,
		"X-GruaLink-Delivery":  d.id,
		"X-GruaLink-Timestamp": fmt.Sprint(timestamp),
		"X-GruaLink-Mode":      "test",
		"X-GruaLink-Signature": BuildSignatureHeader(secrets, timestamp, []byte(d.body)),
	}
}

func (r *receiver) signed(d delivery) map[string]string {
	return signedHeaders(d, []string{webhookSecret}, webhookNow)
}

type webhookAnswer struct {
	status int
	body   map[string]any
	header http.Header
}

func (r *receiver) send(method, url string, body io.Reader, headers map[string]string) webhookAnswer {
	r.t.Helper()
	request, err := http.NewRequest(method, url, body)
	if err != nil {
		r.t.Fatal(err)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		r.t.Fatal(err)
	}
	defer response.Body.Close()
	answer := webhookAnswer{status: response.StatusCode, header: response.Header}
	if err := json.NewDecoder(response.Body).Decode(&answer.body); err != nil {
		r.t.Fatalf("the answer is not JSON: %v", err)
	}
	return answer
}

func (r *receiver) post(body string, headers map[string]string) webhookAnswer {
	r.t.Helper()
	return r.send(http.MethodPost, r.url, strings.NewReader(body), headers)
}

func expectAnswer(t *testing.T, answer webhookAnswer, status int, body map[string]any) {
	t.Helper()
	if answer.status != status || !reflect.DeepEqual(answer.body, body) {
		t.Errorf("got %d %v, want %d %v", answer.status, answer.body, status, body)
	}
}

func TestASignedDeliveryIsAcceptedAndHandedToOnEvent(t *testing.T) {
	r := startReceiver(t)
	d := r.delivery()
	answer := r.post(d.body, r.signed(d))
	expectAnswer(t, answer, 200, map[string]any{"received": true})
	if got := answer.header.Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	events := r.events()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	event := events[0]
	if event.ID != d.id || event.Type != "service_request.confirmed" || event.Mode != "test" || event.Object != "event" {
		t.Errorf("unexpected event: %+v", event)
	}
	if event.Data.PreviousStatus == nil || *event.Data.PreviousStatus != StatusQuoted {
		t.Errorf("unexpected previous_status: %v", event.Data.PreviousStatus)
	}
	sr := event.Data.ServiceRequest
	if sr == nil || sr.Status != StatusConfirmed || sr.ExternalRef == nil || *sr.ExternalRef != "TEST-CONFIRMAR-ÑU" {
		t.Errorf("unexpected service request: %+v", sr)
	}
	if got := r.reasons(); !reflect.DeepEqual(got, []string{"ok"}) {
		t.Errorf("reasons = %v", got)
	}
}

func TestARetryOfTheSameDeliveryAnswers200ButIsProcessedOnce(t *testing.T) {
	r := startReceiver(t)
	d := r.delivery()
	expectAnswer(t, r.post(d.body, r.signed(d)), 200, map[string]any{"received": true})
	// A retry is signed again, with a new timestamp and the same id.
	r.setClock(webhookNow + 60)
	again := r.post(d.body, signedHeaders(d, []string{webhookSecret}, webhookNow+60))
	expectAnswer(t, again, 200, map[string]any{"received": true, "duplicate": true})
	if got := len(r.events()); got != 1 {
		t.Errorf("the event was processed %d times", got)
	}
	if got := r.reasons(); !reflect.DeepEqual(got, []string{"ok", "duplicate"}) {
		t.Errorf("reasons = %v", got)
	}
}

func TestWhenOnEventFailsTheAnswerIs500AndTheRetryIsProcessed(t *testing.T) {
	for name, panics := range map[string]bool{"error": false, "panic": true} {
		t.Run(name, func(t *testing.T) {
			r := startReceiver(t)
			d := r.delivery()
			r.failNextEvent(panics)
			first := r.post(d.body, r.signed(d))
			expectAnswer(t, first, 500, map[string]any{"received": false, "error": "handler_failed"})
			if got := len(r.events()); got != 0 {
				t.Errorf("expected no event, got %d", got)
			}
			retry := r.post(d.body, r.signed(d))
			expectAnswer(t, retry, 200, map[string]any{"received": true})
			if got := len(r.events()); got != 1 {
				t.Errorf("expected 1 event, got %d", got)
			}
			if got := r.reasons(); !reflect.DeepEqual(got, []string{"handler_failed", "ok"}) {
				t.Errorf("reasons = %v", got)
			}
		})
	}
}

func TestDuringARotationADeliverySignedWithBothSecretsIsAccepted(t *testing.T) {
	r := startReceiver(t)
	d := r.delivery()
	both := signedHeaders(d, []string{"whsec_TEST_ONLY_FAKE_SECRET_NEXT", webhookSecret}, webhookNow)
	expectAnswer(t, r.post(d.body, both), 200, map[string]any{"received": true})
	old := r.delivery()
	answer := r.post(old.body, signedHeaders(old, []string{previousSecret}, webhookNow))
	expectAnswer(t, answer, 401, map[string]any{"received": false, "error": "invalid_signature"})
}

func TestATamperedBodyAMissingSignatureAndAStaleTimestampAreRejectedWith401(t *testing.T) {
	r := startReceiver(t)
	d := r.delivery()
	unauthorized := map[string]any{"received": false, "error": "invalid_signature"}

	tampered := strings.Replace(d.body, "confirmed", "completed", 1)
	expectAnswer(t, r.post(tampered, r.signed(d)), 401, unauthorized)

	unsigned := r.signed(d)
	delete(unsigned, "X-GruaLink-Signature")
	expectAnswer(t, r.post(d.body, unsigned), 401, unauthorized)

	r.setClock(webhookNow + 301)
	expectAnswer(t, r.post(d.body, r.signed(d)), 401, unauthorized)

	// The reason stays in the logs of the receiver; the caller always gets the same answer.
	want := []string{"signature_mismatch", "missing_header", "timestamp_out_of_tolerance"}
	if got := r.reasons(); !reflect.DeepEqual(got, want) {
		t.Errorf("reasons = %v", got)
	}
	if got := len(r.events()); got != 0 {
		t.Errorf("expected no event, got %d", got)
	}

	// The limit is inclusive, and a rejected delivery was not remembered as processed.
	r.setClock(webhookNow + 300)
	expectAnswer(t, r.post(d.body, r.signed(d)), 200, map[string]any{"received": true})
}

func TestTheBodyParsedAndSerializedAgainDoesNotVerify(t *testing.T) {
	r := startReceiver(t)
	d := r.delivery()
	var parsed any
	if err := json.Unmarshal([]byte(d.body), &parsed); err != nil {
		t.Fatal(err)
	}
	reserialized, err := json.Marshal(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if string(reserialized) == d.body {
		t.Fatalf("the test body must differ from what encoding/json writes")
	}
	answer := r.post(string(reserialized), r.signed(d))
	expectAnswer(t, answer, 401, map[string]any{"received": false, "error": "invalid_signature"})
}

func TestADeliveryHeaderThatDisagreesWithTheSignedIDIsRejected(t *testing.T) {
	r := startReceiver(t)
	d := r.delivery()
	headers := r.signed(d)
	headers["X-GruaLink-Delivery"] = "cccccccc-0000-4000-8000-000000000000"
	expectAnswer(t, r.post(d.body, headers), 400, map[string]any{"received": false, "error": "delivery_id_mismatch"})

	delete(headers, "X-GruaLink-Delivery")
	expectAnswer(t, r.post(d.body, headers), 400, map[string]any{"received": false, "error": "delivery_id_mismatch"})

	if got := r.reasons(); !reflect.DeepEqual(got, []string{"delivery_id_mismatch", "delivery_id_mismatch"}) {
		t.Errorf("reasons = %v", got)
	}
	if got := len(r.events()); got != 0 {
		t.Errorf("expected no event, got %d", got)
	}
}

func TestASignedBodyThatIsNotAnEventIsA400(t *testing.T) {
	r := startReceiver(t)
	bodies := []string{`[1, 2, 3]`, `null`, `{"type": "webhook.test"}`, `{"id": 7, "type": "webhook.test"}`, `{"id": "x"`, "{\"id\": \"x\", \"type\": \"\xff\"}"}
	for _, body := range bodies {
		d := delivery{id: "x", eventType: "webhook.test", body: body}
		expectAnswer(t, r.post(body, r.signed(d)), 400, map[string]any{"received": false, "error": "invalid_payload"})
	}
	for _, reason := range r.reasons() {
		if reason != "invalid_payload" {
			t.Errorf("reason = %s", reason)
		}
	}
	if got := len(r.reasons()); got != len(bodies) {
		t.Errorf("expected %d results, got %d", len(bodies), got)
	}
}

func TestOtherMethodsAndOversizedBodiesAreRefused(t *testing.T) {
	r := startReceiver(t)
	get := r.send(http.MethodGet, r.url, nil, nil)
	expectAnswer(t, get, 405, map[string]any{"received": false, "error": "method_not_allowed"})
	if got := get.header.Get("Allow"); got != "POST" {
		t.Errorf("Allow = %q", got)
	}

	tooLarge := map[string]any{"received": false, "error": "payload_too_large"}
	big := bytes.Repeat([]byte{' '}, MaxWebhookBodyBytes+1)
	// With a declared length.
	expectAnswer(t, r.send(http.MethodPost, r.url, bytes.NewReader(big), nil), 413, tooLarge)
	// Without one (chunked): the cap applies to what is actually read.
	expectAnswer(t, r.send(http.MethodPost, r.url, io.MultiReader(bytes.NewReader(big)), nil), 413, tooLarge)

	// Exactly at the cap it is read (and then refused for its signature).
	atCap := bytes.Repeat([]byte{' '}, MaxWebhookBodyBytes)
	expectAnswer(t, r.send(http.MethodPost, r.url, bytes.NewReader(atCap), nil), 401, map[string]any{"received": false, "error": "invalid_signature"})

	if got := len(r.events()); got != 0 {
		t.Errorf("expected no event, got %d", got)
	}
}

func TestHandleDeliveryWorksWithoutAServer(t *testing.T) {
	var events []WebhookEvent
	handler := NewWebhookHandler(WebhookHandlerOptions{
		Secrets:    []string{"", webhookSecret},
		NowSeconds: func() int64 { return webhookNow },
		OnEvent: func(_ context.Context, event WebhookEvent) error {
			events = append(events, event)
			return nil
		},
	})
	body := []byte(`{"id":"dddddddd-0000-4000-8000-000000000001","object":"event","type":"webhook.test","mode":"test","created_at":"2026-11-18T10:05:01.000Z","data":{"message":"Aviso de prueba."}}`)
	header := http.Header{}
	// Header names are case-insensitive.
	header.Set("x-grualink-signature", BuildSignatureHeader([]string{webhookSecret}, webhookNow, body))
	header.Set("x-grualink-delivery", "dddddddd-0000-4000-8000-000000000001")

	result := handler.HandleDelivery(context.Background(), body, header)
	if result.Status != 200 || !result.Body.Received || result.Body.Duplicate || result.Reason != "ok" {
		t.Errorf("unexpected result: %+v", result)
	}
	again := handler.HandleDelivery(context.Background(), body, header)
	if again.Status != 200 || !again.Body.Duplicate || again.Reason != ReasonDuplicate {
		t.Errorf("unexpected result: %+v", again)
	}
	if len(events) != 1 || events[0].Type != "webhook.test" || events[0].Data.Message != "Aviso de prueba." {
		t.Errorf("unexpected events: %+v", events)
	}

	// No secret configured: everything is refused.
	unconfigured := NewWebhookHandler(WebhookHandlerOptions{NowSeconds: func() int64 { return webhookNow }})
	if got := unconfigured.HandleDelivery(context.Background(), body, header); got.Status != 401 || got.Reason != "missing_secret" {
		t.Errorf("unexpected result: %+v", got)
	}
}

type failingDeduplicator struct{}

func (failingDeduplicator) Claim(context.Context, string) (bool, error) {
	return false, errors.New("store is down")
}

func (failingDeduplicator) Release(context.Context, string) error { return nil }

func TestADeduplicatorThatFailsAnswers500SoTheDeliveryIsRetried(t *testing.T) {
	called := false
	handler := NewWebhookHandler(WebhookHandlerOptions{
		Secrets:      []string{webhookSecret},
		NowSeconds:   func() int64 { return webhookNow },
		Deduplicator: failingDeduplicator{},
		OnEvent: func(context.Context, WebhookEvent) error {
			called = true
			return nil
		},
	})
	body := []byte(`{"id": "eeeeeeee-0000-4000-8000-000000000001", "type": "webhook.test"}`)
	header := http.Header{}
	header.Set(SignatureHeader, BuildSignatureHeader([]string{webhookSecret}, webhookNow, body))
	header.Set(DeliveryHeader, "eeeeeeee-0000-4000-8000-000000000001")
	if result := handler.HandleDelivery(context.Background(), body, header); result.Status != 500 || called {
		t.Errorf("unexpected result: %+v (called=%v)", result, called)
	}
}

func TestInMemoryDeliveryDeduplicator(t *testing.T) {
	background := context.Background()
	now := time.Unix(0, 0)
	dedup := NewInMemoryDeliveryDeduplicator(InMemoryDeliveryDeduplicatorOptions{
		TTL: time.Second, MaxEntries: 3, Now: func() time.Time { return now },
	})
	claim := func(id string) bool {
		t.Helper()
		claimed, err := dedup.Claim(background, id)
		if err != nil {
			t.Fatal(err)
		}
		return claimed
	}
	if !claim("a") {
		t.Errorf("the first claim must succeed")
	}
	if claim("a") {
		t.Errorf("a duplicate must be refused")
	}
	if err := dedup.Release(background, "a"); err != nil {
		t.Fatal(err)
	}
	if !claim("a") {
		t.Errorf("a released id can be claimed again")
	}

	now = now.Add(1001 * time.Millisecond)
	if !claim("a") {
		t.Errorf("an expired id can be claimed again")
	}

	if !claim("b") || !claim("c") {
		t.Errorf("new ids must be claimed")
	}
	if !claim("d") {
		t.Errorf("the oldest entry is evicted when the cap is reached")
	}
	if claim("d") {
		t.Errorf("a duplicate must be refused")
	}
	if !claim("a") {
		t.Errorf("the evicted id is no longer remembered")
	}

	// Safe for concurrent use: one winner per id.
	shared := NewInMemoryDeliveryDeduplicator(InMemoryDeliveryDeduplicatorOptions{})
	var wins sync.Map
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				id := fmt.Sprintf("id-%d", i)
				if claimed, _ := shared.Claim(background, id); claimed {
					count, _ := wins.LoadOrStore(id, new(int32))
					*count.(*int32)++
				}
			}
		}()
	}
	wg.Wait()
	total := 0
	wins.Range(func(_, value any) bool {
		total += int(*value.(*int32))
		return true
	})
	if total != 200 {
		t.Errorf("expected 200 winning claims, got %d", total)
	}
}
