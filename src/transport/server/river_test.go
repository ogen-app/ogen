package server

import (
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/kernel/config"
)

func TestSoftStopTimeout(t *testing.T) {
	tests := []struct {
		name     string
		soft     time.Duration
		shutdown time.Duration
		want     time.Duration
	}{
		{name: "defaults", soft: 195 * time.Second, shutdown: 240 * time.Second, want: 195 * time.Second},
		{name: "soft past the cancel margin is clamped", soft: 210 * time.Second, shutdown: 240 * time.Second, want: 195 * time.Second},
		{name: "soft longer than shutdown is clamped", soft: 10 * time.Minute, shutdown: 240 * time.Second, want: 195 * time.Second},
		{name: "unset soft uses the ceiling", soft: 0, shutdown: 90 * time.Second, want: 45 * time.Second},
		{name: "shutdown shorter than the margin halves it", soft: 0, shutdown: 30 * time.Second, want: 15 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := softStopTimeout(&config.Config{JobSoftStopTimeout: tt.soft, JobShutdownTimeout: tt.shutdown})
			if got != tt.want {
				t.Errorf("softStopTimeout(soft=%v, shutdown=%v) = %v, want %v", tt.soft, tt.shutdown, got, tt.want)
			}
		})
	}
}
