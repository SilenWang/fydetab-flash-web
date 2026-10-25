//go:build !unix

package testimage

import "os"

// allocatedBytes is unavailable off unix; callers assume sparse files work.
func allocatedBytes(os.FileInfo) (int64, bool) { return 0, false }
