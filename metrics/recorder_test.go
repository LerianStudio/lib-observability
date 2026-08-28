//go:build unit

package metrics

import (
	"context"
	"math"
	"testing"

	"github.com/LerianStudio/lib-observability/v3/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// ---------------------------------------------------------------------------
// A meter whose instruments record what they were handed, so the flattened
// Recorder calls can be observed end to end.
// ---------------------------------------------------------------------------

type recordingMeter struct {
	metric.Meter

	counter   *recordingCounter
	gauge     *recordingGauge
	histogram *recordingHistogram

	counterNames   []string
	gaugeNames     []string
	histogramNames []string
}

type recordingCounter struct {
	metric.Int64Counter

	values []int64
}

type recordingGauge struct {
	metric.Int64Gauge

	values []int64
}

type recordingHistogram struct {
	metric.Int64Histogram

	values []int64
}

func (c *recordingCounter) Add(_ context.Context, v int64, _ ...metric.AddOption) {
	c.values = append(c.values, v)
}

func (g *recordingGauge) Record(_ context.Context, v int64, _ ...metric.RecordOption) {
	g.values = append(g.values, v)
}

func (h *recordingHistogram) Record(_ context.Context, v int64, _ ...metric.RecordOption) {
	h.values = append(h.values, v)
}

func (m *recordingMeter) Int64Counter(name string, _ ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	m.counterNames = append(m.counterNames, name)

	return m.counter, nil
}

func (m *recordingMeter) Int64Gauge(name string, _ ...metric.Int64GaugeOption) (metric.Int64Gauge, error) {
	m.gaugeNames = append(m.gaugeNames, name)

	return m.gauge, nil
}

func (m *recordingMeter) Int64Histogram(name string, _ ...metric.Int64HistogramOption) (metric.Int64Histogram, error) {
	m.histogramNames = append(m.histogramNames, name)

	return m.histogram, nil
}

func newRecordingFactory(t *testing.T) (*MetricsFactory, *recordingMeter) {
	t.Helper()

	base := noop.NewMeterProvider().Meter("recorder-test")

	counter, err := base.Int64Counter("c")
	require.NoError(t, err)
	gauge, err := base.Int64Gauge("g")
	require.NoError(t, err)
	histogram, err := base.Int64Histogram("h")
	require.NoError(t, err)

	meter := &recordingMeter{
		Meter:     base,
		counter:   &recordingCounter{Int64Counter: counter},
		gauge:     &recordingGauge{Int64Gauge: gauge},
		histogram: &recordingHistogram{Int64Histogram: histogram},
	}

	f, err := NewMetricsFactory(meter, log.NewNop())
	require.NoError(t, err)

	return f, meter
}

// ---------------------------------------------------------------------------
// The universal surface is on the concrete type, not a wrapper.
// ---------------------------------------------------------------------------

// TestMetricsFactory_ImplementsRecorderDirectly is the reason Recorder exists
// in this shape: because the methods live on *MetricsFactory itself, an
// existing caller passing tl.MetricsFactory to a parameter that was widened to
// a universal interface keeps compiling with no adapter call at any call site.
func TestMetricsFactory_ImplementsRecorderDirectly(t *testing.T) {
	t.Parallel()

	var r Recorder = NewNopFactory()
	require.NotNil(t, r)

	_, ok := r.(*MetricsFactory)
	assert.True(t, ok, "Recorder must be satisfied by the concrete factory, not a wrapper type")
}

func TestRecorder_NilFactory(t *testing.T) {
	t.Parallel()

	var f *MetricsFactory

	ctx := context.Background()

	assert.ErrorIs(t, f.AddCounter(ctx, "n", "d", "1", nil, 1), ErrNilFactory)
	assert.ErrorIs(t, f.SetGauge(ctx, "n", "d", "1", nil, 1), ErrNilFactory)
	assert.ErrorIs(t, f.RecordHistogram(ctx, "n", "d", "ms", nil, 1, nil), ErrNilFactory)
}

// TestRecordHistogram_NilFactoryStillValidatesValueFirst documents the order:
// an unrepresentable value is rejected before the receiver is consulted, so
// the value error wins on a nil factory.
func TestRecordHistogram_NilFactoryStillValidatesValueFirst(t *testing.T) {
	t.Parallel()

	var f *MetricsFactory

	assert.ErrorIs(t,
		f.RecordHistogram(context.Background(), "n", "d", "ms", nil, math.NaN(), nil),
		ErrHistogramValueNotRepresentable,
	)
}

func TestRecorder_HappyPath(t *testing.T) {
	t.Parallel()

	f, meter := newRecordingFactory(t)
	ctx := context.Background()

	attrs := map[string]string{"component": "ledger"}

	require.NoError(t, f.AddCounter(ctx, "requests_total", "desc", "1", attrs, 3))
	require.NoError(t, f.SetGauge(ctx, "queue_depth", "desc", "1", attrs, 42))
	require.NoError(t, f.RecordHistogram(ctx, "latency_ms", "desc", "ms", attrs, 12, []float64{1, 10, 100}))

	assert.Equal(t, []int64{3}, meter.counter.values)
	assert.Equal(t, []int64{42}, meter.gauge.values)
	assert.Equal(t, []int64{12}, meter.histogram.values)
}

func TestRecorder_NilAttrsAndBucketsAreAccepted(t *testing.T) {
	t.Parallel()

	f, meter := newRecordingFactory(t)
	ctx := context.Background()

	require.NoError(t, f.AddCounter(ctx, "c", "d", "1", nil, 1))
	require.NoError(t, f.SetGauge(ctx, "g", "d", "1", nil, 1))
	// nil buckets select the package default rather than erroring.
	require.NoError(t, f.RecordHistogram(ctx, "h", "d", "ms", nil, 5, nil))

	assert.Equal(t, []int64{1}, meter.counter.values)
	assert.Equal(t, []int64{1}, meter.gauge.values)
	assert.Equal(t, []int64{5}, meter.histogram.values)
}

// TestAddCounter_RejectsNegativeDelta shows the flattened call inherits the
// builder's validation rather than bypassing it.
func TestAddCounter_RejectsNegativeDelta(t *testing.T) {
	t.Parallel()

	f, meter := newRecordingFactory(t)

	assert.ErrorIs(t,
		f.AddCounter(context.Background(), "c", "d", "1", nil, -1),
		ErrNegativeCounterValue,
	)
	assert.Empty(t, meter.counter.values)
}

// ---------------------------------------------------------------------------
// Instrument reuse by name.
// ---------------------------------------------------------------------------

// TestRecorder_ReusesInstrumentsByName: the factory caches by name (histograms
// by name plus bucket set), so repeated flattened calls must not build a new
// instrument each time.
func TestRecorder_ReusesInstrumentsByName(t *testing.T) {
	t.Parallel()

	f, meter := newRecordingFactory(t)
	ctx := context.Background()

	for range 5 {
		require.NoError(t, f.AddCounter(ctx, "same_counter", "d", "1", nil, 1))
		require.NoError(t, f.SetGauge(ctx, "same_gauge", "d", "1", nil, 2))
		require.NoError(t, f.RecordHistogram(ctx, "same_histogram", "d", "ms", nil, 3, []float64{1, 2}))
	}

	assert.Equal(t, []string{"same_counter"}, meter.counterNames)
	assert.Equal(t, []string{"same_gauge"}, meter.gaugeNames)
	assert.Equal(t, []string{"same_histogram"}, meter.histogramNames)

	assert.Len(t, meter.counter.values, 5)
	assert.Len(t, meter.gauge.values, 5)
	assert.Len(t, meter.histogram.values, 5)
}

// TestRecordHistogram_DistinctBucketsCreateDistinctInstruments is the flip
// side: the histogram cache key is name PLUS buckets, so the same name with a
// different bucket set is a different instrument.
func TestRecordHistogram_DistinctBucketsCreateDistinctInstruments(t *testing.T) {
	t.Parallel()

	f, meter := newRecordingFactory(t)
	ctx := context.Background()

	require.NoError(t, f.RecordHistogram(ctx, "dur", "d", "ms", nil, 1, []float64{1, 2}))
	require.NoError(t, f.RecordHistogram(ctx, "dur", "d", "ms", nil, 1, []float64{1, 2}))
	require.NoError(t, f.RecordHistogram(ctx, "dur", "d", "ms", nil, 1, []float64{10, 20}))

	assert.Equal(t, []string{"dur", "dur"}, meter.histogramNames)
}

// ---------------------------------------------------------------------------
// float64 -> int64 rounding and the representability boundary.
// ---------------------------------------------------------------------------

// TestRecordHistogram_RoundsHalfAwayFromZero: the backing instrument is an
// Int64Histogram, so the float64 is rounded before recording. Sub-integer
// measurements - notably durations in SECONDS - collapse to 0, which is why
// callers are told to record milliseconds.
func TestRecordHistogram_RoundsHalfAwayFromZero(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value float64
		want  int64
	}{
		{name: "exact integer", value: 12, want: 12},
		{name: "rounds down below half", value: 2.49, want: 2},
		{name: "half rounds away from zero", value: 2.5, want: 3},
		{name: "just above half", value: 2.51, want: 3},
		{name: "negative half rounds away from zero", value: -2.5, want: -3},
		{name: "negative below half", value: -2.49, want: -2},
		{name: "sub-integer seconds collapse to zero", value: 0.42, want: 0},
		{name: "negative sub-integer collapses to zero", value: -0.4, want: 0},
		{name: "negative zero", value: math.Copysign(0, -1), want: 0},
		{name: "large but representable", value: 1e18, want: 1000000000000000000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f, meter := newRecordingFactory(t)

			require.NoError(t,
				f.RecordHistogram(context.Background(), "h", "d", "ms", nil, tt.value, []float64{1}),
			)
			require.Len(t, meter.histogram.values, 1)
			assert.Equal(t, tt.want, meter.histogram.values[0])
		})
	}
}

