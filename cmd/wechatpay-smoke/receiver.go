package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/flexprice/flexprice/internal/types"
	"github.com/flexprice/flexprice/internal/webhook/payload"
)

// deliveredWebhook is one outbound webhook that reached the isolated receiver,
// after being rendered by the production payload builders.
type deliveredWebhook struct {
	EventName string
	EntityID  string
	Body      []byte
	Status    int
	Error     string
}

// isolatedReceiver is a loopback HTTP endpoint standing in for a tenant's
// configured webhook endpoint. It is the only "network" the harness uses for
// outbound events.
type isolatedReceiver struct {
	srv *httptest.Server

	mu        sync.Mutex
	delivered []deliveredWebhook
}

func newIsolatedReceiver() *isolatedReceiver {
	r := &isolatedReceiver{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body := make([]byte, 0)
		if req.Body != nil {
			defer req.Body.Close()
			buf := new(bytes.Buffer)
			if _, err := buf.ReadFrom(req.Body); err == nil {
				body = buf.Bytes()
			}
		}
		r.mu.Lock()
		r.delivered = append(r.delivered, deliveredWebhook{
			EventName: req.Header.Get("X-Flexprice-Event"),
			Body:      body,
			Status:    http.StatusOK,
		})
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	return r
}

func (r *isolatedReceiver) URL() string { return r.srv.URL }
func (r *isolatedReceiver) Close()      { r.srv.Close() }

func (r *isolatedReceiver) Delivered() []deliveredWebhook {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]deliveredWebhook, len(r.delivered))
	copy(out, r.delivered)
	return out
}

// capturingPublisher plays the role the production WebhookPublisher plays in a
// real deployment: it receives every event the services emit, renders the
// outbound payload with the production payload builders, and POSTs it to the
// isolated receiver over real HTTP. Nothing here is on the production code
// path; it only observes it.
type capturingPublisher struct {
	inner    *inMemoryWebhookStore
	builder  payload.PayloadBuilderFactory
	receiver *isolatedReceiver
	client   *http.Client

	mu       sync.Mutex
	failures []string
}

// inMemoryWebhookStore is the capture buffer, mirroring
// testutil.InMemoryWebhookPublisher's contract without importing test-only
// helpers into the receiver path.
type inMemoryWebhookStore struct {
	mu     sync.Mutex
	events []*types.WebhookEvent
}

func (s *inMemoryWebhookStore) add(e *types.WebhookEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

func (s *inMemoryWebhookStore) Events() []*types.WebhookEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*types.WebhookEvent, len(s.events))
	copy(out, s.events)
	return out
}

func newCapturingPublisher(builderFactory payload.PayloadBuilderFactory, receiver *isolatedReceiver) *capturingPublisher {
	return &capturingPublisher{
		inner:    &inMemoryWebhookStore{},
		builder:  builderFactory,
		receiver: receiver,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

// PublishWebhook implements webhookPublisher.WebhookPublisher. A delivery
// failure never fails the publishing service: production publishes to a bus
// and delivers asynchronously, so the receiver records the failure instead.
func (p *capturingPublisher) PublishWebhook(ctx context.Context, event *types.WebhookEvent) error {
	if event == nil {
		return nil
	}
	p.inner.add(event)

	if p.builder == nil {
		p.recordFailure(event.EventName, "payload builder is not configured")
		return nil
	}
	builder, err := p.builder.GetBuilder(event.EventName)
	if err != nil {
		p.recordFailure(event.EventName, fmt.Sprintf("no payload builder: %v", err))
		return nil
	}
	body, err := builder.BuildPayload(ctx, event.EventName, event.Payload)
	if err != nil {
		p.recordFailure(event.EventName, fmt.Sprintf("build payload: %v", err))
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.receiver.URL(), bytes.NewReader(body))
	if err != nil {
		p.recordFailure(event.EventName, fmt.Sprintf("build delivery request: %v", err))
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Flexprice-Event", string(event.EventName))
	req.Header.Set("X-Flexprice-Event-ID", event.ID)

	resp, err := p.client.Do(req)
	if err != nil {
		p.recordFailure(event.EventName, fmt.Sprintf("deliver: %v", err))
		return nil
	}
	defer resp.Body.Close()

	p.receiver.mu.Lock()
	if idx := len(p.receiver.delivered) - 1; idx >= 0 {
		p.receiver.delivered[idx].EntityID = event.EntityID
	}
	p.receiver.mu.Unlock()
	return nil
}

func (p *capturingPublisher) Close() error { return nil }

func (p *capturingPublisher) recordFailure(eventName types.WebhookEventName, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failures = append(p.failures, string(eventName)+": "+msg)
}

func (p *capturingPublisher) Failures() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.failures))
	copy(out, p.failures)
	return out
}

// Events returns every captured event, in emission order.
func (p *capturingPublisher) Events() []*types.WebhookEvent { return p.inner.Events() }

// countEvents counts captured events by exact name.
func (p *capturingPublisher) countEvents(name types.WebhookEventName) int {
	n := 0
	for _, e := range p.Events() {
		if e.EventName == name {
			n++
		}
	}
	return n
}

// deliveredPayloadsFor returns the delivered bodies for an event name, parsed.
func (p *capturingPublisher) deliveredPayloadsFor(name types.WebhookEventName) []map[string]any {
	var out []map[string]any
	for _, d := range p.receiver.Delivered() {
		if d.EventName != string(name) {
			continue
		}
		var parsed map[string]any
		if err := json.Unmarshal(d.Body, &parsed); err == nil {
			out = append(out, parsed)
		}
	}
	return out
}
