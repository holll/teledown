package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

func TestUploadPartRetryClient(t *testing.T) {
	for _, req := range []bin.Encoder{
		&tg.UploadSaveFilePartRequest{FileID: 81, FilePart: 1, Bytes: []byte("data")},
		&tg.UploadSaveBigFilePartRequest{FileID: 81, FilePart: 2736, FileTotalParts: 3000, Bytes: []byte("data")},
	} {
		t.Run(fmt.Sprintf("%T", req), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				api := tg.NewClient(peerTestInvoker(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
					calls++
					if in != req {
						t.Fatalf("分片请求被替换: %v", in)
					}
					if calls == 1 {
						return fmt.Errorf("rpcDoRequest: engine forcibly closed: %w", context.Canceled)
					}
					out.(*tg.BoolBox).Bool = &tg.BoolTrue{}
					return nil
				}))
				c := uploadPartRetryClient{Client: api}
				var ok bool
				var err error
				switch part := req.(type) {
				case *tg.UploadSaveFilePartRequest:
					ok, err = c.UploadSaveFilePart(context.Background(), part)
				case *tg.UploadSaveBigFilePartRequest:
					ok, err = c.UploadSaveBigFilePart(context.Background(), part)
				}
				if !ok || err != nil || calls != 2 {
					t.Fatalf("分片结果 = %t, %v；共调用 %d 次", ok, err, calls)
				}
			})
		})
	}
}

func TestRetryUploadPartStops(t *testing.T) {
	closed := fmt.Errorf("engine forcibly closed: %w", context.Canceled)
	rpcErr := errors.New("FILE_PART_INVALID")
	for _, tc := range []struct {
		name       string
		sendErr    error
		wantErr    error
		wantCalls  int
		cancelWait bool
	}{
		{"耗尽重试", closed, closed, 3, false},
		{"非连接错误", rpcErr, rpcErr, 1, false},
		{"普通取消错误", context.Canceled, context.Canceled, 1, false},
		{"等待期间取消", closed, context.Canceled, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if tc.cancelWait {
					go func() {
						time.Sleep(500 * time.Millisecond)
						cancel()
					}()
				}
				calls := 0
				ok, err := retryUploadPart(ctx, func() (bool, error) {
					calls++
					return false, tc.sendErr
				})
				if ok || err != tc.wantErr || calls != tc.wantCalls {
					t.Fatalf("分片结果 = %t, %v；共调用 %d 次，预期错误 %v、调用 %d 次", ok, err, calls, tc.wantErr, tc.wantCalls)
				}
			})
		})
	}
}

func TestRetryUploadPartBoundsFalseAcknowledgements(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var attempts []time.Duration
		ok, err := retryUploadPart(t.Context(), func() (bool, error) {
			attempts = append(attempts, time.Since(start))
			return false, nil
		})
		if ok || err == nil || !slices.Equal(attempts, []time.Duration{0, time.Second, 3 * time.Second}) {
			t.Fatalf("分片结果 = %t, %v；尝试时刻 %v", ok, err, attempts)
		}
	})
}

func TestUploadPartSize(t *testing.T) {
	const normal, premium = int64(2_097_152_000), int64(4_194_304_000)
	const kib = 1024
	for _, tc := range []struct {
		name    string
		size    int64
		max     int
		premium bool
		want    int
	}{
		{"普通账号低于上限", normal - 1, 4000, false, 512 * kib},
		{"普通账号等于上限", normal, 4000, false, 512 * kib},
		{"普通账号超出一字节", normal + 1, 4000, false, 0},
		{"Premium 高于普通账号上限", normal + 1, 8000, true, 512 * kib},
		{"Premium 等于上限", premium, 8000, true, 512 * kib},
		{"Premium 超出一字节", premium + 1, 8000, true, 0},
		{"服务器上限低于默认值", 512*kib + 1, 1, false, 0},
		{"服务器上限决定分片大小", 128*kib + 1, 1, false, 256 * kib},
		{"小文件保留 SDK 的默认分片大小", 10 << 20, 4000, false, 128 * kib},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := uploadPartSize(tc.size, tc.max, tc.premium)
			if got != tc.want || (err != nil) != (tc.want == 0) {
				t.Fatalf("分片大小 = %v, %v，预期 %d", got, err, tc.want)
			}
		})
	}
}

