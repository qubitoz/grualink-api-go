package grualink

import (
	"container/list"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Headers of a webhook delivery. Header names are case-insensitive.
const (
	SignatureHeader = "X-GruaLink-Signature"
	DeliveryHeader  = "X-GruaLink-Delivery"
	EventHeader     = "X-GruaLink-Event"
	TimestampHeader = "X-GruaLink-Timestamp"
	ModeHeader      = "X-GruaLink-Mode"
)

const (
	// DefaultToleranceSeconds is the tolerance of the signed timestamp: deliveries that differ more
	// than this from the current time are rejected.
	DefaultToleranceSeconds = 300
	// MaxWebhookBodyBytes is the largest body of a delivery (64 KB).
	MaxWebhookBodyBytes = 64 * 1024
)

// SignatureVerificationReason is the outcome of VerifyWebhookSignature.
type SignatureVerificationReason string

// The six reasons, exactly as in the contract.
const (
	ReasonOK                      SignatureVerificationReason = "ok"
	ReasonMissingSecret           SignatureVerificationReason = "missing_secret"
	ReasonMissingHeader           SignatureVerificationReason = "missing_header"
	ReasonMalformedHeader         SignatureVerificationReason = "malformed_header"
	ReasonTimestampOutOfTolerance SignatureVerificationReason = "timestamp_out_of_tolerance"
	ReasonSignatureMismatch       SignatureVerificationReason = "signature_mismatch"
)

// VerifyWebhookSignatureInput is the input of VerifyWebhookSignature.
type VerifyWebhookSignatureInput struct {
	// RawBody is the RAW request body exactly as received, before any JSON parsing.
	RawBody []byte
	// SignatureHeader is the value of the X-GruaLink-Signature header ("" when absent).
	SignatureHeader string
	// Secrets holds your signing secret, whole (with its whsec_ prefix). Pass two during your own
	// rotation window. Empty strings are ignored.
	Secrets []string
	// NowSeconds is the current time in seconds since 1970 (UTC). Zero means the system clock.
	NowSeconds int64
	// ToleranceSeconds defaults to 300 when zero.
	ToleranceSeconds int64
}

// SignatureVerification is the result of VerifyWebhookSignature.
type SignatureVerification struct {
	Valid  bool                        `json:"valid"`
	Reason SignatureVerificationReason `json:"reason"`
}

func rejectSignature(reason SignatureVerificationReason) SignatureVerification {
	return SignatureVerification{Valid: false, Reason: reason}
}

// ComputeWebhookSignature returns HMAC-SHA256, in lowercase hexadecimal, of "<timestamp>.<raw body>"
// keyed with the secret exactly as issued.
func ComputeWebhookSignature(secret string, timestamp int64, rawBody []byte) string {
	return hex.EncodeToString(signatureOf(secret, strconv.FormatInt(timestamp, 10), rawBody))
}

// BuildSignatureHeader builds the header the way GRUALINK sends it. Useful to test your own receiver
// locally.
func BuildSignatureHeader(secrets []string, timestamp int64, rawBody []byte) string {
	parts := []string{"t=" + strconv.FormatInt(timestamp, 10)}
	for _, secret := range secrets {
		parts = append(parts, "v1="+ComputeWebhookSignature(secret, timestamp, rawBody))
	}
	return strings.Join(parts, ",")
}

// signatureOf signs with the timestamp as the digits received, not as a number.
func signatureOf(secret, timestampText string, rawBody []byte) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestampText))
	mac.Write([]byte{'.'})
	mac.Write(rawBody)
	return mac.Sum(nil)
}

