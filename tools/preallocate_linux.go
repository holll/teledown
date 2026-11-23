package tools

import (
	"errors"
	"fmt"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

func preallocateFile(file *os.File, size int64) (supported bool, err error) {
	err = unix.Fallocate(int(file.Fd()), unix.FALLOC_FL_KEEP_SIZE, 0, size)
	runtime.KeepAlive(file)
	switch {
	case errors.Is(err, unix.EOPNOTSUPP), errors.Is(err, unix.ENOSYS):
		return false, nil
	case errors.Is(err, unix.ENOSPC), errors.Is(err, unix.EDQUOT):
		return true, fmt.Errorf("磁盘空间或配额不足: %w", err)
	default:
		return true, err
	}
}
