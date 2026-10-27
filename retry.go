package resend

import (
	"context"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// Default values for RetryPolicy
const (
	defaultMaxRetries = 3
	defaultBaseDelay  = 500 * time.Millisecond
	defaultMaxDelay   = 30 * time.Second
)

// RetryPolicy configures RetryTransport. Zero values use the defaults.
type RetryPolicy struct {
	// MaxRetries is the number of retries after the first attempt (default 3)
	MaxRetries int

	// BaseDelay is the first backoff delay, doubled on each retry (default 500ms)
	BaseDelay time.Duration

	// MaxDelay caps each wait. A longer Retry-After is returned as an error instead (default 30s)
	MaxDelay time.Duration
}

// RetryTransport is an opt-in http.RoundTripper that retries failed requests.
//
// A 429 is retried after its Retry-After delay, since the API did not process
// the request. A 500, 502, 503, 504 or network error is retried with exponential
// backoff and jitter, only when the request is safe to repeat: GET, HEAD, OPTIONS,
// PUT and DELETE, or any request with an Idempotency-Key header. A POST without
// an Idempotency-Key is not retried after these errors, so an email is never sent twice.
//
// http.Client.Timeout covers all attempts.
//
//	client := resend.NewCustomClient(&http.Client{
//		Transport: resend.NewRetryTransport(nil, resend.RetryPolicy{}),
//	}, apiKey)
//
// See also https://resend.com/docs/api-reference/rate-limit
// See also https://resend.com/docs/dashboard/emails/idempotency-keys
type RetryTransport struct {
	base   http.RoundTripper
	policy RetryPolicy

	// sleep and jitter are replaced in tests
	sleep  func(ctx context.Context, d time.Duration) error
	jitter func(d time.Duration) time.Duration
}

// NewRetryTransport wraps the given transport, or http.DefaultTransport when nil
func NewRetryTransport(base http.RoundTripper, policy RetryPolicy) *RetryTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	if policy.MaxRetries <= 0 {
		policy.MaxRetries = defaultMaxRetries
	}
	if policy.BaseDelay <= 0 {
		policy.BaseDelay = defaultBaseDelay
	}
	if policy.MaxDelay <= 0 {
		policy.MaxDelay = defaultMaxDelay
	}
	return &RetryTransport{
		base:   base,
		policy: policy,
		sleep:  sleepCtx,
		jitter: equalJitter,
	}
}

// RoundTrip implements the http.RoundTripper interface
func (t *RetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	for attempt := 0; ; attempt++ {
		attemptReq, err := rewindRequest(req, attempt)
		if err != nil {
			return nil, err
		}

		resp, err := t.base.RoundTrip(attemptReq)
		if attempt >= t.policy.MaxRetries {
			return resp, err
		}

		wait, retry := t.retryDelay(req, resp, err, attempt)
		if !retry {
			return resp, err
		}

		// Drain the body so the connection can be reused
		if resp != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
		}

		if err := t.sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// retryDelay returns the wait before the next attempt and whether to retry
func (t *RetryTransport) retryDelay(req *http.Request, resp *http.Response, err error, attempt int) (time.Duration, bool) {
	if req.Context().Err() != nil {
		return 0, false
	}

	// The body cannot be sent again without GetBody
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		return 0, false
	}

	// Rate limited requests were not processed, so any method can be retried
	if err == nil && resp.StatusCode == http.StatusTooManyRequests {
		if d, ok := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); ok {
			return d, d <= t.policy.MaxDelay
		}
		return t.backoff(attempt), true
	}

	if !isRetryableFailure(resp, err) || !isSafeToRepeat(req) {
		return 0, false
	}
	return t.backoff(attempt), true
}

// backoff returns BaseDelay * 2^attempt with jitter, capped at MaxDelay
func (t *RetryTransport) backoff(attempt int) time.Duration {
	d := t.policy.MaxDelay
	if attempt < 32 {
		if exp := t.policy.BaseDelay << attempt; exp > 0 && exp < d {
			d = exp
		}
	}
	return t.jitter(d)
}

// isRetryableFailure reports whether the error or status code is transient
func isRetryableFailure(resp *http.Response, err error) bool {
	if err != nil {
		return true
	}
	switch resp.StatusCode {
	case http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// isSafeToRepeat reports whether sending the request again cannot cause a duplicate
func isSafeToRepeat(req *http.Request) bool {
	if req.Header.Get("Idempotency-Key") != "" {
		return true
	}
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

// rewindRequest returns the original request for the first attempt,
// and a copy with a fresh body for later attempts
func rewindRequest(req *http.Request, attempt int) (*http.Request, error) {
	if attempt == 0 || req.GetBody == nil {
		return req, nil
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	r := req.Clone(req.Context())
	r.Body = body
	return r, nil
}

// parseRetryAfter parses a Retry-After value in seconds or as an HTTP date
func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if at, err := http.ParseTime(v); err == nil {
		d := at.Sub(now)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// equalJitter returns a random delay between d/2 and d
func equalJitter(d time.Duration) time.Duration {
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + rand.N(half+1)
}

// sleepCtx waits for d or until the context is done
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
