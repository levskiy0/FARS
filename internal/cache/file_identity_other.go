//go:build !linux && !darwin

package cache

import "os"

func platformFileIdentity(os.FileInfo) (int64, uint64) {
	return 0, 0
}
