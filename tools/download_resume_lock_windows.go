package tools

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

const downloadResumeLocksSupported = true

func lockDownloadResume(file *os.File) error {
	// Lock a byte beyond the metadata so the lock never prevents checkpoint
	// I/O or another process from inspecting file identity.
	off := windows.Overlapped{Offset: 1 << 30}
	return windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &off)
}

func downloadResumeFileKey(file *os.File) (string, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return "", err
	}
	if info.NumberOfLinks != 1 {
		return "", errors.New("断点续传文件具有多个硬链接")
	}
	return fmt.Sprintf("%x:%x:%x:%x:%x", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow,
		info.CreationTime.HighDateTime, info.CreationTime.LowDateTime), nil
}