func TestUploadPartsLimit(t *testing.T) {
	obj := &tg.HelpAppConfig{Config: &tg.JSONObject{Value: []tg.JSONObjectValue{
		{Key: "upload_max_fileparts_default", Value: &tg.JSONNumber{Value: 3000}},
		{Key: "upload_max_fileparts_premium", Value: &tg.JSONNumber{Value: 9000}},
	}}}
	bad := &tg.HelpAppConfig{Config: &tg.JSONObject{Value: []tg.JSONObjectValue{
		{Key: "upload_max_fileparts_default", Value: &tg.JSONNumber{Value: 4.5}},
		{Key: "upload_max_fileparts_premium", Value: &tg.JSONNumber{Value: 1 << 31}},
	}}}
	for _, tc := range []struct {
		name    string
		res     tg.HelpAppConfigClass
		err     error
		premium bool
		want    int
	}{
		{"普通账号服务端配置", obj, nil, false, 3000},
		{"Premium 服务端配置", obj, nil, true, 9000},
		{"忽略非整数", bad, nil, false, 4000},
		{"忽略越界的分片数量", bad, nil, true, 8000},
		{"未返回配置", &tg.HelpAppConfigNotModified{}, nil, false, 4000},
		{"机器人或网络错误", nil, errors.New("unavailable"), false, 4000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			api := tg.NewClient(peerTestInvoker(func(ctx context.Context, in bin.Encoder, out bin.Decoder) error {
				calls++
				if req, ok := in.(*tg.HelpGetAppConfigRequest); !ok || req.Hash != 0 {
					t.Fatalf("预期请求完整配置，得到 %v", in)
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("配置 RPC 缺少超时")
				}
				out.(*tg.HelpAppConfigBox).AppConfig = tc.res
				return tc.err
			}))
			if got := uploadPartsLimit(t.Context(), api, tc.premium); got != tc.want || calls != 1 {
				t.Fatalf("分片上限 = %d，调用 %d 次；期望 %d, 1", got, calls, tc.want)
			}
		})
	}
}

func TestUploadRejectsOversizeBeforeSendingParts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.mp4")
	if err := os.WriteFile(path, make([]byte, 512*1024+1), 0o600); err != nil {
		t.Fatal(err)
	}
	api := tg.NewClient(peerTestInvoker(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
		if _, ok := in.(*tg.HelpGetAppConfigRequest); !ok {
			t.Fatalf("超限文件不应发送 RPC %T", in)
		}
		out.(*tg.HelpAppConfigBox).AppConfig = &tg.HelpAppConfig{Config: &tg.JSONObject{Value: []tg.JSONObjectValue{
			{Key: "upload_max_fileparts_premium", Value: &tg.JSONNumber{Value: 1}},
		}}}
		return nil
	}))
	opt := UploadOptions{Path: path, Premium: true, DeleteAfter: true}
	err := Upload(t.Context(), api, &Peer{Input: &tg.InputPeerSelf{}}, opt)
	if err == nil || !strings.Contains(err.Error(), "524289 字节超过单文件上限 524288 字节") {
		t.Fatalf("预检错误 = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("超限文件不应被删除：%v", err)
	}
}

func TestUploadDoesNotReplayOnSendFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tiny.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	sendFailure := errors.New("发送结果不明确")
	var configs, parts, sends int
	api := tg.NewClient(peerTestInvoker(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
		switch req := in.(type) {
		case *tg.HelpGetAppConfigRequest:
			configs++
			return errors.New("unavailable")
		case *tg.UploadSaveFilePartRequest:
			parts++
			if req.FilePart != 0 || string(req.Bytes) != "hello" {
				t.Fatalf("分片 = %v", req)
			}
			out.(*tg.BoolBox).Bool = &tg.BoolTrue{}
			return nil
		case *tg.MessagesSendMediaRequest:
			sends++
			if parts != 1 {
				t.Fatalf("发送消息时已传分片数 = %d，期望 1", parts)
			}
			return sendFailure
		default:
			t.Fatalf("意外 RPC %T", in)
			return nil
		}
	}))
	opt := UploadOptions{Path: path, Threads: 1, DeleteAfter: true}
	err := Upload(t.Context(), api, &Peer{Input: &tg.InputPeerSelf{}}, opt)
	if !errors.Is(err, sendFailure) || !strings.Contains(err.Error(), "文件分片已上传") ||
		configs != 1 || parts != 1 || sends != 1 {
		t.Fatalf("上传错误 = %v，配置/分片/发送次数 = %d/%d/%d", err, configs, parts, sends)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("发送失败应保留源文件：%v", err)
	}
}

