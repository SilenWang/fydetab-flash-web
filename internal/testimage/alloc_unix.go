//go:build unix

package testimage

import (
	"os"
	"syscall"
)

// allocatedBytes reports how many bytes the filesystem actually allocated for
// st. The bool is false when the platform does not expose that number; callers
// then assume sparse files work.
func allocatedBytes(st os.FileInfo) (int64, bool) {
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int64(sys.Blocks) * 512, true
}
