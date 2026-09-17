package httpapi

import (
	"io"
	"log/slog"
	"runtime"
	"testing"

	"fars/internal/config"
)

// TestNewHandlerSizesResizeSemaphore pins where admission control gets its
// size from. It used to be runtime.vips_concurrency, which sizes libvips'
// thread pool *inside* one operation — production sets it to 1, which capped
// the whole service at one resize at a time.
func TestNewHandlerSizesResizeSemaphore(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	tests := []struct {
		name    string
		runtime config.RuntimeConfig
		want    int
	}{
		{
			name:    "explicit resize_concurrency wins",
			runtime: config.RuntimeConfig{VIPSConcurrency: 1, ResizeConcurrency: 12},
			want:    12,
		},
		{
			name:    "unset falls back to GOMAXPROCS, not vips_concurrency",
			runtime: config.RuntimeConfig{VIPSConcurrency: 1},
			want:    runtime.GOMAXPROCS(0),
		},
		{
			name:    "both unset still gets one slot per CPU",
			runtime: config.RuntimeConfig{},
			want:    runtime.GOMAXPROCS(0),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := NewHandler(&config.Config{Runtime: tc.runtime}, nil, nil, logger)
			if got := cap(handler.resizeSem); got != tc.want {
				t.Fatalf("resize semaphore capacity = %d, want %d", got, tc.want)
			}
		})
	}
}
