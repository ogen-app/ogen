package modelprobe

import (
	"context"
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
)

// TestProbeUnsupportedForServiceSlots: vision and transcribe models run inside
// image-service / audio-service, so there is no in-process probe. They report
// ErrProbeUnsupported before touching the secrets store, so TestSlotModel keeps
// the static verdict instead of failing on a missing key.
func TestProbeUnsupportedForServiceSlots(t *testing.T) {
	r := New(nil)
	cases := [][2]string{
		{modelconfig.FlowVision, modelconfig.SlotClassify},
		{modelconfig.FlowVision, modelconfig.SlotAltText},
		{modelconfig.FlowTranscribe, modelconfig.SlotMain},
	}
	for _, c := range cases {
		if _, _, err := r.Probe(context.Background(), c[0], c[1], "gemini-2.5-flash"); !errors.Is(err, ErrProbeUnsupported) {
			t.Errorf("%s/%s: err = %v, want ErrProbeUnsupported", c[0], c[1], err)
		}
	}

	if _, _, err := r.Probe(context.Background(), modelconfig.FlowContentPlan, modelconfig.SlotMain, "claude-sonnet-4-5-20250929"); err == nil || errors.Is(err, ErrProbeUnsupported) {
		t.Fatalf("chat slot without a store: err = %v, want a real failure", err)
	}
}
