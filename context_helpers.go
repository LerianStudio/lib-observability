package observability

import (
	"context"
	"sync/atomic"
)

// ContextWithHeaderID returns a context with the given correlation/request HeaderID attached.
// This is the ingress middleware entry-point: every inbound HTTP/gRPC request should call
// this once so that all downstream NewTrackingFromContext calls return a stable, client-visible ID.
func ContextWithHeaderID(ctx context.Context, headerID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}

	values := cloneContextValues(ctx)
	values.HeaderID = headerID

	return context.WithValue(ctx, ContextKey, values)
}

type errorCauseKey struct{}

// ContextWithErrorCause installs the slot RecordErrorCause fills, and returns
// the func that reads it. Ingress middleware installs it before running the
// chain so a cause recorded deep inside the request survives to the access line.
func ContextWithErrorCause(ctx context.Context) (context.Context, func() error) {
	if ctx == nil {
		ctx = context.Background()
	}

	slot := &atomic.Pointer[error]{}

	return context.WithValue(ctx, errorCauseKey{}, slot), func() error {
		if cause := slot.Load(); cause != nil {
			return *cause
		}

		return nil
	}
}

// RecordErrorCause records why a request failed when the body the client gets
// hides it (a 5xx). The first cause wins; without a slot it does nothing.
func RecordErrorCause(ctx context.Context, err error) {
	if ctx == nil || err == nil {
		return
	}

	if slot, ok := ctx.Value(errorCauseKey{}).(*atomic.Pointer[error]); ok {
		slot.CompareAndSwap(nil, &err)
	}
}
