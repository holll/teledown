//go:build darwin || dragonfly || freebsd || netbsd || openbsd || solaris

package tools

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

const downloadResumeLocksSupported = true

func lockDownloadResume(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func downloadResumeFileKey(file *os.File) (string, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return "", err
	}
	if stat.Nlink != 1 {
		return "", errors.New("断点续传文件具有多个硬链接")
	}
	return fmt.Sprintf("%x:%x", stat.Dev, stat.Ino), nil
}
