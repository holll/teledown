//go:build !linux && !windows

package tools

import "os"

func preallocateFile(*os.File, int64) (bool, error) {
	return false, nil
}
