package telemetry_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"smartdialer/internal/telemetry"
)

func TestOpenTelemetry_SpanLifecycle(t *testing.T) {
	ctx := context.Background()

	// 1. Root Span
	rootCtx, rootSpan := telemetry.StartSpan(ctx, "pacing_cycle")
	require.NotEmpty(t, rootSpan.TraceID)
	require.NotEmpty(t, rootSpan.SpanID)
	require.Empty(t, rootSpan.ParentID)

	rootSpan.SetAttribute("campaign.id", "camp-123")
	rootSpan.End()

	// 2. Child Span
	childCtx, childSpan := telemetry.StartSpan(rootCtx, "call_reservation")
	require.Equal(t, rootSpan.TraceID, childSpan.TraceID, "Child span must inherit root trace ID")
	require.Equal(t, rootSpan.SpanID, childSpan.ParentID, "Child span must point to parent span ID")

	tp := childSpan.Traceparent()
	require.True(t, strings.HasPrefix(tp, "00-"))
	require.Contains(t, tp, childSpan.TraceID)
	require.Contains(t, tp, childSpan.SpanID)

	// 3. Context retrieval
	retrieved := telemetry.SpanFromContext(childCtx)
	require.NotNil(t, retrieved)
	require.Equal(t, childSpan.SpanID, retrieved.SpanID)
}

func TestOpenTelemetry_W3CTraceparentExtraction(t *testing.T) {
	validHeader := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

	ctx, err := telemetry.ExtractTraceparent(context.Background(), validHeader)
	require.NoError(t, err)

	span := telemetry.SpanFromContext(ctx)
	require.NotNil(t, span)
	require.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", span.TraceID)
	require.Equal(t, "00f067aa0ba902b7", span.SpanID)

	// Invalid headers
	_, errInvalid := telemetry.ExtractTraceparent(context.Background(), "invalid-traceparent")
	require.ErrorIs(t, errInvalid, telemetry.ErrInvalidTraceparent)

	_, errShort := telemetry.ExtractTraceparent(context.Background(), "00-1234-5678-01")
	require.ErrorIs(t, errShort, telemetry.ErrInvalidTraceparent)
}
