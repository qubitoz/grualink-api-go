# GRUALINK API: Go example

[Español](README.md)

A clone-and-run example that integrates your system (insurer, roadside assistance, integrator) with the
GRUALINK API: **request a tow truck**, **check its status** and **receive signed webhooks**.

Public API guide: <https://grualink.mx/api>

What is inside:

- A **minimal client** (`grualink/client.go`): creates requests with `Idempotency-Key`, reads the
  status, typed errors, retries with exponential backoff and timeouts through `context.Context`.
- A **webhook receiver** (`grualink/webhooks.go`, `grualink/webhook_handler.go`): an `http.Handler`
  that verifies the signature over the raw body, checks the time window, supports secret rotation and
  drops duplicates.
- Two **runnable examples** (`examples/`).
- The **contract kit** (`contract/`): the API contract in Spanish and English and the signature test
  cases shared by the examples in every language.

It uses only the standard library. It is not an SDK: it is code meant to be read and copied into your
project.

## Requirements

- Go 1.22 or later.
- A **test key** (`glk_test_…`). The towing company you work with gives it to you.
- For webhooks: the **signing secret** (`whsec_…`) the same company gives you when it registers the
  address of your system.

## Installation

```bash
git clone <this repository's address>
cd grualink-api-go
go build ./...
```

There are no dependencies to download.

## Environment variables

Names only; the values live on your machine and never in the repository. There is a template in
`.env.example` (the examples read the process environment; they do not load `.env` files).

| Variable | Purpose |
|---|---|
| `GRUALINK_API_KEY` | Your **test** key (`glk_test_…`) |
| `GRUALINK_WEBHOOK_SECRET` | Webhook signing secret (`whsec_…`), whole, with its prefix |
| `GRUALINK_WEBHOOK_SECRET_PREVIOUS` | Optional: the previous secret while a rotation is in progress |
| `GRUALINK_API_BASE_URL` | Optional: another base URL (default `https://grualink.mx/api/v1`) |
| `PORT` | Optional: port of the example receiver (default 8787) |

## Running the examples

### Request a tow truck

```bash
export GRUALINK_API_KEY=...   # your test key
go run ./examples/request-tow
```

With fictitious data, the example sends a request, prints the estimated price, sends it again with the
same `Idempotency-Key` to show nothing is duplicated, and reads its status. It only runs with test keys.

With a test key, the beginning of `external_ref` decides the final status: `TEST-CONFIRMAR`,
`TEST-EN-CURSO`, `TEST-TERMINAR`, `TEST-RECHAZAR`, `TEST-CANCELAR`.

### Receive webhooks

```bash
export GRUALINK_WEBHOOK_SECRET=...   # your signing secret
go run ./examples/webhook-server
```

It listens on `http://localhost:8787/webhooks/grualink`. GRUALINK only delivers to public `https`
addresses on port 443: publish the receiver behind your domain (or an `https` tunnel while you test),
give that address to the company and ask them to send you a test event.

## Using it in your code

```go
client, err := grualink.NewClient(os.Getenv("GRUALINK_API_KEY"))
if err != nil {
	return err
}

// The context bounds the whole call, retries and waits included; each attempt also has its own
// timeout (15 s; change it with grualink.WithTimeout).
ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
defer cancel()

result, err := client.CreateServiceRequest(ctx, grualink.ServiceRequestInput{
	ExternalRef: "TEST-CONFIRMAR-0001",
	Contact:     grualink.Contact{Name: "Persona de Ejemplo", Phone: "+525500000000"},
	Pickup: grualink.Place{
		Address: "Av. Juárez 100, Centro, Guadalajara, Jal.",
		Lat:     grualink.Float64(20.6767),
		Lng:     grualink.Float64(-103.3475),
	},
	Destination: &grualink.Place{
		Address: "Av. Patria 500, Zapopan, Jal.",
		Lat:     grualink.Float64(20.7214),
		Lng:     grualink.Float64(-103.3918),
	},
}, grualink.CreateServiceRequestOptions{IdempotencyKey: myRecord.IdempotencyKey}) // store it before calling

var apiErr *grualink.APIError
switch {
case errors.Is(err, grualink.ErrValidation) && errors.As(err, &apiErr):
	log.Println(apiErr.Details)
case err != nil:
	return err
default:
	sr := result.ServiceRequest
	log.Println(sr.ID, sr.Status, result.Replayed)
}
```

