// Package grualink is a minimal client and webhook receiver for the GRUALINK public API.
//
// It is example code meant to be read and copied into your project, not a full SDK. It uses only the
// standard library. The contract it implements is in the contract folder of the repository.
package grualink

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Defaults of the client.
const (
	DefaultBaseURL        = "https://grualink.mx/api/v1"
	DefaultTimeout        = 15 * time.Second
	DefaultMaxRetries     = 3
	DefaultRetryBaseDelay = 500 * time.Millisecond
	DefaultRetryMaxDelay  = 8 * time.Second
)

const (
	maxBodyBytes = 16 * 1024
	// The API never asks to wait more than 60 seconds.
	maxRetryAfterSeconds = 60
	maxResponseBytes     = 1 << 20
	redactedMarker       = "[REDACTED]"
)

// RetryInfo describes a failed attempt that is about to be retried.
type RetryInfo struct {
	// Attempt is the attempt that just failed, starting at 1.
	Attempt int
	// Delay is the time the client will wait before the next attempt.
	Delay time.Duration
	// Err is why it failed. It never contains the API key.
	Err error
}

type clientConfig struct {
	baseURL        string
	timeout        time.Duration
	maxRetries     int
	retryBaseDelay time.Duration
	retryMaxDelay  time.Duration
	onRetry        func(RetryInfo)
	httpClient     *http.Client
	sleep          func(context.Context, time.Duration) error
	random         func() float64
}

// Option configures a Client.
type Option func(*clientConfig)

// WithBaseURL overrides the base URL (default https://grualink.mx/api/v1). It must use https; http
// is only accepted for localhost.
func WithBaseURL(baseURL string) Option {
	return func(c *clientConfig) { c.baseURL = baseURL }
}

// WithTimeout sets the maximum time for each attempt (default 15 seconds). The context passed to
// each call bounds the whole call, retries and waits included.
func WithTimeout(timeout time.Duration) Option {
	return func(c *clientConfig) { c.timeout = timeout }
}

// WithMaxRetries sets the retries after the first attempt for 429 rate_limited, 5xx, timeouts and
// network errors (default 3). Zero disables retries.
func WithMaxRetries(maxRetries int) Option {
	return func(c *clientConfig) { c.maxRetries = maxRetries }
}

// WithRetryDelays sets the first backoff step (default 500 ms) and the ceiling of the exponential
// backoff (default 8 seconds).
func WithRetryDelays(base, ceiling time.Duration) Option {
	return func(c *clientConfig) { c.retryBaseDelay, c.retryMaxDelay = base, ceiling }
}

// WithOnRetry sets a function called before each wait. Use it for your own logging.
func WithOnRetry(onRetry func(RetryInfo)) Option {
	return func(c *clientConfig) { c.onRetry = onRetry }
}

// WithHTTPClient sets the HTTP client to use, for example to share a transport. The client works on
// a copy that never follows redirects.
func WithHTTPClient(httpClient *http.Client) Option {
	return func(c *clientConfig) { c.httpClient = httpClient }
}

// Injection points for tests.
func withSleep(sleep func(context.Context, time.Duration) error) Option {
	return func(c *clientConfig) { c.sleep = sleep }
}

func withRandom(random func() float64) Option {
	return func(c *clientConfig) { c.random = random }
}

// Client calls the GRUALINK API. It is safe for concurrent use.
//
// The API key is not stored in any field: it lives inside two closures, so it is not shown by fmt,
// log or encoding/json, whatever the verb.
type Client struct {
	baseURL        string
	mode           string
	timeout        time.Duration
	maxRetries     int
	retryBaseDelay time.Duration
	retryMaxDelay  time.Duration
	onRetry        func(RetryInfo)
	httpClient     *http.Client
	sleep          func(context.Context, time.Duration) error
	random         func() float64
	// authorize adds the Authorization header; redact replaces the key with a marker in any text
	// that will leave this type.
	authorize func(*http.Request)
	redact    func(string) string
}

