package grualink

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"unicode/utf8"
)

// Outcomes of a delivery besides the six reasons of the signature verification.
const (
	ReasonInvalidPayload     = "invalid_payload"
	ReasonDeliveryIDMismatch = "delivery_id_mismatch"
	ReasonDuplicate          = "duplicate"
	ReasonHandlerFailed      = "handler_failed"
)

// WebhookHandlerOptions configures NewWebhookHandler.
type WebhookHandlerOptions struct {
	// Secrets holds the signing secret (whsec_...), or current and previous during your rotation.
	Secrets []string
	// OnEvent is called once per verified, non-duplicate delivery. Keep it fast (GRUALINK waits 10
	// seconds): store the event or enqueue it and do the heavy work later. If it returns an error
	// (or panics), the answer is 500 and the delivery is retried.
	OnEvent func(ctx context.Context, event WebhookEvent) error
	// Deduplicator defaults to an InMemoryDeliveryDeduplicator.
	Deduplicator DeliveryDeduplicator
	// ToleranceSeconds defaults to 300.
	ToleranceSeconds int64
	// NowSeconds is the clock in seconds, for tests.
	NowSeconds func() int64
	// OnResult is called after each delivery with the outcome, for your logs. The body and the
	// secret are never passed.
	OnResult func(result WebhookResult, request *http.Request)
}

// WebhookResponseBody is the JSON body the receiver answers with.
type WebhookResponseBody struct {
	Received  bool   `json:"received"`
	Duplicate bool   `json:"duplicate,omitempty"`
	Error     string `json:"error,omitempty"`
}

// WebhookResult is the outcome of one delivery.
type WebhookResult struct {
	// Status is the HTTP status to answer with.
	Status int
	// Body is the JSON body to answer with.
	Body WebhookResponseBody
	// Reason is the internal detail for your logs; never send it back to the caller. It is one of
	// the six reasons of the signature verification, or invalid_payload, delivery_id_mismatch,
	// duplicate, handler_failed.
	Reason string
}

// WebhookHandler receives GRUALINK webhooks. It is an http.Handler: mount it on net/http or on any
// router that accepts one, as long as nothing reads the body before it.
type WebhookHandler struct {
	options      WebhookHandlerOptions
	deduplicator DeliveryDeduplicator
}

// NewWebhookHandler returns the receiver: it verifies the signature over the raw body, parses,
// deduplicates and dispatches to OnEvent.
func NewWebhookHandler(options WebhookHandlerOptions) *WebhookHandler {
	deduplicator := options.Deduplicator
	if deduplicator == nil {
		deduplicator = NewInMemoryDeliveryDeduplicator(InMemoryDeliveryDeduplicatorOptions{})
	}
	return &WebhookHandler{options: options, deduplicator: deduplicator}
}

func rejectDelivery(status int, code, reason string) WebhookResult {
	return WebhookResult{Status: status, Body: WebhookResponseBody{Received: false, Error: code}, Reason: reason}
}

// HandleDelivery handles one delivery from its raw body and headers, without touching the network.
// ServeHTTP uses it; call it directly if your framework does not give you an http.Handler.
func (h *WebhookHandler) HandleDelivery(ctx context.Context, rawBody []byte, header http.Header) WebhookResult {
	input := VerifyWebhookSignatureInput{
		RawBody:          rawBody,
		SignatureHeader:  header.Get(SignatureHeader),
		Secrets:          h.options.Secrets,
		ToleranceSeconds: h.options.ToleranceSeconds,
	}
	if h.options.NowSeconds != nil {
		input.NowSeconds = h.options.NowSeconds()
	}
	verification := VerifyWebhookSignature(input)
	if !verification.Valid {
		return rejectDelivery(http.StatusUnauthorized, "invalid_signature", string(verification.Reason))
	}

	// Only now, with the signature verified, is the body parsed.
	var probe struct {
		ID   *string `json:"id"`
		Type *string `json:"type"`
	}
	var event WebhookEvent
	if !utf8.Valid(rawBody) || json.Unmarshal(rawBody, &probe) != nil || probe.ID == nil || probe.Type == nil ||
		json.Unmarshal(rawBody, &event) != nil {
		return rejectDelivery(http.StatusBadRequest, ReasonInvalidPayload, ReasonInvalidPayload)
	}

	// The header is not covered by the signature; the id inside the signed body is. They must agree.
	deliveryIDs := header.Values(DeliveryHeader)
	if len(deliveryIDs) == 0 || deliveryIDs[0] != event.ID {
		return rejectDelivery(http.StatusBadRequest, ReasonDeliveryIDMismatch, ReasonDeliveryIDMismatch)
	}

	claimed, err := h.deduplicator.Claim(ctx, event.ID)
	if err != nil {
		return rejectDelivery(http.StatusInternalServerError, ReasonHandlerFailed, ReasonHandlerFailed)
	}
	if !claimed {
		return WebhookResult{Status: http.StatusOK, Body: WebhookResponseBody{Received: true, Duplicate: true}, Reason: ReasonDuplicate}
	}
	if err := h.dispatch(ctx, event); err != nil {
		// Release the id so the retry of this delivery is processed.
		_ = h.deduplicator.Release(ctx, event.ID)
		return rejectDelivery(http.StatusInternalServerError, ReasonHandlerFailed, ReasonHandlerFailed)
	}
	return WebhookResult{Status: http.StatusOK, Body: WebhookResponseBody{Received: true}, Reason: string(ReasonOK)}
}

// dispatch calls OnEvent and turns a panic into an error, so the id is released and the delivery retried.
func (h *WebhookHandler) dispatch(ctx context.Context, event WebhookEvent) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("grualink: OnEvent panicked: %v", recovered)
		}
	}()
	if h.options.OnEvent == nil {
		return nil
	}
	return h.options.OnEvent(ctx, event)
}

// ServeHTTP reads the raw body with a hard cap and answers with the outcome of HandleDelivery.
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeWebhookResponse(w, http.StatusMethodNotAllowed, WebhookResponseBody{Error: "method_not_allowed"})
		return
	}
	if r.ContentLength > MaxWebhookBodyBytes {
		w.Header().Set("Connection", "close")
		writeWebhookResponse(w, http.StatusRequestEntityTooLarge, WebhookResponseBody{Error: "payload_too_large"})
		return
	}
	// Never buffers more than the cap plus one byte, whatever the caller declares.
	rawBody, err := io.ReadAll(io.LimitReader(r.Body, MaxWebhookBodyBytes+1))
	if err != nil {
		writeWebhookResponse(w, http.StatusBadRequest, WebhookResponseBody{Error: ReasonInvalidPayload})
		return
	}
	if len(rawBody) > MaxWebhookBodyBytes {
		w.Header().Set("Connection", "close")
		writeWebhookResponse(w, http.StatusRequestEntityTooLarge, WebhookResponseBody{Error: "payload_too_large"})
		return
	}
	result := h.HandleDelivery(r.Context(), rawBody, r.Header)
	writeWebhookResponse(w, result.Status, result.Body)
	if h.options.OnResult != nil {
		h.options.OnResult(result, r)
	}
}

func writeWebhookResponse(w http.ResponseWriter, status int, body WebhookResponseBody) {
	payload, err := json.Marshal(body)
	if err != nil {
		payload = []byte(`{"received":false}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}
