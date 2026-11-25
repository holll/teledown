package tools

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNonResumeHardlinkPreflightDoesNotTransfer(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	const target = "media.mp4"
	failure := errors.New("hardlinks unsupported")
	var calls int
	err = runNonResumeDownload(t.Context(), root, target, "", 4, func(source, probe string) error {
		calls++
		if source == target || probe == target || filepath.Dir(probe) != filepath.Dir(target) {
			t.Fatalf("probe used unexpected path: %s -> %s", source, probe)
		}
		if got, err := root.Stat(source); err != nil || got.Size() != 0 {
			t.Fatalf("expected an empty temporary file before link probe: %v %v", got, err)
		}
		return failure
	}, func(*os.File) error {
		t.Fatal("file bytes requested after failing publication preflight")
		return nil
	})
	if !errors.Is(err, failure) || !strings.Contains(err.Error(), "未开始传输") || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	entries, err := os.ReadDir(root.Name())
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed probe left files: %v %v", entries, err)
	}
}

func TestDownloadPublicationProof(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	const target = "media.mp4"
	proof := downloadProofPath(target, "document", 12, "", 4)
	if err := runNonResumeDownload(t.Context(), root, target, proof, 4, root.Link, func(f *os.File) error {
		_, err := f.WriteString("data")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	match := func(proof string) bool {
		t.Helper()
		stat, err := root.Lstat(target)
		if err != nil {
			t.Fatal(err)
		}
		same, err := matchesPublishedDownload(t.Context(), root, target, proof, stat, 4)
		if err != nil {
			t.Fatal(err)
		}
		return same
	}
	if !match(proof) {
		t.Fatal("completed download did not match its proof")
	}
	for _, other := range []string{
		downloadProofPath(target, "photo", 12, "", 4),
		downloadProofPath(target, "document", 13, "", 4),
		downloadProofPath(target, "document", 12, "x", 4),
		downloadProofPath("other.mp4", "document", 12, "", 4),
	} {
		if match(other) {
			t.Fatalf("wrong media or filename matched proof %q", other)
		}
	}
	if err := root.WriteFile(target, []byte("edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	if match(proof) {
		t.Fatal("same-size modification incorrectly matched old proof")
	}
	if err := writeDownloadProof(root, proof, sha256.Sum256([]byte("edit"))); !errors.Is(err, os.ErrExist) {
		t.Fatalf("durable proof may have been overwritten: %v", err)
	}
	if match(proof) {
		t.Fatal("original publication proof was replaced")
	}
}

func TestDownloadCancellationBeforePublicationLeavesNoProof(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	const target = "media.mp4"
	proof := downloadProofPath(target, "document", 12, "", 4)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err = runNonResumeDownload(ctx, root, target, proof, 4, root.Link, func(f *os.File) error {
		_, err := f.WriteString("data")
		cancel()
		return err
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want cancellation, got %v", err)
	}
	entries, err := os.ReadDir(root.Name())
	if err != nil || len(entries) != 0 {
		t.Fatalf("canceled download left files: %v %v", entries, err)
	}
}
