package flowkit

import (
	"context"
	"log/slog"

	"github.com/firebase/genkit/go/ai"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/usage"
)

// Usage logs and meters the model responses of one flow run.
type Usage struct {
	Recorder *usage.Recorder // nil disables recording
	Model    modelconfig.Resolved
	// Feature is the usage-event feature name the spend is attributed to.
	Feature string
	// Component is the logging component, e.g. "genkit.post_assistant".
	Component string
	// Attrs are slog key/value pairs identifying the run, logged after the
	// component on every line.
	Attrs []any
}

// Finish warns when resp was truncated at maxTokens, logs its token counts and
// records its usage. A nil resp is ignored.
func (u Usage) Finish(ctx context.Context, resp *ai.ModelResponse, maxTokens int64) {
	if resp == nil {
		return
	}
	u.WarnIfTruncated(ctx, resp, maxTokens)
	u.LogTokens(ctx, "tokens", resp)
	u.Record(ctx, resp)
}

// WarnIfTruncated logs when resp stopped at the output-token cap, so the cap
// can be tuned before the flows' truncation recovery kicks in.
func (u Usage) WarnIfTruncated(ctx context.Context, resp *ai.ModelResponse, maxTokens int64) {
	if resp == nil || resp.FinishReason != ai.FinishReasonLength {
		return
	}
	var outputTokens int64
	if resp.Usage != nil {
		outputTokens = int64(resp.Usage.OutputTokens)
	}
	slog.WarnContext(ctx, "response truncated at max tokens", u.attrs("output_tokens", outputTokens, "cap", maxTokens)...)
}

// LogTokens logs resp's token counts under msg when the provider reported them.
func (u Usage) LogTokens(ctx context.Context, msg string, resp *ai.ModelResponse) {
	if resp == nil || resp.Usage == nil {
		return
	}
	in, out := resp.Usage.InputTokens, resp.Usage.OutputTokens
	slog.InfoContext(ctx, msg, u.attrs("input", in, "output", out, "total", in+out)...)
}

// Record meters resp under the flow's feature. A nil resp is ignored.
func (u Usage) Record(ctx context.Context, resp *ai.ModelResponse) {
	if resp == nil {
		return
	}
	u.Recorder.RecordResp(ctx, u.Model.Vendor, u.Model.Model, u.Feature, resp)
}

func (u Usage) attrs(extra ...any) []any {
	out := make([]any, 0, 2+len(u.Attrs)+len(extra))
	out = append(out, logging.AttrComponent, u.Component)
	out = append(out, u.Attrs...)
	return append(out, extra...)
}