The other operations: `client.GetServiceRequest(ctx, id)`,
`client.FindServiceRequestsByExternalRef(ctx, reference)` and `client.Health(ctx)`.

### Idempotency and retries

- Every request carries an `Idempotency-Key` of yours. If you repeat the call with the same key and the
  same body within 24 hours, the API answers **`200`** with `Idempotent-Replayed: true` and creates
  nothing (`Replayed` is `true`). The same key with a different body answers `409`.
- The client retries only what makes sense: network errors, timeouts, `5xx` and `429 rate_limited`
  (waiting what `Retry-After` says). It uses exponential backoff with jitter and always resends the same
  key and the same bytes: the body is serialized only once.
- If the context you pass is cancelled or expires, the call ends at once and is not retried.
- It does not follow redirects.
- The key never appears in errors or logs, nor when the client is printed.

### Verifying a webhook

```go
result := grualink.VerifyWebhookSignature(grualink.VerifyWebhookSignatureInput{
	RawBody:         rawBody,                                       // the bytes exactly as received
	SignatureHeader: r.Header.Get(grualink.SignatureHeader),
	Secrets:         []string{os.Getenv("GRUALINK_WEBHOOK_SECRET")}, // whole, with whsec_
})
// result.Valid, result.Reason
```

What matters:

1. Verify the **raw body**, before parsing it as JSON. If you parse it and serialize it again, the
   signature no longer matches.
2. Use the **whole** secret, with its `whsec_` prefix.
3. Reject timestamps that differ by more than 5 minutes.
4. After a secret rotation, two `v1` signatures arrive for 24 hours: accept if either matches.
5. Webhooks can repeat: keep the `id` of the ones you processed and answer `2xx` without reprocessing.
6. Answer `2xx` within 10 seconds and do the heavy work afterwards.

### Mounting the receiver

`grualink.NewWebhookHandler` returns an `http.Handler`. With `net/http`:

```go
handler := grualink.NewWebhookHandler(grualink.WebhookHandlerOptions{
	Secrets: []string{os.Getenv("GRUALINK_WEBHOOK_SECRET")},
	OnEvent: func(ctx context.Context, event grualink.WebhookEvent) error {
		return storeInMyQueue(ctx, event) // fast; if it returns an error the answer is 500 and the webhook is retried
	},
})

mux := http.NewServeMux()
mux.Handle("/webhooks/grualink", handler)
```

The delicate part is getting the **raw body**: nothing may read or parse the body before the receiver.
Because it is an `http.Handler`, it mounts the same way on the usual routers; for example, with chi
`r.Method(http.MethodPost, "/webhooks/grualink", handler)` and with Gin
`router.POST("/webhooks/grualink", gin.WrapH(handler))`, without a middleware that reads the body on
that route. If your framework does not accept an `http.Handler`, read the bytes yourself and call
`handler.HandleDelivery(ctx, rawBody, header)`, which returns the status and the body of the answer.

With several instances, replace `InMemoryDeliveryDeduplicator` with an implementation of
`DeliveryDeduplicator` over your database (a unique key on the event `id` is enough).

## Errors

Every API error has the shape `{ "error": { "code", "message", "details"? } }`. `code` is stable;
branch on it, not on `message`.

In Go, errors are told apart with `errors.Is` against the values of the table, and the data of the
answer (`Status`, `Code`, `Message`, `Details`, `RetryAfterSeconds`, `Attempts`) is read with
`errors.As` on `*grualink.APIError`. Every error of the package satisfies
`errors.Is(err, grualink.ErrGrualink)` and every error answered by the API also satisfies
`errors.Is(err, grualink.ErrAPI)`.

