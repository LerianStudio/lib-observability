//go:build unit

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	observability "github.com/LerianStudio/lib-observability/v4"
	constant "github.com/LerianStudio/lib-observability/v4/constants"
	obslog "github.com/LerianStudio/lib-observability/v4/log"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
)

// recordingLogger captures each emitted record with the fields that were
// bound to the logger at the time it was emitted. captureLogger flattens
// every With/Log call into one slice, which cannot distinguish "the access
// log record carried tenant.id" from "some field was bound at some point" -
// and the negative assertions below need exactly that distinction.
type recordingLogger struct {
	// sink is shared by every logger derived through With, so a record
	// emitted by a grandchild - which is what the middleware actually holds
	// after two With calls - is still visible from the root the test owns.
	sink  *recordSink
	bound []obslog.Field
}

type recordSink struct {
	mu      sync.Mutex
	records []loggedRecord
}

type loggedRecord struct {
	message string
	level   int
	fields  []obslog.Field
}

func newRecordingLogger() *recordingLogger {
	return &recordingLogger{sink: &recordSink{}}
}

func (l *recordingLogger) Log(_ context.Context, level int, msg string, fields ...any) {
	l.sink.mu.Lock()
	defer l.sink.mu.Unlock()

	l.sink.records = append(l.sink.records, loggedRecord{
		message: msg,
		level:   level,
		fields:  append(append([]obslog.Field(nil), l.bound...), obslog.Fields(fields...)...),
	})
}

func (l *recordingLogger) With(fields ...any) obslog.Logger {
	return &recordingLogger{
		sink:  l.sink,
		bound: append(append([]obslog.Field(nil), l.bound...), obslog.Fields(fields...)...),
	}
}

func (l *recordingLogger) WithGroup(_ string) obslog.Logger { return l }

func (l *recordingLogger) Enabled(_ int) bool { return true }

func (l *recordingLogger) Sync(_ context.Context) error { return nil }

// accessLogRecord returns the single record carrying the HTTP access-log
// fields. Identified by http_status_code rather than by position so an
// unrelated record emitted by a handler cannot be mistaken for it.
func (l *recordingLogger) accessLogRecord(t *testing.T) loggedRecord {
	t.Helper()

	l.sink.mu.Lock()
	defer l.sink.mu.Unlock()

	var found []loggedRecord

	for _, rec := range l.sink.records {
		if _, ok := fieldValue(rec.fields, "http_status_code"); ok {
			found = append(found, rec)
		}
	}

	require.Len(t, found, 1, "exactly one access-log record must be emitted per request")

	return found[0]
}

func (l *recordingLogger) recordCount() int {
	l.sink.mu.Lock()
	defer l.sink.mu.Unlock()

	return len(l.sink.records)
}

func fieldValue(fields []obslog.Field, key string) (any, bool) {
	for i := range fields {
		if fields[i].Key == key {
			return fields[i].Value, true
		}
	}

	return nil, false
}

// seedTenantBaggageMidChain mimics midaz's post-auth attestation middleware:
// it seeds tenant.id into the request context AFTER WithHTTPLogging has
// already bound its request logger, then republishes the context through
// c.SetContext. This is the ordering that makes the access-log field
// impossible to resolve at bind time.
func seedTenantBaggageMidChain(t *testing.T, tenantID string) fiber.Handler {
	t.Helper()

	return func(c fiber.Ctx) error {
		member, err := baggage.NewMember(constant.AttrKeyTenantID, tenantID)
		require.NoError(t, err)

		bag, err := baggage.FromContext(c.Context()).SetMember(member)
		require.NoError(t, err)

		c.SetContext(baggage.ContextWithBaggage(c.Context(), bag))

		return c.Next()
	}
}

