package constants

const (
	// HeaderTenantID is the canonical HTTP header carrying the tenant
	// identifier across Lerian services.
	HeaderTenantID = "X-Tenant-Id"

	// MetadataTenantID is the canonical gRPC metadata key carrying the
	// tenant identifier. gRPC metadata keys are lowercase by spec.
	MetadataTenantID = "tenant-id"

	// AttrKeyTenantID is the OpenTelemetry attribute / log field key used
	// everywhere tenant.id is emitted (logs, traces, metrics).
	AttrKeyTenantID = "tenant.id"

	// AttrKeyTenantName is the OpenTelemetry attribute key for the human-readable
	// tenant name. It exists only for display; tenant.id remains the stable
	// aggregation key because names are mutable.
	AttrKeyTenantName = "tenant.name"

	// MaxTenantIDLen caps tenant IDs extracted from request headers to keep
	// telemetry cardinality bounded. Values exceeding the cap are dropped
	// silently.
	MaxTenantIDLen = 128

	// MaxTenantNameLen caps the authenticated tenant display label. Unlike
	// tenant.id, the name arrives as a free-form string and must be bounded.
	MaxTenantNameLen = 64
)
