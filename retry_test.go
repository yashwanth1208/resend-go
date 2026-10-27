package resend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// retryServer replies with the given statuses in order, then 200, and records each request
type retryServer struct {
	mu       sync.Mutex
	statuses []int
	headers  []http.Header
	bodies   []string
	keys     []string
}

func (s *retryServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	n := len(s.bodies)
	s.bodies = append(s.bodies, string(body))
	s.keys = append(s.keys, r.Header.Get("Idempotency-Key"))
	status := http.StatusOK
	var h http.Header
	if n < len(s.statuses) {
		status = s.statuses[n]
		if n < len(s.headers) {
			h = s.headers[n]
		}
	}
	s.mu.Unlock()

	for k, v := range h {
		w.Header()[k] = v
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if status == http.StatusOK {
		fmt.Fprint(w, `{"id":"email_123"}`)
		return
	}
	fmt.Fprintf(w, `{"statusCode":%d,"name":"error","message":"status %d"}`, status, status)
}

func (s *retryServer) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

// newRetryClient returns a client whose RetryTransport records waits instead of sleeping
func newRetryClient(t *testing.T, srv *retryServer, policy RetryPolicy) (*Client, *[]time.Duration) {
	t.Helper()
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	rt := NewRetryTransport(nil, policy)
	waits := &[]time.Duration{}
	rt.sleep = func(ctx context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		return ctx.Err()
	}
	rt.jitter = func(d time.Duration) time.Duration { return d }

	c := NewCustomClient(&http.Client{Transport: rt}, "re_test")
	u, err := url.Parse(ts.URL + "/")
	require.NoError(t, err)
	c.BaseURL = u
	return c, waits
}

func sendParams() *SendEmailRequest {
	return &SendEmailRequest{From: "a@example.com", To: []string{"b@example.com"}, Subject: "hi", Text: "hello"}
}

func TestRetryTransport429HonorsRetryAfter(t *testing.T) {
	srv := &retryServer{
		statuses: []int{http.StatusTooManyRequests},
		headers:  []http.Header{{"Retry-After": {"2"}}},
	}
	c, waits := newRetryClient(t, srv, RetryPolicy{})

	// A 429 is retried even without an Idempotency-Key
	sent, err := c.Emails.Send(sendParams())
	require.NoError(t, err)
	assert.Equal(t, "email_123", sent.Id)
	assert.Equal(t, 2, srv.calls())
	assert.Equal(t, []time.Duration{2 * time.Second}, *waits)
	assert.Equal(t, srv.bodies[0], srv.bodies[1], "body must be replayed")
}

func TestRetryTransport429RetryAfterAboveMaxDelayReturnsError(t *testing.T) {
	srv := &retryServer{
		statuses: []int{http.StatusTooManyRequests},
		headers:  []http.Header{{"Retry-After": {"60"}}},
	}
	c, waits := newRetryClient(t, srv, RetryPolicy{MaxDelay: 10 * time.Second})

	_, err := c.Emails.Send(sendParams())
	var rl *RateLimitError
	require.ErrorAs(t, err, &rl)
	assert.Equal(t, "60", rl.RetryAfter)
	assert.Equal(t, 1, srv.calls())
	assert.Empty(t, *waits)
}

func TestRetryTransport5xxPostWithoutIdempotencyKeyIsNotRetried(t *testing.T) {
	srv := &retryServer{statuses: []int{http.StatusServiceUnavailable}}
	c, _ := newRetryClient(t, srv, RetryPolicy{})

	_, err := c.Emails.Send(sendParams())
	require.Error(t, err)
	assert.Equal(t, 1, srv.calls(), "unkeyed POST may have been processed")
}

func TestRetryTransport5xxPostWithIdempotencyKeyIsRetried(t *testing.T) {
	srv := &retryServer{statuses: []int{http.StatusInternalServerError, http.StatusBadGateway}}
	c, waits := newRetryClient(t, srv, RetryPolicy{BaseDelay: 100 * time.Millisecond})

	sent, err := c.Emails.SendWithOptions(context.Background(), sendParams(), &SendEmailOptions{IdempotencyKey: "key-1"})
	require.NoError(t, err)
	assert.Equal(t, "email_123", sent.Id)
	assert.Equal(t, 3, srv.calls())
	assert.Equal(t, []string{"key-1", "key-1", "key-1"}, srv.keys)
	assert.Equal(t, []time.Duration{100 * time.Millisecond, 200 * time.Millisecond}, *waits)
	assert.Equal(t, srv.bodies[0], srv.bodies[2])
}

func TestRetryTransportGetIsRetriedOn5xx(t *testing.T) {
	srv := &retryServer{statuses: []int{http.StatusGatewayTimeout}}
	c, _ := newRetryClient(t, srv, RetryPolicy{})

	_, err := c.Emails.Get("email_123")
	require.NoError(t, err)
	assert.Equal(t, 2, srv.calls())
}

func TestRetryTransportClientErrorsAreNotRetried(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusUnprocessableEntity} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := &retryServer{statuses: []int{status}}
			c, _ := newRetryClient(t, srv, RetryPolicy{})

			_, err := c.Emails.Get("email_123")
			require.Error(t, err)
			assert.Equal(t, 1, srv.calls())
		})
	}
}

