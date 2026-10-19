package tools

// 本文件对应 Python 版 tools/tool.py 的媒体部分：get_file_id / get_file_name 及 file_reference 刷新。

import (
	"context"
	"fmt"

	"github.com/gotd/td/telegram/query"
	"github.com/gotd/td/tg"
)

// mediaInfo 描述一条消息里可下载的媒体。
type mediaInfo struct {
	Location  tg.InputFileLocationClass
	Name      string
	MIME      string
	Size      int64
	FileID    int64
	IsSticker bool
}

// ExtractMedia 从消息中取出可下载的媒体，返回 false 表示没有可下载内容。
func ExtractMedia(msg *tg.Message) (*mediaInfo, bool) {
	switch m := msg.Media.(type) {
	case *tg.MessageMediaDocument:
		doc, ok := m.Document.(*tg.Document)
		if !ok {
			return nil, false
		}
		info := &mediaInfo{
			// 空 thumbSize 表示文件本体（与 Telethon 下载文档的默认行为一致）
			Location:  doc.AsInputDocumentFileLocation(""),
			MIME:      doc.MimeType,
			Size:      doc.Size,
			FileID:    doc.ID,
			IsSticker: hasStickerAttribute(doc),
		}
		for _, attr := range doc.Attributes {
			if fn, ok := attr.(*tg.DocumentAttributeFilename); ok {
				info.Name = fn.FileName
			}
		}
		return info, true

	case *tg.MessageMediaPhoto:
		photo, ok := m.Photo.(*tg.Photo)
		if !ok {
			return nil, false
		}
		// 图片必须指定某个实际尺寸的 thumbSize：空串不是合法的图片定位
		// （Telethon 取最大尺寸的 type，gotd 自己的 Elem.File() 取不到尺寸时直接
		// 认为「没有文件」），所以这里取不到就跳过。
		thumbSize, size := largestPhoto(photo)
		if thumbSize == "" {
			return nil, false
		}
		return &mediaInfo{
			Location: photo.AsInputPhotoFileLocation(thumbSize),
			MIME:     "image/jpeg",
			Size:     size,
			FileID:   photo.ID,
		}, true

	case *tg.MessageMediaWebPage:
		// 网页预览不是可下载媒体，与 Python 版一致跳过
		return nil, false
	}
	return nil, false
}

// hasStickerAttribute 判断是否为贴纸（Python 版同样跳过贴纸）。
func hasStickerAttribute(doc *tg.Document) bool {
	for _, attr := range doc.Attributes {
		if _, ok := attr.(*tg.DocumentAttributeSticker); ok {
			return true
		}
	}
	return false
}

// sizedPhoto 是「可下载的图片尺寸」共同的方法集，与 gotd 内部
// （telegram/query/messages/specialize.go）用的判断一致：
// PhotoSize、PhotoCachedSize、PhotoSizeProgressive 三者都带 Type/W/H，
// 而 PhotoSizeEmpty / PhotoStrippedSize / PhotoPathSize 只是预览，不能当作定位。
type sizedPhoto interface {
	GetType() string
	GetW() int
	GetH() int
}

// largestPhoto 取最大的可下载尺寸，返回 thumbSize 与字节数。
//
// 只认 *tg.PhotoSize 会漏掉现代照片普遍使用的 PhotoSizeProgressive（大小记在
// Sizes 列表里），拿到 0 会让「同名同大小即已下载」的判断失效、每次重跑都重复下载。
// 这里按字节数排序（与 Telethon 的 _get_thumb 一致），字节数相同时再比面积。
func largestPhoto(photo *tg.Photo) (thumbSize string, size int64) {
	var maxW, maxH int
	for _, s := range photo.Sizes {
		sz, ok := s.(sizedPhoto)
		if !ok || sz.GetType() == "" {
			continue
		}
		bytes := photoSizeBytes(s)
		if thumbSize != "" {
			// 已有候选：字节数更大者胜出，字节数相同再比面积
			if bytes < size || (bytes == size && sz.GetW()*sz.GetH() <= maxW*maxH) {
				continue
			}
		}
		thumbSize, size = sz.GetType(), bytes
		maxW, maxH = sz.GetW(), sz.GetH()
	}
	return thumbSize, size
}

// photoSizeBytes 取某个尺寸对应的字节数；渐进式 JPEG 的大小在 Sizes 列表末尾。
func photoSizeBytes(s tg.PhotoSizeClass) int64 {
	switch v := s.(type) {
	case *tg.PhotoSize:
		return int64(v.Size)
	case *tg.PhotoCachedSize:
		return int64(len(v.Bytes))
	case *tg.PhotoSizeProgressive:
		if len(v.Sizes) > 0 {
			return int64(v.Sizes[len(v.Sizes)-1])
		}
	}
	return 0
}

// MessageText 取消息文本，用于推导文件名。
func MessageText(msg *tg.Message) string {
	return msg.Message
}

// refetchMessage 重新拉取消息，用于刷新过期的 file_reference。
//
// 走 query 包的 GetMessages 而不是直接调 messages.getMessages：频道消息的 ID 只在
// 该频道内唯一，messages.getMessages 不认频道（传进来会返回无关消息），
// query 会按 peer 类型自动分流到 channels.getMessages。
func refetchMessage(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, msgID int) (*tg.Message, error) {
	raw, err := query.NewQuery(api).Messages().GetMessages(ctx, peer, msgID)
	if err != nil {
		return nil, fmt.Errorf("重新拉取消息 %d 失败: %w", msgID, err)
	}
	for _, m := range raw {
		if msg, ok := m.(*tg.Message); ok && msg.ID == msgID {
			return msg, nil
		}
	}
	return nil, fmt.Errorf("消息 %d 已不存在", msgID)
}
