package human

import "testing"

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		name string
		in   int64
		out  string
	}{
		{"zero", 0, "0 B"},
		{"bytes", 512, "512 B"},
		{"kilobytes", 2048, "2.00 KB"},
		{"megabytes", 5*1024*1024 + 512, "5.00 MB"},
		{"gigabytes", 3*1024*1024*1024 + 100, "3.00 GB"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatBytes(tt.in); got != tt.out {
				t.Fatalf("FormatBytes(%d) = %q, want %q", tt.in, got, tt.out)
			}
		})
	}
}