func TestVideoAttributes(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        float64
	}{
		{"fractional", `{"streams":[{"codec_type":"video","width":32,"height":24,"duration":"0.5"}]}`, 0.5},
		{"fallback on N/A", `{"streams":[{"codec_type":"video","width":32,"height":24,"duration":"N/A"}],"format":{"duration":"3.5"}}`, 3.5},
		{"nonfinite", `{"streams":[{"codec_type":"video","width":32,"height":24,"duration":"NaN"}],"format":{"duration":"Inf"}}`, 0},
		{"attached cover only", `{"streams":[{"codec_type":"video","width":32,"height":24,"disposition":{"attached_pic":1}}],"format":{"duration":"5"}}`, 0},
		{"cover before video", `{"streams":[{"codec_type":"video","width":200,"height":200,"disposition":{"attached_pic":1}},{"codec_type":"video","width":32,"height":24,"duration":"2"}]}`, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var result ffprobeResult
			if err := json.Unmarshal([]byte(tc.input), &result); err != nil {
				t.Fatal(err)
			}
			got := videoAttributes(result)
			if tc.want == 0 {
				if got != nil {
					t.Fatalf("expected invalid video, got %v", got)
				}
				return
			}
			if got == nil || got.Duration != tc.want || got.W != 32 || got.H != 24 {
				t.Fatalf("expected %gs, 32x24; got %v", tc.want, got)
			}
		})
	}
}

func TestDetectMIME(t *testing.T) {
	for _, tc := range []struct {
		name, header, want string
	}{
		{"image", "\xff\xd8\xff", "image/jpeg"},
		{"image.MP4", "\xff\xd8\xff", "image/jpeg"},
		{"video", "\x00\x00\x00\x14ftypmp42\x00\x00\x00\x00mp42", "video/mp4"},
		{"packed.mp4", "PK\x03\x04", "application/zip"},
		{"document.docx", "PK\x03\x04", "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
		{"table.CSV", "a,b\n1,2\n", "text/csv"},
		{"subs.SRT", "1\n00:00:00,000 --> 00:00:01,000\n字幕\n", "application/x-subrip"},
		{"notes", "just text", "text/plain"},
		{"unknown", "\x00\x01\x02", "application/octet-stream"},
		{"unknown.mp4", "\x00\x01\x02", "video/mp4"},
		{"empty.png", "", "image/png"},
		{"video.mkv", "\x1a\x45\xdf\xa3", "video/x-matroska"},
		{"video.webm", "\x1a\x45\xdf\xa3", "video/webm"},
		{"ebml", "\x1a\x45\xdf\xa3", "application/octet-stream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tc.name)
			if err := os.WriteFile(path, []byte(tc.header), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := detectMIME(path); got != tc.want {
				t.Fatalf("detectMIME(%q) = %q，期望 %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestCollectFilesSkipsSymlinks(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "regular.txt")
	if err := os.WriteFile(file, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "symlink.txt")
	if err := os.Symlink(file, link); err != nil {
		t.Skipf("cannot create symlinks: %v", err)
	}
	got, isDir, err := collectFiles(dir)
	if err != nil || !isDir || !slices.Equal(got, []string{file}) {
		t.Fatalf("collectFiles directory = %v, %v, %v", got, isDir, err)
	}
	if _, _, err := collectFiles(link); err == nil {
		t.Fatal("explicit symlink must not be treated as uploadable")
	}
}
