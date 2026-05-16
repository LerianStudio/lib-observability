package observability

import "context"

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
