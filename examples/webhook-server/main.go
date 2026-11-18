// Example webhook receiver with net/http, no framework.
//
//	GRUALINK_WEBHOOK_SECRET=<your whsec_ secret> go run ./examples/webhook-server
//
// Expose it over https (GRUALINK only delivers to https addresses on port 443) and give that address
// to the towing company. Ask them to send a test event (webhook.test) to check the connection.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/qubitoz/grualink-api-go/grualink"
)

const path = "/webhooks/grualink"

func main() {
	var secrets []string
	for _, name := range []string{"GRUALINK_WEBHOOK_SECRET", "GRUALINK_WEBHOOK_SECRET_PREVIOUS"} {
		if secret := os.Getenv(name); secret != "" {
			secrets = append(secrets, secret)
		}
	}
	if len(secrets) == 0 {
		fmt.Fprintln(os.Stderr, "Set GRUALINK_WEBHOOK_SECRET to your signing secret (it starts with whsec_). See .env.example.")
		os.Exit(1)
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8787"
	}

	// Stand-in for your own queue or database. Answer fast and do the real work elsewhere.
	queue := make(chan grualink.WebhookEvent, 256)
	go work(queue)

	handler := grualink.NewWebhookHandler(grualink.WebhookHandlerOptions{
		Secrets: secrets,
		OnEvent: func(_ context.Context, event grualink.WebhookEvent) error {
			select {
			case queue <- event:
				return nil
			default:
				// Returning an error answers 500 and the delivery is retried later.
				return errors.New("the queue is full")
			}
		},
		OnResult: func(result grualink.WebhookResult, request *http.Request) {
			// The reason stays in your logs; the caller only sees a generic answer.
			if result.Reason != "ok" {
				log.Printf("Delivery %q: %d (%s)", request.Header.Get(grualink.DeliveryHeader), result.Status, result.Reason)
			}
		},
	})

	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		MaxHeaderBytes:    16 * 1024,
	}
	log.Printf("Listening for GRUALINK webhooks on http://localhost:%s%s", port, path)
	log.Fatal(server.ListenAndServe())
}

func work(queue <-chan grualink.WebhookEvent) {
	for event := range queue {
		if sr := event.Data.ServiceRequest; sr != nil {
			reference := "no reference"
			if sr.ExternalRef != nil {
				reference = *sr.ExternalRef
			}
			log.Printf("[%s] %s: request %s (%s) is now %q", event.Mode, event.Type, sr.ID, reference, sr.Status)
		} else {
			log.Printf("[%s] %s: %s", event.Mode, event.Type, event.Data.Message)
		}
	}
}
