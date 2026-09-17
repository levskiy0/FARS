package configutil

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestParseByteSizeUnits(t *testing.T) {
	cases := []struct {
		input string
		want  int64
	}{
		{"", 0},
		{"0", 0},
		{"0gb", 0},
		{"42", 42},
		{"42b", 42},
		{"42B", 42},
		{"8k", 8 << 10},
		{"8kb", 8 << 10},
		{"8KiB", 8 << 10},
		{"3m", 3 << 20},
		{"300mb", 300 << 20},
		{"5MiB", 5 << 20},
		{"2g", 2 << 30},
		{"50gb", 50 << 30},
		{"1GiB", 1 << 30},
		{"4t", 4 << 40},
		{"4tb", 4 << 40},
		{"4TiB", 4 << 40},
		{"1p", 1 << 50},
		{"1pb", 1 << 50},
		{"1PiB", 1 << 50},
		{"  7  mb  ", 7 << 20},
		{"\t12kb\n", 12 << 10},
		{"6 GB", 6 << 30},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, err := ParseByteSize(tc.input)
			if err != nil {
				t.Fatalf("ParseByteSize(%q): %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("ParseByteSize(%q) = %d, want %d", tc.input, got, tc.want)
			}
		})
	}
}

func TestParseByteSizeMalformed(t *testing.T) {
	cases := []string{
		"12foobar",
		"mb",
		"-5mb",
		"1.5gb",
		"1 2 mb",
		"gb12",
		"12 mb extra",
		"0x10",
		"twelve",
		"12eb",
		"12ib",
		"12i",
	}
	for _, input := range cases {
		t.Run(input, func(t *testing.T) {
			got, err := ParseByteSize(input)
			if err == nil {
				t.Fatalf("ParseByteSize(%q) accepted malformed input as %d", input, got)
			}
			if got != 0 {
				t.Fatalf("ParseByteSize(%q) returned %d alongside an error", input, got)
			}
		})
	}
}

func TestParseByteSizeRejectsOverflow(t *testing.T) {
	huge := strings.Repeat("9", 3) + "0000000000000000gb" // far beyond int64 once scaled
	if _, err := ParseByteSize(huge); err == nil {
		t.Fatalf("accepted an overflowing size %q", huge)
	}
	// A value that fits int64 on its own but not after scaling.
	overflow := "9223372036854775807kb"
	if _, err := ParseByteSize(overflow); err == nil {
		t.Fatalf("accepted an overflowing size %q", overflow)
	}
	if got, err := ParseByteSize("9223372036854775807"); err != nil || got != math.MaxInt64 {
		t.Fatalf("ParseByteSize(MaxInt64) = %d, %v", got, err)
	}
}

func TestParseFlexibleDurationForms(t *testing.T) {
	cases := []struct {
		input string
		want  time.Duration
	}{
		{"0", 0},
		{"30d", 30 * 24 * time.Hour},
		{"24h", 24 * time.Hour},
		{"1d12h", 36 * time.Hour},
		{"45m10s", 45*time.Minute + 10*time.Second},
		{"250ms", 250 * time.Millisecond},
		{"1h30m", 90 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, err := ParseFlexibleDuration(tc.input)
			if err != nil {
				t.Fatalf("ParseFlexibleDuration(%q): %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("ParseFlexibleDuration(%q) = %s, want %s", tc.input, got, tc.want)
			}
		})
	}
}

// An all-optional pattern means the empty string parses to zero; config's own
// decoder short-circuits it before reaching here, so this only pins the shape
// callers may rely on.
func TestParseFlexibleDurationEmptyIsZero(t *testing.T) {
	got, err := ParseFlexibleDuration("")
	if err != nil || got != 0 {
		t.Fatalf("ParseFlexibleDuration(\"\") = %s, %v", got, err)
	}
}

func TestParseFlexibleDurationMalformed(t *testing.T) {
	for _, input := range []string{
		"tomorrow", "10x", "d", "1w", "1 h",
		// Each unit is parsed on its own, so each one can overflow on its own.
		"99999999999999999999d",
		"99999999999999999999h",
		"99999999999999999999m",
		"99999999999999999999s",
		"1d99999999999999999999h",
		"1d1h99999999999999999999m",
		"1d1h1m99999999999999999999s",
	} {
		t.Run(input, func(t *testing.T) {
			if got, err := ParseFlexibleDuration(input); err == nil {
				t.Fatalf("ParseFlexibleDuration(%q) accepted malformed input as %s", input, got)
			}
		})
	}
}
