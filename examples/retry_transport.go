package examples

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/resend/resend-go/v4"
)

func retryTransportExample() {
	ctx := context.TODO()
	apiKey := os.Getenv("RESEND_API_KEY")

	// Retry 429s after Retry-After, and 5xx or network errors with backoff
	client := resend.NewCustomClient(&http.Client{
		Timeout: 2 * time.Minute, // covers all attempts
		Transport: resend.NewRetryTransport(nil, resend.RetryPolicy{
			MaxRetries: 3,
			BaseDelay:  500 * time.Millisecond,
			MaxDelay:   30 * time.Second,
		}),
	}, apiKey)

	params := &resend.SendEmailRequest{
		To:      []string{"delivered@resend.dev"},
		From:    "onboarding@resend.dev",
		Text:    "hello world",
		Subject: "Hello from Golang",
	}

	// The idempotency key makes 5xx retries safe, so the email is sent only once
	sent, err := client.Emails.SendWithOptions(ctx, params, &resend.SendEmailOptions{
		IdempotencyKey: "welcome-user-123",
	})
	if err != nil {
		panic(err)
	}
	fmt.Printf("Email sent: %s\n", sent.Id)
}
