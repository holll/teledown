package tools

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestPreallocateFileReservesSpaceWithoutExtendingEOF(t *testing.T) {
	file, err := os.Create(filepath.Join(t.TempDir(), "media.downloading"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	const size = 1 << 20
	supported, err := preallocateFile(file, size)
	if err != nil {
		t.Fatal(err)
	}
	if !supported {
		t.Skip("测试所在的文件系统不支持预分配")
	}

	var info struct {
		allocationSize int64
		endOfFile      int64
		numberOfLinks  uint32
		deletePending  byte
		directory      byte
		_              [2]byte
	}
	if err := windows.GetFileInformationByHandleEx(windows.Handle(file.Fd()), windows.FileStandardInfo,
		(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		t.Fatal(err)
	}
	if info.allocationSize < size || info.endOfFile != 0 {
		t.Fatalf("分配大小=%d，逻辑长度=%d；预期至少分配 %d 字节且不改变逻辑长度", info.allocationSize, info.endOfFile, size)
	}
}