// TestRecordHistogram_Representability pins the int64 boundary.
//
// The bound is expressed against 2^63 rather than math.MaxInt64 because
// float64 cannot represent math.MaxInt64 exactly - it rounds UP to 2^63 - so a
// naive comparison against the converted constant would let a value one ULP
// too large through and overflow the conversion.
func TestRecordHistogram_Representability(t *testing.T) {
	t.Parallel()

	// The largest float64 strictly below 2^63, and the first one at or above it.
	const largestBelow2Pow63 = float64(1<<62) * 2 * (1 - 1.0/(1<<53))

	tests := []struct {
		name       string
		value      float64
		wantRecord bool
	}{
		{name: "NaN", value: math.NaN()},
		{name: "positive infinity", value: math.Inf(1)},
		{name: "negative infinity", value: math.Inf(-1)},
		{name: "exactly 2^63 is out of range", value: math.Pow(2, 63)},
		{name: "above 2^63", value: math.Pow(2, 63) * 2},
		{name: "math.MaxFloat64", value: math.MaxFloat64},
		{name: "-math.MaxFloat64", value: -math.MaxFloat64},
		{name: "below -2^63", value: -math.Pow(2, 63) * 2},
		// float64(math.MaxInt64) IS 2^63, so it must be rejected too.
		{name: "float64 of math.MaxInt64 rounds up to 2^63", value: float64(math.MaxInt64)},

		{name: "largest float64 below 2^63", value: largestBelow2Pow63, wantRecord: true},
		{name: "exactly -2^63 is in range", value: -math.Pow(2, 63), wantRecord: true},
		{name: "float64 of math.MinInt64", value: float64(math.MinInt64), wantRecord: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f, meter := newRecordingFactory(t)

			err := f.RecordHistogram(context.Background(), "h", "d", "ms", nil, tt.value, []float64{1})

			if !tt.wantRecord {
				assert.ErrorIs(t, err, ErrHistogramValueNotRepresentable)
				assert.Empty(t, meter.histogram.values, "no garbage sample may be recorded")

				return
			}

			require.NoError(t, err)
			require.Len(t, meter.histogram.values, 1)
		})
	}
}

