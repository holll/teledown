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
	fd := int(file.Fd())
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return "", err
	}
	if st.Nlink != 1 {
		return "", errors.New("断点续传文件具有多个硬链接")
	}
	// Creation time distinguishes reused inode numbers where the filesystem
	// provides it. Older kernels/filesystems still provide the dev/inode pair.
	var sx unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH, unix.STATX_BTIME, &sx); err == nil && sx.Mask&unix.STATX_BTIME != 0 {
		return fmt.Sprintf("%x:%x:%x:%x", st.Dev, st.Ino, sx.Btime.Sec, sx.Btime.Nsec), nil
	}
	return fmt.Sprintf("%x:%x", st.Dev, st.Ino), nil
}
