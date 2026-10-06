package models

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Caller runs model calls under one global concurrency cap, with a per-attempt
// timeout and retries on 429 and 5xx. Share one Caller between all adapters.
type Caller struct {
	sem     chan struct{}
	Timeout time.Duration // per attempt
	Retries int           // extra attempts after the first
	Backoff time.Duration // wait before the first retry; doubles each time
	Sleep   func(ctx context.Context, d time.Duration) error
}

// NewCaller allows at most concurrency calls in flight (MAILRULES_MODEL_CONCURRENCY).
func NewCaller(concurrency int) *Caller {
	return &Caller{
		sem:     make(chan struct{}, max(concurrency, 1)),
		Timeout: 15 * time.Second,
		Retries: 2,
		Backoff: 500 * time.Millisecond,
		Sleep: func(ctx context.Context, d time.Duration) error {
			select {
			case <-time.After(d):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
}

// Do runs fn, retrying it while it returns a StatusError with 429 or 5xx.
func (c *Caller) Do(ctx context.Context, fn func(ctx context.Context) error) error {
	for attempt := 0; ; attempt++ {
		err := c.once(ctx, fn)
		var se *StatusError
		if err == nil || attempt >= c.Retries || !errors.As(err, &se) ||
			(se.Code != http.StatusTooManyRequests && se.Code < 500) {
			return err
		}
		if err := c.Sleep(ctx, c.Backoff<<attempt); err != nil {
			return err
		}
	}
}

// once holds a concurrency slot for one attempt only, not while backing off.
func (c *Caller) once(ctx context.Context, fn func(ctx context.Context) error) error {
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.sem }()
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	return fn(ctx)
}

// postJSON is the one HTTP round trip used by the adapters that have no SDK.
// header carries the credentials; they never reach an error string.
func postJSON(ctx context.Context, hc *http.Client, provider, url string, header http.Header, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("%s: encode request: %w", provider, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("%s: build request: %w", provider, err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", provider, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%s: read response: %w", provider, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &StatusError{Provider: provider, Code: resp.StatusCode}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s: %w: response is not the expected JSON", provider, ErrBadOutput)
	}
	return nil
}
