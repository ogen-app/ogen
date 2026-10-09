package httptransport

import (
	"net/http"
	"testing"
)

func TestTuneDefault(t *testing.T) {
	prev := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = prev })

	TuneDefault()
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("default transport = %T, want *http.Transport", http.DefaultTransport)
	}
	if tr.MaxIdleConnsPerHost != maxIdleConnsPerHost || tr.MaxIdleConns != maxIdleConns {
		t.Errorf("idle limits = %d/%d, want %d/%d", tr.MaxIdleConnsPerHost, tr.MaxIdleConns, maxIdleConnsPerHost, maxIdleConns)
	}
	if tr.Proxy == nil || tr.IdleConnTimeout == 0 {
		t.Error("clone lost the stdlib defaults (proxy from env, idle timeout)")
	}
	if prev.(*http.Transport).MaxIdleConnsPerHost == maxIdleConnsPerHost {
		t.Error("the original transport was modified instead of cloned")
	}
}
