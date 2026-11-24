//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package tools

import (
	"errors"
	"os"
)

const downloadResumeLocksSupported = false

func lockDownloadResume(*os.File) error { return errors.ErrUnsupported }

func downloadResumeFileKey(*os.File) (string, error) { return "", errors.ErrUnsupported }
