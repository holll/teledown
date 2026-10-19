package tools

import (
	"testing"

	"github.com/gotd/td/tg"
)

// 现代照片的大尺寸是 PhotoSizeProgressive（字节数记在 Sizes 列表里）。
// 只认 PhotoSize 会得到 size=0，让「同名同大小即已下载」失效、每次重跑都重复下载。
func TestLargestPhotoPrefersProgressive(t *testing.T) {
	photo := &tg.Photo{
		ID: 1,
		Sizes: []tg.PhotoSizeClass{
			&tg.PhotoStrippedSize{Type: "i", Bytes: []byte{1, 2, 3}},
			&tg.PhotoSize{Type: "m", W: 320, H: 240, Size: 20_000},
			&tg.PhotoSizeProgressive{Type: "y", W: 1280, H: 960, Sizes: []int{5_000, 40_000, 120_000}},
		},
	}
	thumbSize, size := largestPhoto(photo)
	if thumbSize != "y" {
		t.Errorf("thumbSize = %q，期望取字节数最大的 y", thumbSize)
	}
	if size != 120_000 {
		t.Errorf("size = %d，期望 progressive 的最后一个值 120000", size)
	}
}

func TestLargestPhotoPlainSizes(t *testing.T) {
	photo := &tg.Photo{Sizes: []tg.PhotoSizeClass{
		&tg.PhotoSize{Type: "s", W: 100, H: 100, Size: 3_000},
		&tg.PhotoSize{Type: "x", W: 800, H: 600, Size: 50_000},
		&tg.PhotoSizeEmpty{Type: ""},
	}}
	if thumbSize, size := largestPhoto(photo); thumbSize != "x" || size != 50_000 {
		t.Errorf("largestPhoto = (%q, %d)，期望 (x, 50000)", thumbSize, size)
	}
}

// 只有预览尺寸时不能给出定位：空 thumb_size 不是合法的图片下载定位，
// 这种情况应当跳过该图片而不是发一个必然失败的请求。
func TestLargestPhotoWithoutDownloadableSize(t *testing.T) {
	photo := &tg.Photo{Sizes: []tg.PhotoSizeClass{
		&tg.PhotoStrippedSize{Type: "i", Bytes: []byte{9}},
		&tg.PhotoPathSize{Type: "j", Bytes: []byte{1}},
	}}
	if thumbSize, _ := largestPhoto(photo); thumbSize != "" {
		t.Errorf("无可下载尺寸时应返回空 thumbSize，得到 %q", thumbSize)
	}
}

// 图片的下载定位必须带真实 thumb_size（与 Telethon 取最大尺寸的 type 一致）。
func TestExtractMediaPhotoLocation(t *testing.T) {
	photo := &tg.Photo{
		ID:         42,
		AccessHash: 7,
		Sizes: []tg.PhotoSizeClass{
			&tg.PhotoSize{Type: "m", W: 320, H: 240, Size: 20_000},
			&tg.PhotoSizeProgressive{Type: "y", W: 800, H: 600, Sizes: []int{10_000, 99_000}},
		},
	}
	info, ok := ExtractMedia(&tg.Message{Media: &tg.MessageMediaPhoto{Photo: photo}})
	if !ok {
		t.Fatal("图片应被识别为可下载媒体")
	}
	loc, ok := info.Location.(*tg.InputPhotoFileLocation)
	if !ok {
		t.Fatalf("定位类型 = %T，期望 *tg.InputPhotoFileLocation", info.Location)
	}
	if loc.ThumbSize != "y" {
		t.Errorf("ThumbSize = %q，期望 y", loc.ThumbSize)
	}
	if loc.FileReference == nil {
		t.Log("提示：测试数据未设置 file_reference，此处不断言")
	}
	// 字节数取 progressive 的最后一个值（与 Telethon 的 max(size.sizes) 一致）
	if info.Size != 99_000 || info.FileID != 42 {
		t.Errorf("Size=%d FileID=%d，期望 99000 / 42", info.Size, info.FileID)
	}
}

// 文档仍用空 thumb_size 下载文件本体，不能被上面的图片逻辑牵连。
func TestExtractMediaDocumentUsesEmptyThumbSize(t *testing.T) {
	doc := &tg.Document{
		ID:       5,
		MimeType: "video/mp4",
		Size:     1234,
		Attributes: []tg.DocumentAttributeClass{
			&tg.DocumentAttributeFilename{FileName: "a.mp4"},
		},
	}
	info, ok := ExtractMedia(&tg.Message{Media: &tg.MessageMediaDocument{Document: doc}})
	if !ok {
		t.Fatal("文档应被识别为可下载媒体")
	}
	loc, ok := info.Location.(*tg.InputDocumentFileLocation)
	if !ok {
		t.Fatalf("定位类型 = %T，期望 *tg.InputDocumentFileLocation", info.Location)
	}
	if loc.ThumbSize != "" {
		t.Errorf("文档本体的 ThumbSize 应为空，得到 %q", loc.ThumbSize)
	}
	if info.Name != "a.mp4" || info.Size != 1234 {
		t.Errorf("Name=%q Size=%d，期望 a.mp4 / 1234", info.Name, info.Size)
	}
}

// 贴纸仍然跳过（与 Python 版一致）。
func TestExtractMediaSkipsSticker(t *testing.T) {
	doc := &tg.Document{ID: 6, MimeType: "application/x-tgsticker",
		Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeSticker{Alt: "x"}}}
	info, ok := ExtractMedia(&tg.Message{Media: &tg.MessageMediaDocument{Document: doc}})
	if !ok {
		t.Fatal("应先识别出文档，跳过与否交给调用方判断")
	}
	if !info.IsSticker {
		t.Error("带 sticker 属性的文档应标记为贴纸")
	}
}
