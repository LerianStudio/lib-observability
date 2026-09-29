package tracing

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// TestSecondsHistogramViewFixesDBClientBuckets pins the regression this View
// exists for: without it the SDK default boundaries (designed for
// milliseconds) are applied to db.client.operation.duration, which otelsql
// emits in SECONDS. The smallest default boundary is 5, so every real query —
// measured in production at 0.14 ms to 3.5 ms — lands in the first bucket and
// histogram_quantile returns a constant instead of a latency.
func TestSecondsHistogramViewFixesDBClientBuckets(t *testing.T) {
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(
		metric.WithReader(reader),
		metric.WithView(secondsHistogramView()),
	)

	// Same instrument name and kind otelsql creates.
	hist, err := mp.Meter("test").Float64Histogram("db.client.operation.duration")
	if err != nil {
		t.Fatalf("create histogram: %v", err)
	}

	// A 2 ms query: typical for the ledger in production.
	hist.Record(context.Background(), 0.002)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}

	bounds, counts := histogramBoundsAndCounts(t, &rm, "db.client.operation.duration")

	if got, want := bounds[0], 0.005; got != want {
		t.Errorf("smallest boundary = %v, want %v (seconds, not the %v-second SDK default)", got, want, 5.0)
	}

	// The 2 ms observation must fall in the FIRST bucket (<= 5 ms). Under the
	// broken default it also lands in bucket 0, but that bucket means "<= 5
	// seconds" — which is why the boundary assertion above is the real check.
	if counts[0] != 1 {
		t.Errorf("counts[0] = %d, want 1 (a 2 ms query belongs in the <=5 ms bucket)", counts[0])
	}

	// Guard against the millisecond-scale defaults creeping back in.
	for _, b := range bounds {
		if b > 10 {
			t.Errorf("boundary %v exceeds 10 s; the millisecond defaults are back", b)
			break
		}
	}
}

func histogramBoundsAndCounts(t *testing.T, rm *metricdata.ResourceMetrics, name string) ([]float64, []uint64) {
	t.Helper()

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}

			h, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("%s is %T, want Histogram[float64]", name, m.Data)
			}

			if len(h.DataPoints) == 0 {
				t.Fatalf("%s has no data points", name)
			}

			return h.DataPoints[0].Bounds, h.DataPoints[0].BucketCounts
		}
	}

	t.Fatalf("metric %q not found", name)

	return nil, nil
}
