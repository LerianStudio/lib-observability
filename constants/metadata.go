package constants

const (
	// MetadataTraceparent is the metadata key for W3C traceparent.
	MetadataTraceparent = "traceparent"
	// MetadataTracestate is the metadata key for W3C tracestate.
	MetadataTracestate = "tracestate"
	// MetadataBaggage is the metadata key for W3C baggage. gRPC metadata keys
	// are always lowercased by the transport (HTTP/2 requires lowercase
	// header field names), while propagation.HeaderCarrier's Get/Set
	// canonicalize to HeaderBaggagePascal - see ExtractGRPCContext /
	// InjectGRPCContext for the same remapping already applied to
	// traceparent/tracestate.
	MetadataBaggage = "baggage"
)
