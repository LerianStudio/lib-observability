//go:build unit

package httpobs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// A request replayed through NewHandler under ContextWithStartTime is measured
// from the given start: the SERVER span starts there and the duration sample
// covers the time spent before the handler saw the request.
func TestNewHandler_ContextWithStartTimeMeasuresFromTheGivenStart(t *testing.T) {
	mp, reader, tp, sr := newHarness(t)

	const earlier = 2 * time.Second

	start := time.Now().Add(-earlier)

	h := NewHandler(muxOn("/status/{id}"), WithTracerProvider(tp), WithMeterProvider(mp))

	req := httptest.NewRequestWithContext(ContextWithStartTime(context.Background(), start), http.MethodGet, "/status/1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	spans := serverSpans(t, sr)
	require.Len(t, spans, 1)
	assert.True(t, spans[0].StartTime().Equal(start), "span start %v, want %v", spans[0].StartTime(), start)

	rm := &metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), rm))

	var sum float64

	var found bool

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != httpServerRequestDurationMetric {
				continue
			}

			hist, ok := m.Data.(metricdata.Histogram[float64])
			require.True(t, ok, "expected float64 histogram, got %T", m.Data)
			require.Len(t, hist.DataPoints, 1)
			require.Equal(t, uint64(1), hist.DataPoints[0].Count)

			sum, found = hist.DataPoints[0].Sum, true
		}
	}

	require.True(t, found, "expected %s to be emitted", httpServerRequestDurationMetric)
	assert.GreaterOrEqual(t, sum, earlier.Seconds(), "duration must be measured from the given start")
}
