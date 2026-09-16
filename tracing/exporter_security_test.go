//go:build unit

package tracing

import (
	"context"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/LerianStudio/lib-observability/v4/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// firstBytesOnTheWire listens on loopback and reports the first 3 bytes of the
// first accepted connection. It is how the exporter's actual transport is
// observed: 0x16 opens a TLS record (ClientHello), "PRI" opens the HTTP/2
// cleartext preface.
func firstBytesOnTheWire(t *testing.T) (addr string, first <-chan []byte) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	seen := make(chan []byte, 1)

	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))

		buf := make([]byte, 3)
		if _, readErr := io.ReadFull(conn, buf); readErr != nil {
			return
		}

		seen <- buf
	}()

	return ln.Addr().String(), seen
}

// exportOneSpan builds telemetry against the given endpoint and drives a single
// span export, which is what forces the gRPC client to dial.
func exportOneSpan(t *testing.T, cfg TelemetryConfig) {
	t.Helper()

	cfg.EnableTelemetry = true
	cfg.LibraryName = "exporter-security-test"
	cfg.Logger = log.NewNop()

	tl, err := NewTelemetry(cfg)
	require.NoError(t, err)
	require.NotNil(t, tl)

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		_ = tl.ShutdownTelemetryWithContext(ctx)
	})

	_, span := tl.TracerProvider.Tracer("exporter-security-test").Start(context.Background(), "dial")
	span.End()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	// The collector is a raw listener, so the export never completes; the flush
	// error is expected and the dial it triggers is the point.
	_ = tl.ForceFlush(ctx)
}

func awaitFirstBytes(t *testing.T, first <-chan []byte) []byte {
	t.Helper()

	select {
	case got := <-first:
		return got
	case <-time.After(3 * time.Second):
		t.Fatal("exporter never dialled the collector")

		return nil
	}
}

// A secure exporter must stay secure even when the environment carries a
// scheme-less endpoint, which the SDK would otherwise read as plaintext.
func TestExporter_SecureDialsTLSDespiteSchemelessEnv(t *testing.T) {
	addr, first := firstBytesOnTheWire(t)

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", addr)

	exportOneSpan(t, TelemetryConfig{
		CollectorExporterEndpoint: "https://" + addr,
		InsecureExporter:          false,
	})

	assert.Equal(t, byte(0x16), awaitFirstBytes(t, first)[0],
		"a secure exporter must open a TLS record, not the h2c preface")
}

func TestExporter_InsecureDialsPlaintext(t *testing.T) {
	addr, first := firstBytesOnTheWire(t)

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+addr)

	exportOneSpan(t, TelemetryConfig{
		CollectorExporterEndpoint: "http://" + addr,
	})

	assert.Equal(t, "PRI", string(awaitFirstBytes(t, first)),
		"an insecure exporter must open the h2c preface")
}

func TestNormalizeEndpointEnvVars_SchemeFollowsSecurityMode(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		insecure bool
		want     string
	}{
		{name: "bare value, secure exporter", value: "collector:4317", want: "https://collector:4317"},
		{name: "bare value, insecure exporter", value: "collector:4317", insecure: true, want: "http://collector:4317"},
		{name: "http scheme kept", value: "http://collector:4317", want: "http://collector:4317"},
		{name: "https scheme kept", value: "https://collector:4317", insecure: true, want: "https://collector:4317"},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", testCase.value)
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", testCase.value)
			t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", testCase.value)

			normalizeEndpointEnvVars(log.NewNop(), testCase.insecure)

			for _, key := range []string{
				"OTEL_EXPORTER_OTLP_ENDPOINT",
				"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
				"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
			} {
				assert.Equal(t, testCase.want, os.Getenv(key), key)
			}
		})
	}
}
