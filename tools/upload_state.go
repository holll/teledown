package tools

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

type uploadStatus int

const (
	uploadSent uploadStatus = iota
	uploadFailed
	uploadUnknown
	uploadSkipped
	uploadStatusCount
)

var uploadStatusText = [uploadStatusCount]string{"已发送", "失败", "结果未知（请先核对目标会话，不要直接重试）", "未开始"}

type uploadResult struct {
	status uploadStatus
	err    error
}

type uploadBatchError struct {
	summary string
	causes  []error
}

func (e *uploadBatchError) Error() string   { return e.summary }
func (e *uploadBatchError) Unwrap() []error { return e.causes }

func runUploadBatch(ctx context.Context, out io.Writer, files []string, isDir bool, send func(context.Context, string) uploadResult) error {
	var counts [uploadStatusCount]int
	var causes []error
	for _, path := range files {
		result := uploadResult{status: uploadSkipped, err: ctx.Err()}
		if result.err == nil {
			result = send(ctx, path)
		}
		counts[result.status]++
		message := fmt.Sprintf("上传 %q: %s", path, uploadStatusText[result.status])
		if result.err != nil {
			fileErr := fmt.Errorf("%s: %w", message, result.err)
			fmt.Fprintln(out, fileErr)
			if !isDir {
				return fileErr
			}
			// 未开始的文件有相同的取消原因，不为每个文件保留一份错误。
			if result.status != uploadSkipped || counts[uploadSkipped] == 1 {
				causes = append(causes, fileErr)
			}
		} else {
			fmt.Fprintln(out, message)
		}
	}
	if !isDir {
		return nil
	}
	summary := fmt.Sprintf("上传汇总：已发送 %d，失败 %d，结果未知 %d，未开始 %d", counts[uploadSent], counts[uploadFailed], counts[uploadUnknown], counts[uploadSkipped])
	fmt.Fprintln(out, summary)
	if len(causes) != 0 {
		return &uploadBatchError{summary: summary, causes: causes}
	}
	return nil
}

func sameUploadSource(expected, actual fs.FileInfo) bool {
	return actual.Mode().IsRegular() && os.SameFile(expected, actual) &&
		expected.Size() == actual.Size() && expected.ModTime().Equal(actual.ModTime())
}

const (
	uploadQuarantinePrefix = ".teledown-upload-"
	uploadQuarantinedName  = "source"
)

// Quarantine the candidate before verifying it again and unlink only from the
// private directory. This avoids a check-then-unlink on the public filename.
// These are multiple filesystem operations, not an atomic compare-and-delete;
// the quarantine assumes other processes cannot modify its private contents.
func removeUploadedSource(parent *os.Root, name string, expected fs.FileInfo) error {
	current, err := parent.Lstat(name)
	if err != nil {
		return err
	}
	if !sameUploadSource(expected, current) {
		return errors.New("路径已替换或源文件已修改，未删除")
	}
	quarantine := uploadQuarantinePrefix + rand.Text()
	if err := parent.Mkdir(quarantine, 0o700); err != nil {
		return err
	}
	private, err := parent.OpenRoot(quarantine)
	if err != nil {
		return errors.Join(err, parent.Remove(quarantine))
	}
	staged := filepath.Join(quarantine, uploadQuarantinedName)
	if err := parent.Rename(name, staged); err != nil {
		return errors.Join(err, private.Close(), parent.Remove(quarantine))
	}
	finishErr, empty := finishUploadQuarantine(parent, private, name, staged, expected)
	closeErr := private.Close()
	if !empty {
		return errors.Join(finishErr, closeErr)
	}
	return errors.Join(finishErr, closeErr, parent.Remove(quarantine))
}

func finishUploadQuarantine(parent, private *os.Root, name, staged string, expected fs.FileInfo) (err error, empty bool) {
	recovery := filepath.Join(parent.Name(), staged)
	current, err := private.Lstat(uploadQuarantinedName)
	if err != nil {
		return fmt.Errorf("无法验证隔离文件，请检查 %q: %w", recovery, err), false
	}
	changed := !sameUploadSource(expected, current)
	if changed {
		// Link creates the destination only if absent, unlike Rename which may
		// overwrite a newer entry. Unrestorable entries stay in quarantine.
		if !current.Mode().IsRegular() {
			return fmt.Errorf("候选文件已变化，已保留在 %q", recovery), false
		}
		if err := parent.Link(staged, name); err != nil {
			return fmt.Errorf("候选文件已变化，已保留在 %q: %w", recovery, err), false
		}
		restored, err := parent.Lstat(name)
		if err != nil || !sameUploadSource(current, restored) {
			return fmt.Errorf("未能确认恢复原路径，候选文件已保留在 %q", recovery), false
		}
	}
	if err := private.Remove(uploadQuarantinedName); err != nil {
		return fmt.Errorf("无法清理隔离文件，请检查 %q: %w", recovery, err), false
	}
	if changed {
		return errors.New("候选文件已变化，已恢复原路径，未删除"), true
	}
	return nil, true
}
