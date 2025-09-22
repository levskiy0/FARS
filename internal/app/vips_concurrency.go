//go:build cgo

package app

/*
#cgo pkg-config: vips
#include <vips/vips.h>
*/
import "C"

func configureVipsConcurrency(value int) {
	if value <= 0 {
		return
	}
	C.vips_concurrency_set(C.int(value))
}