// NewClient returns a client for the given API key (glk_test_... or glk_live_...). Read the key from
// the environment (GRUALINK_API_KEY); never hard-code it.
func NewClient(apiKey string, options ...Option) (*Client, error) {
	config := clientConfig{
		baseURL:        DefaultBaseURL,
		timeout:        DefaultTimeout,
		maxRetries:     DefaultMaxRetries,
		retryBaseDelay: DefaultRetryBaseDelay,
		retryMaxDelay:  DefaultRetryMaxDelay,
		sleep:          sleepContext,
		random:         mrand.Float64,
	}
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}

	key := strings.TrimSpace(apiKey)
	if key == "" {
		return nil, configError("the API key is required (set GRUALINK_API_KEY).")
	}
	mode, ok := keyMode(key)
	if !ok {
		// The value is not echoed: it may be a real key with a typo.
		return nil, configError("the API key must start with glk_test_ or glk_live_.")
	}

	baseURL := strings.TrimRight(config.baseURL, "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" {
		return nil, configError("the base URL is not a valid URL.")
	}
	host := parsed.Hostname()
	local := host == "localhost" || host == "127.0.0.1" || host == "::1"
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && local) {
		return nil, configError("the base URL must use https (http is only accepted for localhost).")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.User != nil {
		return nil, configError("the base URL must not contain credentials, query or fragment.")
	}
	if config.timeout <= 0 {
		return nil, configError("the timeout must be positive.")
	}
	if config.maxRetries < 0 {
		return nil, configError("the maximum number of retries must not be negative.")
	}
	if config.retryBaseDelay <= 0 || config.retryMaxDelay <= 0 {
		return nil, configError("the retry delays must be positive.")
	}
	if config.sleep == nil || config.random == nil {
		return nil, configError("the sleep and random functions must not be nil.")
	}

	httpClient := &http.Client{}
	if config.httpClient != nil {
		copied := *config.httpClient
		httpClient = &copied
	}
	// A redirect is never followed: the key must not travel to another address.
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	return &Client{
		baseURL:        baseURL,
		mode:           mode,
		timeout:        config.timeout,
		maxRetries:     config.maxRetries,
		retryBaseDelay: config.retryBaseDelay,
		retryMaxDelay:  config.retryMaxDelay,
		onRetry:        config.onRetry,
		httpClient:     httpClient,
		sleep:          config.sleep,
		random:         config.random,
		authorize:      func(request *http.Request) { request.Header.Set("Authorization", "Bearer "+key) },
		redact:         func(text string) string { return strings.ReplaceAll(text, key, redactedMarker) },
	}, nil
}

// Mode is test or live, from the key prefix.
func (c *Client) Mode() string { return c.mode }

// String describes the client without the key.
func (c *Client) String() string {
	return fmt.Sprintf("grualink.Client{mode: %s, baseURL: %s}", c.mode, c.baseURL)
}

// Health calls GET /health. It is sent without the key.
func (c *Client) Health(ctx context.Context) (*HealthStatus, error) {
	var health HealthStatus
	if _, err := c.call(ctx, http.MethodGet, "/health", nil, nil, false, &health); err != nil {
		return nil, err
	}
	return &health, nil
}

// CreateServiceRequestOptions are the options of Client.CreateServiceRequest.
type CreateServiceRequestOptions struct {
	// IdempotencyKey is 1 to 255 visible ASCII characters, unique per request you want to create.
	// Store it with your record before calling: sending it again with the same body returns the
	// original response instead of a duplicate. Generated (UUID v4) when empty.
	IdempotencyKey string
}

// CreateServiceRequestResult is the result of Client.CreateServiceRequest.
type CreateServiceRequestResult struct {
	ServiceRequest ServiceRequest
	// Replayed is true when the API answered 200 with Idempotent-Replayed: true (nothing new was created).
	Replayed bool
	// IdempotencyKey is the key that was sent (yours or the generated one).
	IdempotencyKey string
}

// CreateServiceRequest calls POST /service-requests. The body is serialized once and the exact same
// bytes and Idempotency-Key are reused on every retry, so a retry can never create a second request.
func (c *Client) CreateServiceRequest(ctx context.Context, input ServiceRequestInput, options CreateServiceRequestOptions) (*CreateServiceRequestResult, error) {
	idempotencyKey := options.IdempotencyKey
	if idempotencyKey == "" {
		generated, err := newUUID()
		if err != nil {
			return nil, configError("could not generate an Idempotency-Key.")
		}
		idempotencyKey = generated
	}
	if !validIdempotencyKey(idempotencyKey) {
		return nil, configError("the Idempotency-Key must be 1 to 255 visible ASCII characters.")
	}
	body, err := json.Marshal(input)
	if err != nil {
		return nil, configError("the service request cannot be serialized as JSON.")
	}
	if len(body) > maxBodyBytes {
		return nil, configError("the service request exceeds 16 KB.")
	}

	headers := map[string]string{"Content-Type": "application/json", "Idempotency-Key": idempotencyKey}
	result := &CreateServiceRequestResult{IdempotencyKey: idempotencyKey}
	response, err := c.call(ctx, http.MethodPost, "/service-requests", body, headers, true, &result.ServiceRequest)
	if err != nil {
		return nil, err
	}
	result.Replayed = response.StatusCode == http.StatusOK && response.Header.Get("Idempotent-Replayed") == "true"
	return result, nil
}

