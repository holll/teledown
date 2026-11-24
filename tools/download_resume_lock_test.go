package tools

import (
	"os"
	"testing"
)

func TestDownloadResumeKernelLockAndFileKey(t *testing.T) {
	if !downloadResumeLocksSupported {
		t.Skip("resume locking unavailable")
	}
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	first, err := root.OpenFile("record", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := root.OpenFile("record", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	firstKey, err := downloadResumeFileKey(first)
	if err != nil || firstKey == "" {
		t.Fatalf("file key = %q, err = %v", firstKey, err)
	}
	if secondKey, err := downloadResumeFileKey(second); err != nil || secondKey != firstKey {
		t.Fatalf("file key changed across opens: %q %q %v", firstKey, secondKey, err)
	}
	if err := lockDownloadResume(first); err != nil {
		t.Fatal(err)
	}
	if err := lockDownloadResume(second); err == nil {
		t.Fatal("accepted an overlapping lock")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lockDownloadResume(second); err != nil {
		t.Fatalf("closing owner did not release lock: %v", err)
	}
	if _, err := second.WriteAt([]byte("checkpoint"), 0); err != nil {
		t.Fatalf("lock blocked its owner's checkpoint write: %v", err)
	}
	if err := root.Remove("record"); err != nil {
		t.Fatalf("cannot unlink checkpoint name while locked: %v", err)
	}
}
