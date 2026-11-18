// End-to-end example: request a tow truck with fictitious data, print the quote and check the status.
//
//	GRUALINK_API_KEY=<your TEST key> go run ./examples/request-tow
//
// Use a TEST key (glk_test_...). Test requests never become real services.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/qubitoz/grualink-api-go/grualink"
)

func main() {
	apiKey := os.Getenv("GRUALINK_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "Set GRUALINK_API_KEY to your TEST key (it starts with glk_test_). See .env.example.")
		os.Exit(1)
	}
	if !strings.HasPrefix(apiKey, "glk_test_") {
		fmt.Fprintln(os.Stderr, "This example only runs with a TEST key (glk_test_...).")
		os.Exit(1)
	}
	if err := run(apiKey); err != nil {
		fmt.Fprintln(os.Stderr, describe(err))
		os.Exit(1)
	}
}

func run(apiKey string) error {
	options := []grualink.Option{
		grualink.WithOnRetry(func(info grualink.RetryInfo) {
			fmt.Fprintf(os.Stderr, "Attempt %d failed (%v); retrying in %s\n", info.Attempt, info.Err, info.Delay)
		}),
	}
	if baseURL := os.Getenv("GRUALINK_API_BASE_URL"); baseURL != "" {
		options = append(options, grualink.WithBaseURL(baseURL))
	}
	client, err := grualink.NewClient(apiKey, options...)
	if err != nil {
		return err
	}

	// The context bounds the whole run, retries and waits included.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// With a test key, the prefix of external_ref decides the final status: TEST-CONFIRMAR -> confirmed,
	// TEST-EN-CURSO -> in_progress, TEST-TERMINAR -> completed, TEST-RECHAZAR -> rejected,
	// TEST-CANCELAR -> cancelled.
	suffix, err := randomHex(4)
	if err != nil {
		return err
	}
	request := grualink.ServiceRequestInput{
		ExternalRef: "TEST-CONFIRMAR-" + suffix,
		Contact:     grualink.Contact{Name: "Persona de Ejemplo", Phone: "+525500000000"},
		Vehicle:     &grualink.Vehicle{Make: "Nissan", Model: "Versa", Year: 2019, Color: "Gris", Plate: "ABC123A", Type: "auto"},
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
		Service: &grualink.ServiceDetails{Reason: "No arranca", Category: "vial", RequiredClass: "A"},
		Notes:   "Datos ficticios de ejemplo.",
	}

	health, err := client.Health(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("API status: %s (%s)\n", health.Status, health.Version)

	// In your system, store this key next to your own record BEFORE calling, and reuse it if you have
	// to retry.
	idempotencyKey, err := randomHex(16)
	if err != nil {
		return err
	}
	createOptions := grualink.CreateServiceRequestOptions{IdempotencyKey: idempotencyKey}

	created, err := client.CreateServiceRequest(ctx, request, createOptions)
	if err != nil {
		return err
	}
	sr := created.ServiceRequest
	fmt.Printf("\nCreated %s (%s) with status %q, reference %s\n", sr.ID, sr.Mode, sr.Status, text(sr.ExternalRef, "none"))
	printQuote(sr)

	// Sending it again with the same key and the same body creates nothing new.
	again, err := client.CreateServiceRequest(ctx, request, createOptions)
	if err != nil {
		return err
	}
	fmt.Printf("\nSent again with the same Idempotency-Key: replayed=%t, same id=%t\n", again.Replayed, again.ServiceRequest.ID == sr.ID)

	current, err := client.GetServiceRequest(ctx, sr.ID)
	if err != nil {
		return err
	}
	fmt.Printf("\nStatus now: %q", current.Status)
	if current.Service != nil && current.Service.Folio != nil {
		fmt.Printf(", folio %s", *current.Service.Folio)
	}
	fmt.Println()

	byRef, err := client.FindServiceRequestsByExternalRef(ctx, request.ExternalRef)
	if err != nil {
		return err
	}
	fmt.Printf("Requests with reference %s: %d\n", request.ExternalRef, len(byRef))
	return nil
}

func printQuote(sr grualink.ServiceRequest) {
	price := sr.Quote.EstimatedPrice
	if price == nil {
		fmt.Printf("  No estimated price (%s); the company will quote it.\n", text(sr.Quote.UnpricedReason, "unknown reason"))
		return
	}
	fmt.Printf("  Subtotal:  %.2f %s\n", price.Subtotal, price.Currency)
	for _, tax := range price.Taxes {
		sign := "+"
		if tax.Type == "retention" {
			sign = "-"
		}
		fmt.Printf("  %s %s (%g%%): %.2f\n", sign, tax.Name, tax.Rate, tax.Amount)
	}
	fmt.Printf("  Total:     %.2f %s (estimate, extras not included)\n", price.Total, price.Currency)
}

// describe turns an error into one line for the terminal. Branch on the kind and on the code, never
// on the message.
func describe(err error) string {
	var apiErr *grualink.APIError
	switch {
	case errors.Is(err, grualink.ErrValidation) && errors.As(err, &apiErr):
		fields := make([]string, 0, len(apiErr.Details))
		for _, detail := range apiErr.Details {
			fields = append(fields, fmt.Sprintf("%s (%s)", detail.Field, detail.Issue))
		}
		return "Invalid fields: " + strings.Join(fields, ", ")
	case errors.As(err, &apiErr):
		return fmt.Sprintf("API error %d %s: %s", apiErr.Status, apiErr.Code, apiErr.Message)
	case errors.Is(err, grualink.ErrTimeout):
		return "Timeout: " + err.Error()
	case errors.Is(err, grualink.ErrConnection):
		return "Connection error: " + err.Error()
	}
	return err.Error()
}

func text(value *string, fallback string) string {
	if value == nil || *value == "" {
		return fallback
	}
	return *value
}

func randomHex(bytes int) (string, error) {
	buffer := make([]byte, bytes)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}
