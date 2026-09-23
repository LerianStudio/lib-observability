// Package buildmeta resolves the OpenTelemetry instrumentation scope that
// lib-observability stamps on every signal it emits itself.
//
// It is internal on purpose: the scope identifies this library, so a consumer
// has no reason to read or override it. Signals produced by service code go
// through Telemetry.Tracer / Telemetry.Meter, the MetricsFactory and the tracer
// on the request context, all scoped to TelemetryConfig.LibraryName.
package buildmeta

import (
	"runtime/debug"
	"sync"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// modulePath is this library's Go module path. It is matched exactly (never by
// prefix) so that a binary linking two majors of lib-observability reports the
// major that actually produced the signal. Bump it together with go.mod.
const modulePath = "github.com/LerianStudio/lib-observability/v4"

// develVersion is the version reported when the module carries no stamped
// version, e.g. a source build or a local replace directive.
const develVersion = "(devel)"

var (
	scopeOnce    sync.Once
	scopeName    string
	scopeVersion string
)

// Scope returns the instrumentation scope name and version for signals this
// library emits: the module path of lib-observability and its version inside
// the running binary. The version falls back to "(devel)" when the build
// carries none. Resolved once and cached.
func Scope() (name, version string) {
	scopeOnce.Do(resolve)

	return scopeName, scopeVersion
}

// Tracer returns a tracer from tp scoped to this library.
func Tracer(tp trace.TracerProvider) trace.Tracer {
	name, version := Scope()

	return tp.Tracer(name, trace.WithInstrumentationVersion(version))
}

// Meter returns a meter from mp scoped to this library.
func Meter(mp metric.MeterProvider) metric.Meter {
	name, version := Scope()

	return mp.Meter(name, metric.WithInstrumentationVersion(version))
}

func resolve() {
	scopeName, scopeVersion = modulePath, develVersion

	if bi, ok := debug.ReadBuildInfo(); ok {
		scopeVersion = versionFrom(bi)
	}
}

// versionFrom derives the module version from build info. It is split out of
// resolve so that the branch which actually runs in production is reachable
// from a test: under `go test` this library is always the main module, while in
// every consumer binary it is a dependency and only the Deps scan below runs.
func versionFrom(bi *debug.BuildInfo) string {
	if bi == nil {
		return develVersion
	}

	// The library is the main module when running its own tests.
	if bi.Main.Path == modulePath {
		if bi.Main.Version != "" {
			return bi.Main.Version
		}

		return develVersion
	}

	for _, dep := range bi.Deps {
		if dep.Path != modulePath {
			continue
		}

		version := dep.Version
		// A replace directive decides the code that is actually linked, so its
		// version wins. A directory replace carries none, which stays "(devel)".
		if dep.Replace != nil {
			version = dep.Replace.Version
		}

		if version != "" {
			return version
		}

		return develVersion
	}

	return develVersion
}