// VerifyWebhookSignature verifies X-GruaLink-Signature. It is pure and total: it never panics and
// has no side effects.
//
// Order of checks (shared by every language; the contract vectors pin it):
//  1. no secret                                      -> missing_secret
//  2. header absent or blank                         -> missing_header
//  3. no t of 1-12 digits, or no well-formed v1      -> malformed_header
//  4. |now - t| greater than the tolerance           -> timestamp_out_of_tolerance
//  5. no v1 equals the expected signature            -> signature_mismatch
func VerifyWebhookSignature(input VerifyWebhookSignatureInput) SignatureVerification {
	var secrets []string
	for _, secret := range input.Secrets {
		if secret != "" {
			secrets = append(secrets, secret)
		}
	}
	if len(secrets) == 0 {
		return rejectSignature(ReasonMissingSecret)
	}

	if strings.TrimSpace(input.SignatureHeader) == "" {
		return rejectSignature(ReasonMissingHeader)
	}

	timestampText, hasTimestamp := "", false
	var candidates [][]byte
	for _, part := range strings.Split(input.SignatureHeader, ",") {
		name, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		switch {
		case name == "t" && !hasTimestamp:
			timestampText, hasTimestamp = value, true
		case name == "v1" && isSignatureV1(value):
			// Anything that is not exactly 64 lowercase hex characters is ignored, as are
			// unknown schemes.
			candidates = append(candidates, []byte(value))
		}
	}
	if !hasTimestamp || len(timestampText) > 12 || !allDigits(timestampText) {
		return rejectSignature(ReasonMalformedHeader)
	}
	if len(candidates) == 0 {
		return rejectSignature(ReasonMalformedHeader)
	}
	timestamp, err := strconv.ParseInt(timestampText, 10, 64)
	if err != nil {
		return rejectSignature(ReasonMalformedHeader)
	}

	now := float64(time.Now().UnixMilli()) / 1000
	if input.NowSeconds != 0 {
		now = float64(input.NowSeconds)
	}
	tolerance := input.ToleranceSeconds
	if tolerance == 0 {
		tolerance = DefaultToleranceSeconds
	}
	if math.Abs(now-float64(timestamp)) > float64(tolerance) {
		return rejectSignature(ReasonTimestampOutOfTolerance)
	}

	matched := false
	// Every pair is compared in constant time and the loop never exits early.
	for _, secret := range secrets {
		expected := make([]byte, hex.EncodedLen(sha256.Size))
		hex.Encode(expected, signatureOf(secret, timestampText, input.RawBody))
		for _, candidate := range candidates {
			if hmac.Equal(candidate, expected) {
				matched = true
			}
		}
	}
	if !matched {
		return rejectSignature(ReasonSignatureMismatch)
	}
	return SignatureVerification{Valid: true, Reason: ReasonOK}
}

func isSignatureV1(value string) bool {
	if len(value) != 64 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// DeliveryDeduplicator remembers which deliveries were already processed. Implement it over a shared
// store when you run several instances (a unique key in your database is enough).
type DeliveryDeduplicator interface {
	// Claim returns true the first time an id is seen and false for a duplicate.
	Claim(ctx context.Context, deliveryID string) (bool, error)
	// Release forgets an id so a later retry is processed again (called when processing failed).
	Release(ctx context.Context, deliveryID string) error
}

// InMemoryDeliveryDeduplicatorOptions configures an InMemoryDeliveryDeduplicator. Zero values mean
// the defaults.
type InMemoryDeliveryDeduplicatorOptions struct {
	// TTL is how long an id is remembered (default 48 hours).
	TTL time.Duration
	// MaxEntries caps the ids kept (default 50,000). The oldest are evicted first.
	MaxEntries int
	// Now is the clock, for tests.
	Now func() time.Time
}

type claimedDelivery struct {
	id        string
	expiresAt time.Time
}

// InMemoryDeliveryDeduplicator is an in-memory deduplication for a single process. Retries of one
// delivery span up to 24 hours, so ids are kept for 48 hours by default. It is safe for concurrent use.
type InMemoryDeliveryDeduplicator struct {
	mu         sync.Mutex
	ttl        time.Duration
	maxEntries int
	now        func() time.Time
	// order holds the claimed ids from oldest to newest; seen finds them by id.
	order *list.List
	seen  map[string]*list.Element
}

// NewInMemoryDeliveryDeduplicator returns an empty deduplicator.
func NewInMemoryDeliveryDeduplicator(options InMemoryDeliveryDeduplicatorOptions) *InMemoryDeliveryDeduplicator {
	d := &InMemoryDeliveryDeduplicator{
		ttl:        options.TTL,
		maxEntries: options.MaxEntries,
		now:        options.Now,
		order:      list.New(),
		seen:       make(map[string]*list.Element),
	}
	if d.ttl <= 0 {
		d.ttl = 48 * time.Hour
	}
	if d.maxEntries <= 0 {
		d.maxEntries = 50_000
	}
	if d.now == nil {
		d.now = time.Now
	}
	return d
}

// Claim returns true the first time an id is seen and false while it is remembered.
func (d *InMemoryDeliveryDeduplicator) Claim(_ context.Context, deliveryID string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	// Drop from the oldest: what expired, and whatever is needed to stay under the cap.
	for element := d.order.Front(); element != nil; element = d.order.Front() {
		entry := element.Value.(claimedDelivery)
		if entry.expiresAt.After(now) && len(d.seen) < d.maxEntries {
			break
		}
		d.remove(element)
	}
	if element, found := d.seen[deliveryID]; found {
		if element.Value.(claimedDelivery).expiresAt.After(now) {
			return false, nil
		}
		d.remove(element)
	}
	d.seen[deliveryID] = d.order.PushBack(claimedDelivery{id: deliveryID, expiresAt: now.Add(d.ttl)})
	return true, nil
}

// Release forgets an id.
func (d *InMemoryDeliveryDeduplicator) Release(_ context.Context, deliveryID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if element, found := d.seen[deliveryID]; found {
		d.remove(element)
	}
	return nil
}

func (d *InMemoryDeliveryDeduplicator) remove(element *list.Element) {
	delete(d.seen, element.Value.(claimedDelivery).id)
	d.order.Remove(element)
}
