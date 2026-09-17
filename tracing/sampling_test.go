//go:build unit

package tracing

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/LerianStudio/lib-observability/v4/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// sdkDefaultSamplerDescription is the sampler the OpenTelemetry SDK installs
// when a TracerProvider is built without WithSampler — the behavior every
// caller had before SampleRatio existed, and the behavior SampleRatio == 0
// must keep.
func sdkDefaultSamplerDescription() string {
	return sdktrace.ParentBased(sdktrace.AlwaysSample()).Description()
}

// countSampled starts n root spans on tp and reports how many carried a sampled
// span context. The sampling decision is taken at Start, so this needs no
// exporter.
func countSampled(t *testing.T, tp *sdktrace.TracerProvider, n int) int {
	t.Helper()

	tracer := tp.Tracer("sampling-test")

	var sampled int

	for range n {
		_, span := tracer.Start(context.Background(), "unit")
		if span.SpanContext().IsSampled() {
			sampled++
		}

		span.End()
	}

	return sampled
}

func TestSampler_ZeroRatioKeepsSDKDefault(t *testing.T) {
	cfg := TelemetryConfig{}

	require.Nil(t, cfg.sampler(),
		"SampleRatio 0 must install no sampler, leaving the SDK default %q", sdkDefaultSamplerDescription())

	// The provider newTracerProvider builds for ratio 0: no WithSampler, so the
	// SDK default applies and every root span is recorded and exported.
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	assert.Equal(t, 200, countSampled(t, tp, 200), "ratio 0 must sample every root span")
	assert.Len(t, sr.Ended(), 200, "ratio 0 must export every span")
}

func TestSampler_RatioOneSamplesEverything(t *testing.T) {
	cfg := TelemetryConfig{SampleRatio: 1}

	s := cfg.sampler()
	require.NotNil(t, s)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(s), sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	assert.Equal(t, 200, countSampled(t, tp, 200), "ratio 1 must sample every root span")
	assert.Len(t, sr.Ended(), 200, "ratio 1 must export every span")
}

func TestSampler_RatioInRangeInstallsParentBasedTraceIDRatio(t *testing.T) {
	for _, ratio := range []float64{0.0001, 0.1, 0.5, 0.99} {
		s := (&TelemetryConfig{SampleRatio: ratio}).sampler()
		require.NotNil(t, s)

		desc := s.Description()
		assert.Contains(t, desc, "TraceIDRatioBased", "ratio %v must install a TraceIDRatioBased sampler", ratio)
		assert.Contains(t, desc, "ParentBased", "ratio %v must wrap the ratio sampler in ParentBased", ratio)
		assert.NotEqual(t, sdkDefaultSamplerDescription(), desc, "ratio %v must not leave the SDK default", ratio)
	}
}

// A sampled parent wins over the ratio: ParentBased means an in-flight trace is
// never truncated halfway through by a downstream service's own ratio.
func TestSampler_SampledParentOverridesRatio(t *testing.T) {
	// A ratio this small samples nothing on its own across 200 root spans.
	const vanishingRatio = 1e-9

	s := (&TelemetryConfig{SampleRatio: vanishingRatio}).sampler()
	require.NotNil(t, s)

	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(s))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	assert.Equal(t, 0, countSampled(t, tp, 200), "a vanishing ratio must sample no root span")

	parentCtx, parent := sdktrace.NewTracerProvider().Tracer("parent").Start(context.Background(), "parent")
	require.True(t, parent.SpanContext().IsSampled(), "parent must be sampled for this test to mean anything")

	_, child := tp.Tracer("child").Start(parentCtx, "child")
	defer child.End()

	assert.True(t, child.SpanContext().IsSampled(), "a sampled parent must override the ratio")
}

// newTracerProvider is the only place the sampler is applied; this pins the
// wiring so a future edit cannot build the sampler and forget to pass it.
func TestNewTracerProvider_AppliesSampleRatio(t *testing.T) {
	all := (&TelemetryConfig{SampleRatio: 1, Redactor: NewDefaultRedactor()}).
		newTracerProvider(nil, &countingSpanExporter{})
	t.Cleanup(func() { _ = all.Shutdown(context.Background()) })
	assert.Equal(t, 100, countSampled(t, all, 100), "SampleRatio 1 must reach the built provider")

	none := (&TelemetryConfig{SampleRatio: 1e-9, Redactor: NewDefaultRedactor()}).
		newTracerProvider(nil, &countingSpanExporter{})
	t.Cleanup(func() { _ = none.Shutdown(context.Background()) })
	assert.Equal(t, 0, countSampled(t, none, 100), "a vanishing SampleRatio must reach the built provider")
}

func TestNewTelemetry_InvalidSampleRatioIsRejectedBeforeAnyProvider(t *testing.T) {
	cases := map[string]float64{
		"negative":      -0.1,
		"minus one":     -1,
		"above one":     1.0000001,
		"far above one": 42,
		"NaN":           math.NaN(),
		"positive inf":  math.Inf(1),
		"negative inf":  math.Inf(-1),
	}

	for name, ratio := range cases {
		t.Run(name, func(t *testing.T) {
			// EnableTelemetry is false: the noop path would otherwise succeed, so
			// an error here proves validation runs before any provider is built.
			tl, err := NewTelemetry(TelemetryConfig{Logger: log.NewNop(), SampleRatio: ratio})

			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrInvalidSampleRatio), "expected ErrInvalidSampleRatio, got %v", err)
			assert.Nil(t, tl, "no telemetry handle may be returned for an invalid ratio")
		})
	}
}

func TestNewTelemetry_ValidSampleRatioAccepted(t *testing.T) {
	for _, ratio := range []float64{0, 0.0001, 0.5, 1} {
		tl, err := NewTelemetry(TelemetryConfig{Logger: log.NewNop(), SampleRatio: ratio})

		require.NoError(t, err, "ratio %v must be accepted", ratio)
		require.NotNil(t, tl)
		t.Cleanup(tl.ShutdownTelemetry)
	}
}

func TestErrInvalidSampleRatio_MessageNamesTheRange(t *testing.T) {
	_, err := NewTelemetry(TelemetryConfig{Logger: log.NewNop(), SampleRatio: 2})

	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "(0, 1]"), "error must name the accepted range, got %q", err)
}
