package human

import "fmt"

// FormatBytes converts a byte count into a human readable string.
func FormatBytes(n int64) string {
	if n == 0 {
		return "0 B"
	}
	sizes := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	value := float64(n)
	idx := 0
	for value >= 1024 && idx < len(sizes)-1 {
		value /= 1024
		idx++
	}
	if idx == 0 {
		return fmt.Sprintf("%d %s", n, sizes[idx])
	}
	return fmt.Sprintf("%.2f %s", value, sizes[idx])
}
