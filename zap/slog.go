package zap

import (
	"io"
	"log/slog"

	logpkg "github.com/LerianStudio/lib-observability/v3/log"
	"go.uber.org/zap/exp/zapslog"
)

// Slog adapts a log.Logger to a stdlib *slog.Logger, so it can be handed to
// libraries that accept an slog-compatible logger (for example
// lib-service-discovery's libsd.WithLogger) without exposing any
// lib-observability type on the boundary.
//
// When l is zap-backed — the production path, since czap.New returns *Logger —
// the returned *slog.Logger writes through the very same zap core, so its output
// stays unified with the rest of the service's logs. Any other implementation
// (the nil logger, gomock doubles) falls back to a discarding handler rather
// than panicking, mirroring the "unknown logger is silent" posture elsewhere.
func Slog(l logpkg.Logger) *slog.Logger {
	if zl, ok := l.(*Logger); ok {
		return slog.New(zapslog.NewHandler(zl.Raw().Core()))
	}

	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
