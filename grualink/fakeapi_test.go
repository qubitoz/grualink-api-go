package grualink

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type recordedRequest struct {
	Method string
	// Path is the request target as sent: path and query.
	Path   string
	Header http.Header
	Body   string
}

type fakeResponse struct {
	Status  int
	Headers map[string]string
	// JSON is sent as JSON; when nil, Text is sent as an HTML page.
	JSON any
	Text string
	// Delay waits before answering (to exercise the timeout).
	Delay time.Duration
	// Destroy closes the connection without answering (to exercise network errors).
	Destroy bool
	// Fn builds the answer from the request.
	Fn func(recordedRequest) fakeResponse
}

// fakeAPI is a local stand-in for the API: it records what it receives and answers what the test queued.
type fakeAPI struct {
	BaseURL string

	server   *httptest.Server
	mu       sync.Mutex
	requests []recordedRequest
	queue    []fakeResponse
}

func startFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	api := &fakeAPI{}
	api.server = httptest.NewServer(http.HandlerFunc(api.handle))
	api.BaseURL = api.server.URL + "/api/v1"
	t.Cleanup(api.server.Close)
	return api
}

// reply queues answers; the last one repeats once the queue is exhausted.
func (f *fakeAPI) reply(responses ...fakeResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = responses
}

func (f *fakeAPI) recorded() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

func (f *fakeAPI) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	request := recordedRequest{Method: r.Method, Path: r.URL.RequestURI(), Header: r.Header.Clone(), Body: string(body)}

	f.mu.Lock()
	f.requests = append(f.requests, request)
	answer := fakeResponse{Status: http.StatusInternalServerError, Text: "no reply queued"}
	if len(f.queue) > 0 {
		answer = f.queue[0]
	}
	if len(f.queue) > 1 {
		f.queue = f.queue[1:]
	}
	f.mu.Unlock()

	if answer.Fn != nil {
		answer = answer.Fn(request)
	}
	if answer.Destroy {
		if hijacker, ok := w.(http.Hijacker); ok {
			if conn, _, err := hijacker.Hijack(); err == nil {
				conn.Close()
			}
		}
		return
	}
	if answer.Delay > 0 {
		select {
		case <-time.After(answer.Delay):
		case <-r.Context().Done():
			return
		}
	}
	contentType := "text/html"
	payload := []byte(answer.Text)
	if answer.JSON != nil {
		contentType = "application/json; charset=utf-8"
		payload, _ = json.Marshal(answer.JSON)
	}
	w.Header().Set("Content-Type", contentType)
	for name, value := range answer.Headers {
		w.Header().Set(name, value)
	}
	w.WriteHeader(answer.Status)
	_, _ = w.Write(payload)
}

func apiErrorResponse(status int, code, message string, details []ErrorDetail, headers map[string]string) fakeResponse {
	body := map[string]any{"code": code, "message": message}
	if details != nil {
		body["details"] = details
	}
	return fakeResponse{Status: status, JSON: map[string]any{"error": body}, Headers: headers}
}

const sampleServiceRequestID = "11111111-2222-4333-8444-555555555555"

// sampleServiceRequest is the object of section 5 of the contract.
var sampleServiceRequest = json.RawMessage(`{
  "id": "11111111-2222-4333-8444-555555555555",
  "object": "service_request",
  "status": "quoted",
  "mode": "test",
  "external_ref": "TEST-CONFIRMAR-0001",
  "created_at": "2026-11-18T10:00:00.000Z",
  "resolved_at": null,
  "rejection_reason": null,
  "service": null,
  "quote": {
    "estimated_price": {
      "currency": "MXN",
      "subtotal": 864.33,
      "taxes": [{ "name": "IVA", "type": "tax", "rate": 16, "amount": 138.29 }],
      "tax_total": 138.29,
      "retention_total": 0,
      "total": 1002.62
    },
    "includes_extras": false,
    "approximate_distance": true,
    "unpriced_reason": null,
    "note": "Precio estimado.",
    "a_field_added_later": "ignored"
  },
  "assignment": null,
  "timeline": null
}`)
