package models

import (
	"context"
	"encoding/json"
	"sync"
)

// Fake is a scripted Decider and Generator for tests. It is safe for concurrent use.
type Fake struct {
	NameValue string
	// DecideFunc answers Decide; nil answers "none" with confidence 1.
	DecideFunc func(req DecideRequest) (Decision, Usage, error)
	// GenerateFunc answers Generate, usually by unmarshalling canned JSON into out.
	GenerateFunc func(system, user string, schema json.RawMessage, out any) (Usage, error)

	mu       sync.Mutex
	requests []DecideRequest
}

// Name implements Decider.
func (f *Fake) Name() string { return f.NameValue }

// Decide implements Decider and remembers the request.
func (f *Fake) Decide(_ context.Context, req DecideRequest) (Decision, Usage, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if f.DecideFunc == nil {
		return Decision{Confidence: 1}, Usage{Provider: "fake", Model: f.NameValue}, nil
	}
	return f.DecideFunc(req)
}

// Generate implements Generator.
func (f *Fake) Generate(_ context.Context, system, user string, schema json.RawMessage, out any) (Usage, error) {
	return f.GenerateFunc(system, user, schema, out)
}

// Requests returns every request Decide has seen, in order.
func (f *Fake) Requests() []DecideRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]DecideRequest(nil), f.requests...)
}