func TestInt64FromFloat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value float64
		want  int64
		ok    bool
	}{
		{name: "zero", value: 0, want: 0, ok: true},
		{name: "positive half", value: 0.5, want: 1, ok: true},
		{name: "negative half", value: -0.5, want: -1, ok: true},
		{name: "min int64", value: math.MinInt64, want: math.MinInt64, ok: true},
		{name: "NaN", value: math.NaN()},
		{name: "+Inf", value: math.Inf(1)},
		{name: "-Inf", value: math.Inf(-1)},
		{name: "2^63", value: math.Pow(2, 63)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := int64FromFloat(tt.value)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ---------------------------------------------------------------------------
// NewMetricsFactory takes the LOCAL one-method Logger interface.
// ---------------------------------------------------------------------------

// foreignLogger implements only Log(ctx, int, string, ...any) - nothing named
// by this module - and is accepted by NewMetricsFactory with no adapter.
type foreignLogger struct{ calls int }

func (f *foreignLogger) Log(context.Context, int, string, ...any) { f.calls++ }

func TestNewMetricsFactory_AcceptsForeignLogger(t *testing.T) {
	t.Parallel()

	f, err := NewMetricsFactory(noop.NewMeterProvider().Meter("m"), &foreignLogger{})
	require.NoError(t, err)
	require.NotNil(t, f)

	assert.NoError(t, f.AddCounter(context.Background(), "c", "d", "1", nil, 1))
}

func TestNewMetricsFactory_AcceptsNilLoggerAndRejectsNilMeter(t *testing.T) {
	t.Parallel()

	f, err := NewMetricsFactory(noop.NewMeterProvider().Meter("m"), nil)
	require.NoError(t, err)
	assert.NoError(t, f.SetGauge(context.Background(), "g", "d", "1", nil, 1))

	f, err = NewMetricsFactory(nil, log.NewNop())
	assert.Nil(t, f)
	assert.ErrorIs(t, err, ErrNilMeter)
}
