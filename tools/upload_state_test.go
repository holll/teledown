package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRunUploadBatchReportsEachState(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	files := []string{"first/data", "second/data", "ambiguous", "not-attempted", "also-not-attempted"}
	failure, ambiguous := errors.New("part rejected"), errors.New("response lost")
	var attempted []string
	var out strings.Builder
	err := runUploadBatch(ctx, &out, files, true, func(_ context.Context, path string) uploadResult {
		attempted = append(attempted, path)
		switch path {
		case files[0]:
			return uploadResult{status: uploadSent}
		case files[1]:
			return uploadResult{status: uploadFailed, err: failure}
		case files[2]:
			cancel()
			return uploadResult{status: uploadUnknown, err: ambiguous}
		default:
			t.Fatalf("started %s after cancellation", path)
			return uploadResult{}
		}
	})
	if !slices.Equal(attempted, files[:3]) || !errors.Is(err, failure) || !errors.Is(err, ambiguous) || !errors.Is(err, context.Canceled) {
		t.Fatalf("attempted %v, error %v", attempted, err)
	}
	for _, want := range []string{
		`"first/data": 已发送`, `"second/data": 失败`, `"ambiguous": 结果未知`,
		`"not-attempted": 未开始`, `"also-not-attempted": 未开始`, "已发送 1，失败 1，结果未知 1，未开始 2",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("batch output missing %q: %s", want, &out)
		}
	}
}

func TestRemoveUploadedSource(t *testing.T) {
	for _, kind := range []string{"unchanged", "replaced", "edited"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			name := "item"
			path := filepath.Join(dir, name)
			writeUploadTestSource(t, path, "original")
			parent, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer parent.Close()
			f, err := parent.Open(name)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			expected, err := f.Stat()
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "replaced":
				if err := parent.Rename(name, "backup"); err != nil {
					t.Fatal(err)
				}
				writeUploadTestSource(t, path, "modified") // Same length, different inode.
			case "edited":
				writeUploadTestSource(t, path, "longer modified contents")
			}
			err = removeUploadedSource(parent, name, expected)
			switch kind {
			case "unchanged":
				entries, readErr := os.ReadDir(dir)
				if err != nil || readErr != nil || len(entries) != 0 {
					t.Fatalf("cleanup: %v; remaining entries %v, read error %v", err, entries, readErr)
				}
			case "replaced":
				checkUploadTestContents(t, path, "modified")
				checkUploadTestContents(t, filepath.Join(dir, "backup"), "original")
			case "edited":
				checkUploadTestContents(t, path, "longer modified contents")
			}
			if kind != "unchanged" && err == nil {
				t.Fatal("expected skipped deletion warning")
			}
		})
	}
}

func TestFinishUploadQuarantineNeverOverwritesReplacement(t *testing.T) {
	for _, occupied := range []bool{false, true} {
		name := "vacant"
		if occupied {
			name = "occupied"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			original := filepath.Join(dir, "original")
			writeUploadTestSource(t, original, "uploaded file")
			expected, err := os.Lstat(original)
			if err != nil {
				t.Fatal(err)
			}
			parent, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer parent.Close()
			quarantine := uploadQuarantinePrefix + "test"
			if err := parent.Mkdir(quarantine, 0o700); err != nil {
				t.Fatal(err)
			}
			private, err := parent.OpenRoot(quarantine)
			if err != nil {
				t.Fatal(err)
			}
			defer private.Close()
			staged := filepath.Join(quarantine, uploadQuarantinedName)
			// Model a swap after the public-path check but before quarantine.
			writeUploadTestSource(t, filepath.Join(dir, staged), "unexpected candidate")
			public := filepath.Join(dir, "public")
			if occupied {
				writeUploadTestSource(t, public, "newer entry")
			}
			err, empty := finishUploadQuarantine(parent, private, "public", staged, expected)
			if err == nil || empty == occupied {
				t.Fatalf("cleanup result %v, empty %v, occupied %v", err, empty, occupied)
			}
			if occupied {
				checkUploadTestContents(t, filepath.Join(dir, staged), "unexpected candidate")
				checkUploadTestContents(t, public, "newer entry")
				if !strings.Contains(err.Error(), filepath.Join(dir, staged)) {
					t.Fatalf("missing recovery path: %v", err)
				}
			} else {
				checkUploadTestContents(t, public, "unexpected candidate")
				if _, err := private.Lstat(uploadQuarantinedName); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("old staged path not cleared: %v", err)
				}
			}
		})
	}
}

func writeUploadTestSource(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func checkUploadTestContents(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("%s: contents %q, err %v; want %q", path, got, err, want)
	}
}
