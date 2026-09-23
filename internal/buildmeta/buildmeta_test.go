//go:build unit

package buildmeta

import (
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestScopeIsThisModule pins the instrumentation scope of the library to its
// own module path (FC-7). Running under `go test`, lib-observability is the
// main module, so the resolved name must match go.mod exactly - which also
// catches a major-version bump that forgets to update the constant.
func TestScopeIsThisModule(t *testing.T) {
	t.Parallel()

	bi, ok := debug.ReadBuildInfo()
	require.True(t, ok, "build info must be readable from a test binary")

	name, version := Scope()

	assert.Equal(t, "github.com/LerianStudio/lib-observability/v4", name)
	assert.Equal(t, bi.Main.Path, name, "scope name must track the module path in go.mod")
	assert.Equal(t, "(devel)", version, "a test binary is a source build of this module and carries no stamped version")
}

// TestVersionFrom covers the branch that Scope() can never reach from inside
// this repository: in a consumer binary lib-observability is a dependency, not
// the main module, so the Deps scan and the replace rule from FC-7 are the only
// code that runs. Each case also pins a distinct "(devel)" fallback.
func TestVersionFrom(t *testing.T) {
	t.Parallel()

	const (
		thisModule = "github.com/LerianStudio/lib-observability/v4"
		nextMajor  = "github.com/LerianStudio/lib-observability/v5"
	)

	dep := func(path, version string, replace *debug.Module) *debug.Module {
		return &debug.Module{Path: path, Version: version, Replace: replace}
	}

	consumer := func(deps ...*debug.Module) *debug.BuildInfo {
		return &debug.BuildInfo{
			Main: debug.Module{Path: "github.com/LerianStudio/some-service", Version: "v1.2.3"},
			Deps: deps,
		}
	}

	tests := []struct {
		name        string
		buildInfo   *debug.BuildInfo
		wantVersion string
	}{
		{
			name:        "dependency with a stamped version",
			buildInfo:   consumer(dep(thisModule, "v4.2.0", nil)),
			wantVersion: "v4.2.0",
		},
		{
			name: "replace directive wins over the required version",
			buildInfo: consumer(dep(thisModule, "v4.2.0",
				&debug.Module{Path: thisModule, Version: "v4.3.1"})),
			wantVersion: "v4.3.1",
		},
		{
			name: "directory replace carries no version",
			buildInfo: consumer(dep(thisModule, "v4.2.0",
				&debug.Module{Path: "../lib-observability"})),
			wantVersion: "(devel)",
		},
		{
			name:        "dependency with an empty version",
			buildInfo:   consumer(dep(thisModule, "", nil)),
			wantVersion: "(devel)",
		},
		{
			name:        "another major linked into the same binary is ignored",
			buildInfo:   consumer(dep(nextMajor, "v5.0.0", nil), dep(thisModule, "v4.2.0", nil)),
			wantVersion: "v4.2.0",
		},
		{
			name:        "library absent from the dependency list",
			buildInfo:   consumer(dep("github.com/gofiber/fiber/v3", "v3.0.0", nil)),
			wantVersion: "(devel)",
		},
		{
			name: "library is the main module with a stamped version",
			buildInfo: &debug.BuildInfo{
				Main: debug.Module{Path: thisModule, Version: "v4.2.0"},
			},
			wantVersion: "v4.2.0",
		},
		{
			name: "library is the main module of a source build",
			buildInfo: &debug.BuildInfo{
				Main: debug.Module{Path: thisModule},
			},
			wantVersion: "(devel)",
		},
		{
			name:        "no build info at all",
			buildInfo:   nil,
			wantVersion: "(devel)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.wantVersion, versionFrom(tt.buildInfo))
		})
	}
}
