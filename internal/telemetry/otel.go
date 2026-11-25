package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

type contextKey string

const (
	spanContextKey contextKey = "smartdialer_otel_span"
)

var (
	ErrInvalidTraceparent = errors.New("invalid W3C traceparent header format")
)

// Span represents an active distributed tracing span.
type Span struct {
	mu         sync.RWMutex
	TraceID    string
	SpanID     string
	ParentID   string
	Name       string
	StartTime  time.Time
	EndTime    time.Time
	Attributes map[string]any
}

// NewTraceID generates a 16-byte random hex string (32 characters).
func NewTraceID() string {
	bytes := make([]byte, 16)
	_, _ = rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

// NewSpanID generates an 8-byte random hex string (16 characters).
func NewSpanID() string {
	bytes := make([]byte, 8)
	_, _ = rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

// StartSpan creates a child span or a root span conforming to W3C Trace Context.
func StartSpan(ctx context.Context, name string) (context.Context, *Span) {
	var traceID string
	var parentSpanID string

	if parent, ok := ctx.Value(spanContextKey).(*Span); ok && parent != nil {
		traceID = parent.TraceID
		parentSpanID = parent.SpanID
	} else {
		traceID = NewTraceID()
	}

	span := &Span{
		TraceID:    traceID,
		SpanID:     NewSpanID(),
		ParentID:   parentSpanID,
		Name:       name,
		StartTime:  time.Now().UTC(),
		Attributes: make(map[string]any),
	}

	return context.WithValue(ctx, spanContextKey, span), span
}

// SpanFromContext retrieves the current span from context if present.
func SpanFromContext(ctx context.Context) *Span {
	if span, ok := ctx.Value(spanContextKey).(*Span); ok {
		return span
	}
	return nil
}

// SetAttribute records key-value telemetry on the span.
func (s *Span) SetAttribute(key string, val any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Attributes[key] = val
}

// End marks the span completed.
func (s *Span) End() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.EndTime = time.Now().UTC()
}

// Traceparent formats the span into a standard W3C header value: 00-{trace_id}-{span_id}-01.
func (s *Span) Traceparent() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fmt.Sprintf("00-%s-%s-01", s.TraceID, s.SpanID)
}

// ExtractTraceparent parses an incoming W3C traceparent header into context.
func ExtractTraceparent(ctx context.Context, header string) (context.Context, error) {
	header = strings.TrimSpace(header)
	parts := strings.Split(header, "-")
	if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 {
		return ctx, ErrInvalidTraceparent
	}

	span := &Span{
		TraceID:    parts[1],
		SpanID:     parts[2],
		Name:       "carrier_webhook_inbound",
		StartTime:  time.Now().UTC(),
		Attributes: make(map[string]any),
	}

	return context.WithValue(ctx, spanContextKey, span), nil
}