func TestWithHTTPLoggingAccessLogCarriesTenantSeededAfterLoggerBinding(t *testing.T) {
	t.Parallel()

	logger := newRecordingLogger()

	app := fiber.New()
	app.Use(WithHTTPLogging(WithCustomLogger(logger)))
	app.Use(seedTenantBaggageMidChain(t, "acme"))
	app.Get("/v1/organizations", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/v1/organizations", nil))
	require.NoError(t, err)

	defer func() { require.NoError(t, resp.Body.Close()) }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	record := logger.accessLogRecord(t)

	value, ok := fieldValue(record.fields, constant.AttrKeyTenantID)
	require.True(t, ok,
		"the access-log record must carry tenant.id resolved from the post-c.Next() context")
	assert.Equal(t, "acme", value)
}

func TestWithHTTPLoggingAccessLogPrefersAttrBagTenantOverBaggage(t *testing.T) {
	t.Parallel()

	logger := newRecordingLogger()

	app := fiber.New()
	app.Use(WithHTTPLogging(WithCustomLogger(logger)))
	app.Use(seedTenantBaggageMidChain(t, "from-baggage"))
	app.Get("/v1/ledgers", func(c fiber.Ctx) error {
		c.SetContext(observability.ContextWithSpanAttributes(c.Context(),
			attribute.String(constant.AttrKeyTenantID, "from-attrbag")))

		return c.SendStatus(http.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/v1/ledgers", nil))
	require.NoError(t, err)

	defer func() { require.NoError(t, resp.Body.Close()) }()

	value, ok := fieldValue(logger.accessLogRecord(t).fields, constant.AttrKeyTenantID)
	require.True(t, ok)
	assert.Equal(t, "from-attrbag", value,
		"the access log must agree with the span attribute, which prefers the AttrBag")
}

func TestWithHTTPLoggingAccessLogOmitsTenantWhenAbsent(t *testing.T) {
	t.Parallel()

	logger := newRecordingLogger()

	app := fiber.New()
	app.Use(WithHTTPLogging(WithCustomLogger(logger)))
	app.Get("/v1/ledgers", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/v1/ledgers", nil))
	require.NoError(t, err)

	defer func() { require.NoError(t, resp.Body.Close()) }()

	value, ok := fieldValue(logger.accessLogRecord(t).fields, constant.AttrKeyTenantID)
	assert.False(t, ok,
		"an unauthenticated request must produce no tenant.id field at all, not an empty one")
	assert.Nil(t, value)
}

func TestWithHTTPLoggingAccessLogOmitsTenantWhenSeededValueIsUnusable(t *testing.T) {
	t.Parallel()

	// Values the shared sanitizer rejects: the AttrBag can be written
	// directly by any in-process caller, so the access log must apply the
	// same cardinality and control-byte guards as every other tenant
	// ingestion path rather than trusting what it finds.
	cases := []struct {
		name  string
		value string
	}{
		{name: "empty", value: ""},
		{name: "whitespace only", value: "   "},
		{name: "over the length cap", value: repeatByte('a', constant.MaxTenantIDLen+1)},
	}

	for _, tt := range cases {
		tt := tt

		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			logger := newRecordingLogger()

			app := fiber.New()
			app.Use(WithHTTPLogging(WithCustomLogger(logger)))
			app.Get("/v1/assets", func(c fiber.Ctx) error {
				c.SetContext(observability.ContextWithSpanAttributes(c.Context(),
					attribute.String(constant.AttrKeyTenantID, tt.value)))

				return c.SendStatus(http.StatusOK)
			})

			resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/v1/assets", nil))
			require.NoError(t, err)

			defer func() { require.NoError(t, resp.Body.Close()) }()

			_, ok := fieldValue(logger.accessLogRecord(t).fields, constant.AttrKeyTenantID)
			assert.False(t, ok, "an unusable tenant.id must be dropped, never logged as-is")
		})
	}
}

func TestWithHTTPLoggingExcludedRouteEmitsNoRecordEvenWithTenant(t *testing.T) {
	t.Parallel()

	// An excluded path returns before the logger is ever built, so there is
	// no access-log record for a tenant field to appear on. Asserted rather
	// than assumed: the tenant append sits after c.Next(), and a future
	// refactor that moved the exclusion check below it would start emitting
	// a probe line per scrape.
	logger := newRecordingLogger()

	app := fiber.New()
	app.Use(WithHTTPLogging(WithCustomLogger(logger)))
	app.Use(seedTenantBaggageMidChain(t, "acme"))
	app.Get("/health", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusOK)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/health", nil))
	require.NoError(t, err)

	defer func() { require.NoError(t, resp.Body.Close()) }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Zero(t, logger.recordCount(),
		"a probe path must emit no access log, and therefore no tenant.id field")
}

func repeatByte(b byte, n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}

	return string(out)
}
