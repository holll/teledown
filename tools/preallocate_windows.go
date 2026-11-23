package tools

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

func preallocateFile(file *os.File, size int64) (supported bool, err error) {
	err = windows.SetFileInformationByHandle(windows.Handle(file.Fd()), windows.FileAllocationInfo,
		(*byte)(unsafe.Pointer(&size)), uint32(unsafe.Sizeof(size)))
	runtime.KeepAlive(file)
	switch {
	case errors.Is(err, windows.ERROR_INVALID_FUNCTION), errors.Is(err, windows.ERROR_NOT_SUPPORTED), errors.Is(err, windows.ERROR_CALL_NOT_IMPLEMENTED):
		return false, nil
	case errors.Is(err, windows.ERROR_DISK_FULL), errors.Is(err, windows.ERROR_HANDLE_DISK_FULL), errors.Is(err, windows.ERROR_DISK_QUOTA_EXCEEDED):
		return true, fmt.Errorf("磁盘空间或配额不足: %w", err)
	default:
		return true, err
	}
}
