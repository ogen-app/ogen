package assistantkit

import (
	"context"
	"slices"
	"sync"
	"testing"
)

func TestTouches(t *testing.T) {
	var ts Touches
	ctx := WithTouches(t.Context(), &ts)
	Touch(ctx, "idea", "i1")
	Touch(ctx, "idea", "i1")
	Touch(ctx, "post", "p1")
	Touch(ctx, "idea", "") // covers i1
	Touch(ctx, "idea", "i2")
	want := []Touched{{Kind: "post", ID: "p1"}, {Kind: "idea"}}
	if got := ts.List(); !slices.Equal(got, want) {
		t.Fatalf("touched = %v, want %v", got, want)
	}

	Touch(context.Background(), "idea", "i9") // no sink: a no-op
	if got := (&Touches{}).List(); got == nil || len(got) != 0 {
		t.Fatalf("empty list = %#v, want []", got)
	}
}

func TestTouches_Concurrent(t *testing.T) {
	var ts Touches
	ctx := WithTouches(t.Context(), &ts)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() { Touch(ctx, "idea", string(rune('a'+i%5))) })
	}
	wg.Wait()
	if n := len(ts.List()); n != 5 {
		t.Fatalf("touched %d ideas, want 5", n)
	}
}
