package configutil

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	durationPattern = regexp.MustCompile(`(?i)^(?:(\d+)d)?(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?$`)
	byteSizePattern = regexp.MustCompile(`(?i)^\s*(\d+)\s*([kmgtp]?i?b?)?\s*$`)
)

func ParseFlexibleDuration(raw string) (time.Duration, error) {
	matches := durationPattern.FindStringSubmatch(raw)
	if matches == nil {
		return 0, fmt.Errorf("invalid duration %q", raw)
	}
	var total time.Duration
	if matches[1] != "" {
		days, err := strconv.Atoi(matches[1])
		if err != nil {
			return 0, fmt.Errorf("parse duration days %q: %w", matches[1], err)
		}
		total += time.Duration(days) * 24 * time.Hour
	}
	if matches[2] != "" {
		hours, err := time.ParseDuration(matches[2] + "h")
		if err != nil {
			return 0, err
		}
		total += hours
	}
	if matches[3] != "" {
		mins, err := time.ParseDuration(matches[3] + "m")
		if err != nil {
			return 0, err
		}
		total += mins
	}
	if matches[4] != "" {
		secs, err := time.ParseDuration(matches[4] + "s")
		if err != nil {
			return 0, err
		}
		total += secs
	}
	return total, nil
}

func ParseByteSize(raw string) (int64, error) {
	clean := strings.TrimSpace(raw)
	if clean == "" {
		return 0, nil
	}
	matches := byteSizePattern.FindStringSubmatch(clean)
	if matches == nil {
		return 0, fmt.Errorf("invalid size %q", raw)
	}
	value, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse size %q: %w", matches[1], err)
	}
	unit := strings.ToLower(strings.TrimSpace(matches[2]))
	if unit == "" || unit == "b" {
		return value, nil
	}
	multiplier, ok := sizeMultiplier(unit)
	if !ok {
		return 0, fmt.Errorf("unknown size unit %q", matches[2])
	}
	if value < 0 {
		return 0, fmt.Errorf("size must be non-negative, got %d", value)
	}
	if value > 0 && multiplier > 0 && value > maxInt64/multiplier {
		return 0, fmt.Errorf("size %q overflows", raw)
	}
	return value * multiplier, nil
}

const maxInt64 = int64(^uint64(0) >> 1)

func sizeMultiplier(unit string) (int64, bool) {
	switch unit {
	case "k", "kb", "kib":
		return 1 << 10, true
	case "m", "mb", "mib":
		return 1 << 20, true
	case "g", "gb", "gib":
		return 1 << 30, true
	case "t", "tb", "tib":
		return 1 << 40, true
	case "p", "pb", "pib":
		return 1 << 50, true
	default:
		return 0, false
	}
}