// GetServiceRequest calls GET /service-requests/{id}.
func (c *Client) GetServiceRequest(ctx context.Context, id string) (*ServiceRequest, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, configError("the id is required.")
	}
	var serviceRequest ServiceRequest
	if _, err := c.call(ctx, http.MethodGet, "/service-requests/"+url.PathEscape(id), nil, nil, true, &serviceRequest); err != nil {
		return nil, err
	}
	return &serviceRequest, nil
}

// FindServiceRequestsByExternalRef calls GET /service-requests?external_ref=...: up to 20 of your
// requests with that reference, newest first.
func (c *Client) FindServiceRequestsByExternalRef(ctx context.Context, externalRef string) ([]ServiceRequest, error) {
	if strings.TrimSpace(externalRef) == "" {
		return nil, configError("the external reference is required.")
	}
	query := url.Values{"external_ref": {externalRef}}.Encode()
	var list ServiceRequestList
	if _, err := c.call(ctx, http.MethodGet, "/service-requests?"+query, nil, nil, true, &list); err != nil {
		return nil, err
	}
	if list.Data == nil {
		list.Data = []ServiceRequest{}
	}
	return list.Data, nil
}

// call sends one request, retrying what can be retried, and decodes a 2xx answer into out.
func (c *Client) call(ctx context.Context, method, path string, body []byte, headers map[string]string, authenticated bool, out any) (*http.Response, error) {
	if ctx == nil {
		return nil, configError("the context must not be nil.")
	}
	for attempt := 1; ; attempt++ {
		response, failure := c.attempt(ctx, method, path, body, headers, authenticated, out)
		if failure == nil {
			return response, nil
		}
		setAttempts(failure, attempt)
		if ctx.Err() != nil || !isRetryable(failure) || attempt > c.maxRetries {
			return nil, failure
		}
		delay := c.retryDelay(attempt, failure)
		if c.onRetry != nil {
			c.onRetry(RetryInfo{Attempt: attempt, Delay: delay, Err: failure})
		}
		if err := c.sleep(ctx, delay); err != nil {
			interrupted := contextError(ctx, err)
			interrupted.Attempts = attempt
			return nil, interrupted
		}
	}
}

