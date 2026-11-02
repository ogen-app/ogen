package llm

import (
	"context"

	"github.com/ogen-app/ogen/src/infra/vendors"
)

// EffortLow is the effort level for short, well-specified calls such as
// scoring, where extended thinking adds output tokens without adding quality.
const EffortLow = "low"

type effortKey struct{}

// WithEffort marks ctx's model request with an output_config.effort level.
func WithEffort(ctx context.Context, level string) context.Context {
	return context.WithValue(ctx, effortKey{}, level)
}

// EffortRequested returns the effort level the model request carrying ctx
// asked for, or "" when it asked for none. The HTTP transport reads it and
// writes output_config.effort next to whatever output_config the genkit plugin
// built (it replaces the whole object for structured output, so the level
// cannot travel in the call config).
func EffortRequested(ctx context.Context) string {
	v, _ := ctx.Value(effortKey{}).(string)
	return v
}

// SupportsEffort reports whether model accepts output_config.effort. Models
// that do not reject the field, so the transport only sends it to these.
func SupportsEffort(model string) bool {
	caps, ok := vendors.CapabilitiesOf(VendorAnthropic, model)
	return ok && caps.Effort
}