func TestRetryTransportStopsAfterMaxRetries(t *testing.T) {
	srv := &retryServer{statuses: []int{503, 503, 503, 503, 503}}
	c, waits := newRetryClient(t, srv, RetryPolicy{MaxRetries: 2})

	_, err := c.Emails.Get("email_123")
	require.Error(t, err)
	assert.Equal(t, 3, srv.calls())
	assert.Len(t, *waits, 2)
}

func TestRetryTransportBackoffIsCapped(t *testing.T) {
	srv := &retryServer{statuses: []int{503, 503, 503}}
	c, waits := newRetryClient(t, srv, RetryPolicy{BaseDelay: time.Second, MaxDelay: 3 * time.Second})

	_, err := c.Emails.Get("email_123")
	require.NoError(t, err)
	assert.Equal(t, []time.Duration{time.Second, 2 * time.Second, 3 * time.Second}, *waits)
}

func TestRetryTransportStopsWhenContextIsCanceled(t *testing.T) {
	srv := &retryServer{statuses: []int{503, 503, 503}}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	rt := NewRetryTransport(nil, RetryPolicy{BaseDelay: time.Hour, MaxDelay: time.Hour})
	c := NewCustomClient(&http.Client{Transport: rt}, "re_test")
	c.BaseURL, _ = url.Parse(ts.URL + "/")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for srv.calls() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()

	start := time.Now()
	_, err := c.Emails.GetWithContext(ctx, "email_123")
	require.ErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Equal(t, 1, srv.calls())
}

type failingTransport struct{ n int }

func (f *failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.n++
	return nil, errors.New("connection reset")
}

func TestRetryTransportNetworkErrorRespectsMethodSafety(t *testing.T) {
	post := func(key string) int {
		ft := &failingTransport{}
		rt := NewRetryTransport(ft, RetryPolicy{MaxRetries: 2})
		rt.sleep = func(context.Context, time.Duration) error { return nil }
		req, _ := http.NewRequest(http.MethodPost, "http://example.invalid/emails", nil)
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		_, err := rt.RoundTrip(req)
		assert.Error(t, err)
		return ft.n
	}
	assert.Equal(t, 1, post(""))
	assert.Equal(t, 3, post("key-1"))
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"0", 0, true},
		{"7", 7 * time.Second, true},
		{"-1", 0, false},
		{"", 0, false},
		{"soon", 0, false},
		{now.Add(5 * time.Second).Format(http.TimeFormat), 5 * time.Second, true},
		{now.Add(-5 * time.Second).Format(http.TimeFormat), 0, true},
	}
	for _, c := range cases {
		got, ok := parseRetryAfter(c.in, now)
		assert.Equal(t, c.ok, ok, c.in)
		assert.Equal(t, c.want, got, c.in)
	}
}

func TestEqualJitterBounds(t *testing.T) {
	d := 100 * time.Millisecond
	for range 1000 {
		j := equalJitter(d)
		assert.GreaterOrEqual(t, j, d/2)
		assert.LessOrEqual(t, j, d)
	}
}