// attempt makes a single HTTP attempt with its own timeout. The returned response has its body
// already read and closed.
func (c *Client) attempt(ctx context.Context, method, path string, body []byte, headers map[string]string, authenticated bool, out any) (*http.Response, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// The same bytes are sent on every attempt: the body is never serialized again.
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(attemptCtx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, configError("the request could not be built.")
	}
	request.Header.Set("Accept", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	if authenticated {
		c.authorize(request)
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, c.transportError(ctx, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return nil, c.transportError(ctx, err)
	}

	if response.StatusCode < 200 || response.StatusCode > 299 {
		return nil, c.apiError(response, data)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return nil, &APIError{
			Status:  response.StatusCode,
			Code:    CodeUnexpectedResponse,
			Message: "The API answered with a body that is not the expected JSON.",
		}
	}
	return response, nil
}

// transportError turns a failure without HTTP response into ErrTimeout or ErrConnection.
func (c *Client) transportError(ctx context.Context, err error) *Error {
	if ctx.Err() != nil {
		return contextError(ctx, ctx.Err())
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return &Error{Kind: ErrTimeout, Message: fmt.Sprintf("no response from the GRUALINK API within %s.", c.timeout)}
	}
	// The cause is not wrapped: only its text is kept, with the key removed.
	return &Error{Kind: ErrConnection, Message: c.redact("could not reach the GRUALINK API: " + err.Error())}
}

// contextError reports that the context of the caller ended. It is never retried.
func contextError(ctx context.Context, err error) *Error {
	cause := ctx.Err()
	if cause == nil {
		cause = err
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return &Error{Kind: ErrTimeout, Message: "the context deadline passed before the GRUALINK API answered.", cause: context.DeadlineExceeded}
	}
	return &Error{Kind: ErrConnection, Message: "the call was cancelled by its context.", cause: context.Canceled}
}

// apiError builds the error for a non-2xx answer. The raw body is never echoed.
func (c *Client) apiError(response *http.Response, data []byte) *APIError {
	apiErr := &APIError{
		Status:            response.StatusCode,
		Code:              CodeUnexpectedResponse,
		Message:           fmt.Sprintf("The API answered HTTP %d.", response.StatusCode),
		RetryAfterSeconds: parseRetryAfter(response.Header.Get("Retry-After"), time.Now()),
	}
	// Not JSON (for example an HTML page from a proxy): keep the generic message.
	var envelope struct {
		Error map[string]json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return apiErr
	}
	code, ok := jsonString(envelope.Error["code"])
	if !ok {
		return apiErr
	}
	apiErr.Code = c.redact(code)
	if message, ok := jsonString(envelope.Error["message"]); ok {
		apiErr.Message = c.redact(message)
	}
	var details []json.RawMessage
	if err := json.Unmarshal(envelope.Error["details"], &details); err == nil {
		for _, raw := range details {
			var entry map[string]any
			if err := json.Unmarshal(raw, &entry); err != nil || entry == nil {
				continue
			}
			apiErr.Details = append(apiErr.Details, ErrorDetail{
				Field: c.redact(textOf(entry["field"])),
				Issue: c.redact(textOf(entry["issue"])),
			})
		}
	}
	return apiErr
}

// retryDelay is an exponential backoff with full jitter; Retry-After is a floor when the API sends it.
func (c *Client) retryDelay(attempt int, failure error) time.Duration {
	ceiling := c.retryBaseDelay
	for i := 1; i < attempt && ceiling < c.retryMaxDelay; i++ {
		ceiling *= 2
	}
	ceiling = min(ceiling, c.retryMaxDelay)
	delay := time.Duration(c.random() * float64(ceiling))
	var apiErr *APIError
	if errors.As(failure, &apiErr) && apiErr.RetryAfterSeconds != nil {
		delay += time.Duration(min(*apiErr.RetryAfterSeconds, maxRetryAfterSeconds)) * time.Second
	}
	return delay.Round(time.Millisecond)
}

// isRetryable is true only for network errors, timeouts, 5xx and 429 rate_limited.
func isRetryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		if apiErr.Status >= 500 {
			return true
		}
		// too_many_open_requests only clears when the company resolves a request: waiting a few
		// seconds does not help.
		return apiErr.Status == http.StatusTooManyRequests && apiErr.Code != "too_many_open_requests"
	}
	return errors.Is(err, ErrConnection) || errors.Is(err, ErrTimeout)
}

func setAttempts(err error, attempts int) {
	var apiErr *APIError
	var plain *Error
	switch {
	case errors.As(err, &apiErr):
		apiErr.Attempts = attempts
	case errors.As(err, &plain):
		plain.Attempts = attempts
	}
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// parseRetryAfter reads Retry-After as seconds or as an HTTP date.
func parseRetryAfter(value string, now time.Time) *int {
	text := strings.TrimSpace(value)
	if text == "" {
		return nil
	}
	if len(text) <= 9 && allDigits(text) {
		seconds, err := strconv.Atoi(text)
		if err != nil {
			return nil
		}
		return &seconds
	}
	date, err := http.ParseTime(text)
	if err != nil {
		return nil
	}
	seconds := 0
	if wait := date.Sub(now); wait > 0 {
		seconds = int((wait + time.Second - 1) / time.Second)
	}
	return &seconds
}

// keyMode returns test or live for a key of the form glk_test_... or glk_live_... without spaces.
func keyMode(key string) (string, bool) {
	for _, mode := range []string{"test", "live"} {
		rest, found := strings.CutPrefix(key, "glk_"+mode+"_")
		if found && rest != "" && strings.IndexFunc(rest, unicode.IsSpace) < 0 {
			return mode, true
		}
	}
	return "", false
}

func validIdempotencyKey(key string) bool {
	if len(key) < 1 || len(key) > 255 {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e {
			return false
		}
	}
	return true
}

func allDigits(text string) bool {
	if text == "" {
		return false
	}
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return false
		}
	}
	return true
}

// jsonString returns the value of raw when it is a JSON string.
func jsonString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return "", false
	}
	return text, true
}

func textOf(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	}
	return fmt.Sprint(value)
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