| HTTP | `code` | What happened | `errors.Is` | Retried? |
|---|---|---|---|---|
| 400 | `invalid_request` | The request is not valid | `ErrInvalidRequest` | no |
| 400 | `invalid_json` | The body is not valid JSON | `ErrInvalidRequest` | no |
| 400 | `key_in_query` | The key was sent in the URL; have it revoked | `ErrInvalidRequest` | no |
| 400 | `idempotency_key_required` | `Idempotency-Key` is missing | `ErrInvalidRequest` | no |
| 400 | `invalid_idempotency_key` | Invalid `Idempotency-Key` (1 to 255 visible ASCII characters) | `ErrInvalidRequest` | no |
| 401 | `unauthorized` | Key missing, invalid, expired or revoked | `ErrAuthentication` | no |
| 403 | `insufficient_scope` | The key is not allowed to do this | `ErrPermission` | no |
| 403 | `customer_unavailable` | Your customer account with the company is not available | `ErrPermission` | no |
| 403 | `live_mode_unavailable` | Live requests are not enabled yet | `ErrPermission` | no |
| 403 | `browser_not_allowed` | A live key was used from a browser | `ErrPermission` | no |
| 404 | `not_found` | It does not exist or is not yours | `ErrNotFound` | no |
| 405 | `method_not_allowed` | Method not allowed for this path | `ErrInvalidRequest` | no |
| 409 | `idempotency_conflict` | That `Idempotency-Key` was already used with another body | `ErrIdempotencyConflict` | no |
| 413 | `payload_too_large` | The body exceeds 16 KB | `ErrInvalidRequest` | no |
| 415 | `unsupported_media_type` | The body must be sent as `application/json` | `ErrInvalidRequest` | no |
| 422 | `validation_error` | Invalid fields; `Details` says which | `ErrValidation` | no |
| 429 | `rate_limited` | Too many requests; wait `Retry-After` | `ErrRateLimit` | yes |
| 429 | `too_many_open_requests` | Too many unresolved requests with the company | `ErrRateLimit` | no |
| 500 | `internal_error` | Internal error | `ErrServer` | yes |

Also, with the type `*grualink.Error`: `ErrConnection` (no response, retried), `ErrTimeout` (timed out,
retried) and `ErrConfig` (invalid configuration or argument; nothing was sent). When the context you
passed ends, the error also satisfies `errors.Is(err, context.Canceled)` or
`errors.Is(err, context.DeadlineExceeded)`.

An error answer whose body is not the documented shape has the code `unexpected_response`.

The full contract, with fields, limits and the order of checks, is in
[`contract/API.md`](contract/API.md).

### Contract names in Go

The contract names the operations the same way in every language. In Go they are used without
repeating the package name:

| Contract | Go |
|---|---|
| `GrualinkClient` | `grualink.Client` (`grualink.NewClient`) |
| `createServiceRequest`, `getServiceRequest`, `findServiceRequestsByExternalRef`, `health` | `Client.CreateServiceRequest`, `Client.GetServiceRequest`, `Client.FindServiceRequestsByExternalRef`, `Client.Health` |
| `verifyWebhookSignature`, `computeWebhookSignature`, `buildSignatureHeader` | `grualink.VerifyWebhookSignature`, `grualink.ComputeWebhookSignature`, `grualink.BuildSignatureHeader` |
| `DeliveryDeduplicator`, `InMemoryDeliveryDeduplicator` | `grualink.DeliveryDeduplicator` (`Claim`, `Release`), `grualink.InMemoryDeliveryDeduplicator` |
| `createWebhookHandler` | `grualink.NewWebhookHandler` |
| `GrualinkError` | `grualink.ErrGrualink` |
| `GrualinkConfigError`, `GrualinkConnectionError`, `GrualinkTimeoutError` | `grualink.ErrConfig`, `grualink.ErrConnection`, `grualink.ErrTimeout` (type `*grualink.Error`) |
| `GrualinkApiError` | `grualink.ErrAPI` (type `*grualink.APIError`) |
| `GrualinkInvalidRequestError`, `GrualinkAuthenticationError`, `GrualinkPermissionError`, `GrualinkNotFoundError` | `grualink.ErrInvalidRequest`, `grualink.ErrAuthentication`, `grualink.ErrPermission`, `grualink.ErrNotFound` |
| `GrualinkIdempotencyConflictError`, `GrualinkValidationError`, `GrualinkRateLimitError`, `GrualinkServerError` | `grualink.ErrIdempotencyConflict`, `grualink.ErrValidation`, `grualink.ErrRateLimit`, `grualink.ErrServer` |

## Tests

```bash
go test -race ./...   # tests of the package and of the two examples
go vet ./...
gofmt -l .            # must list nothing
```

The tests never call the real API: they use local fake servers (`net/http/httptest`). They cover every
case of `contract/signature-vectors.json`, each error code, retries, idempotency and that the key never
appears in errors or logs. `examples/examples_test.go` builds and runs the two examples against a fake
server.

## Layout

```
contract/     API contract (es/en), signature test cases and checklist
grualink/     client, errors, types, webhook verification and receiver, with their tests
examples/     request-tow and webhook-server, and the test that runs them
```

The `contract/` folder is an unchanged copy of the one shared by the examples in every language; it is
not edited here.

## License

[MIT](LICENSE)
