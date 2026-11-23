package tools

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/gotd/td/tg"
)

// SenderFilter 按带类型标记的 Telegram ID 筛选实际发送者；nil 表示不筛选。
type SenderFilter struct {
	ids    map[int64]struct{}
	selfID int64
}

// ResolveSenders 支持数字 ID、@用户名和 me，多项以逗号或 | 分隔。
// 解析失败直接报错，不能把无效筛选条件降级成下载全部消息。
func (pm *PeerManager) ResolveSenders(ctx context.Context, refs string) (*SenderFilter, error) {
	if strings.TrimSpace(refs) == "" {
		return nil, nil
	}
	filter := &SenderFilter{ids: make(map[int64]struct{})}
	includeSelf := false
	for _, ref := range strings.FieldsFunc(refs, func(r rune) bool { return r == ',' || r == '|' }) {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		if ref == "me" {
			includeSelf = true
			continue
		}
		id, err := strconv.ParseInt(ref, 10, 64)
		if err == nil {
			if _, _, err := parsePeerID(ref); err != nil {
				return nil, fmt.Errorf("-user 中的发送者 ID %q 无效: %w", ref, err)
			}
			filter.ids[id] = struct{}{}
			continue
		}
		if errors.Is(err, strconv.ErrRange) {
			return nil, fmt.Errorf("-user 中的发送者 ID %q 超出范围: %w", ref, err)
		}
		peer, err := pm.mgr.ResolveDomain(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("解析 -user 发送者 %q 失败: %w", ref, err)
		}
		filter.ids[int64(peer.TDLibPeerID())] = struct{}{}
	}
	if len(filter.ids) == 0 && !includeSelf {
		return nil, fmt.Errorf("-user 未指定有效发送者")
	}
	// 私聊发出的消息可能省略 FromID，需用自身 ID 而非对方（接收者）的 ID。
	self, err := pm.mgr.Self(ctx)
	if err != nil {
		return nil, fmt.Errorf("获取发送者筛选所需的自身 ID 失败: %w", err)
	}
	if self.ID() <= 0 {
		return nil, fmt.Errorf("获取发送者筛选所需的自身 ID 失败: 无有效用户")
	}
	filter.selfID = self.ID()
	if includeSelf {
		filter.ids[filter.selfID] = struct{}{}
	}
	return filter, nil
}

func (f *SenderFilter) Match(msg *tg.Message) bool {
	if f == nil {
		return true
	}
	sender := msg.FromID
	if sender == nil {
		switch peer := msg.PeerID.(type) {
		case *tg.PeerUser:
			if msg.Out {
				sender = &tg.PeerUser{UserID: f.selfID}
			} else {
				sender = peer
			}
		case *tg.PeerChannel:
			if msg.Post {
				sender = peer
			}
		}
	}
	var id int64
	switch peer := sender.(type) {
	case *tg.PeerUser:
		id = peer.UserID
	case *tg.PeerChat:
		id = -peer.ChatID
	case *tg.PeerChannel:
		id = -1000000000000 - peer.ChannelID
	default:
		return false
	}
	_, ok := f.ids[id]
	return ok
}
