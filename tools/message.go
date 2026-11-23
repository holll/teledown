package tools

// 本文件对应 Python 版 tools/tool.py 的 get_history_message：--range 解析与消息遍历。

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/telegram/query"
	"github.com/gotd/td/tg"
)

// RangeMode 对应 Python 版 --range 的几种写法。
type RangeMode int

const (
	RangeAll     RangeMode = iota
	RangeMin               // >n
	RangeMax               // <n
	RangeSingle            // n
	RangeList              // 1,2,3
	RangeBetween           // 10..200（含两端）
)

// MsgRange 描述要处理的消息范围。
type MsgRange struct {
	Mode RangeMode
	Min  int
	Max  int
	IDs  []int
}

const maxMessageID = 1<<31 - 1

// ParseRange parses positive message IDs, strict comparisons and closed A..B ranges.
func ParseRange(raw string) (MsgRange, error) {
	s := strings.TrimSpace(raw)
	if s == "" || s == ">0" {
		return MsgRange{}, nil
	}
	parse := func(value string, allowZero bool) (int, error) {
		value = strings.TrimSpace(value)
		for _, r := range value {
			if r < '0' || r > '9' {
				return 0, fmt.Errorf("无效消息 ID %q", value)
			}
		}
		id, err := strconv.ParseUint(value, 10, 31)
		if err != nil || id == 0 && !allowZero {
			return 0, fmt.Errorf("无效消息 ID %q", value)
		}
		return int(id), nil
	}
	if lo, hi, ok := strings.Cut(s, ".."); ok {
		min, err := parse(lo, true)
		if err != nil {
			return MsgRange{}, err
		}
		max, err := parse(hi, false)
		if err != nil {
			return MsgRange{}, err
		}
		if min > max {
			return MsgRange{}, fmt.Errorf("--range 下限不能大于上限")
		}
		return MsgRange{Mode: RangeBetween, Min: min, Max: max}, nil
	}
	if strings.Contains(s, ",") {
		var ids []int
		seen := make(map[int]struct{})
		for part := range strings.SplitSeq(s, ",") {
			id, err := parse(part, false)
			if err != nil {
				return MsgRange{}, err
			}
			if _, exists := seen[id]; !exists {
				ids = append(ids, id)
				seen[id] = struct{}{}
			}
		}
		return MsgRange{Mode: RangeList, IDs: ids}, nil
	}
	switch s[0] {
	case '>':
		id, err := parse(s[1:], true)
		if err != nil {
			return MsgRange{}, err
		}
		if id == 0 {
			return MsgRange{}, nil
		}
		return MsgRange{Mode: RangeMin, Min: id}, nil
	case '<':
		id, err := parse(s[1:], false)
		if err != nil {
			return MsgRange{}, err
		}
		return MsgRange{Mode: RangeMax, Max: id}, nil
	default:
		id, err := parse(strings.TrimPrefix(s, "="), false)
		if err != nil {
			return MsgRange{}, err
		}
		return MsgRange{Mode: RangeSingle, IDs: []int{id}}, nil
	}
}

// DateRange 是消息发送时间的过滤区间，两侧都是本地时区的秒级时间戳，0 表示该侧不限。
type DateRange struct {
	Since int // 下界（含）
	Until int // 上界（含）
}

// Contains 判断消息时间是否落在区间内。
func (d DateRange) Contains(unix int) bool {
	if d.Since > 0 && unix < d.Since {
		return false
	}
	if d.Until > 0 && unix > d.Until {
		return false
	}
	return true
}

// dateLayouts 是 --since / --until 接受的写法，按「带时间」到「只有日期」排列。
var dateLayouts = []string{
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04",
	"2006-01-02T15:04",
	"2006/01/02 15:04:05",
	"2006/01/02 15:04",
	"2006-01-02",
	"2006/01/02",
}

// parseDateArg 解析一个日期参数，返回其时间与是否携带具体时刻。
func parseDateArg(s string) (time.Time, bool, error) {
	for _, layout := range dateLayouts {
		t, err := time.ParseInLocation(layout, s, time.Local)
		if err != nil {
			continue
		}
		// 只有日期的两个布局不含时刻，其余布局都精确到分或秒
		hasTime := strings.ContainsAny(layout, "T ")
		return t, hasTime, nil
	}
	return time.Time{}, false, fmt.Errorf("期望形如 2026-09-01 或 2026-09-01 12:30")
}

// ParseDateRange 解析 --since / --until 两个参数。
//
//	"2026-09-01"          --since 从当天 00:00:00 起；--until 到当天 23:59:59 止
//	"2026-09-01 12:30"    精确到该时刻（也接受 T 分隔或 2026/09/01 写法）
//
// 只给日期时两侧都含当天，符合「2026-09-01 以后」这类口语表达。
func ParseDateRange(sinceRaw, untilRaw string) (DateRange, error) {
	var out DateRange

	if s := strings.TrimSpace(sinceRaw); s != "" {
		t, _, err := parseDateArg(s)
		if err != nil {
			return DateRange{}, fmt.Errorf("--since %q 解析失败：%w", sinceRaw, err)
		}
		out.Since = int(t.Unix())
	}

	if u := strings.TrimSpace(untilRaw); u != "" {
		t, hasTime, err := parseDateArg(u)
		if err != nil {
			return DateRange{}, fmt.Errorf("--until %q 解析失败：%w", untilRaw, err)
		}
		if !hasTime {
			// 只给日期时把上界推到当天最后一秒，否则当天的消息会被整段排除
			t = time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, time.Local)
		}
		out.Until = int(t.Unix())
	}

	if out.Since != 0 && out.Until != 0 && out.Since > out.Until {
		return DateRange{}, fmt.Errorf("--since 不能晚于 --until")
	}
	return out, nil
}

// EachMessage 按消息 ID 升序逐条回调，回调返回错误时立即中断。
//
// since 为消息时间的下界（秒级时间戳，0 表示不限）。历史接口从新到旧返回，
// 越过下界时停止拉取，再将已收集的消息反向处理，以保证跨页升序。
func EachMessage(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, r MsgRange, since int, fn func(*tg.Message) error) error {
	if r.Mode == RangeSingle || r.Mode == RangeList {
		// 按 ID 拉取不能据日期提前结束，日期过滤由回调负责。
		return eachMessageByID(ctx, api, peer, r.IDs, fn)
	}

	var messages []*tg.Message
	if err := eachMessageByHistory(ctx, api, peer, r, since, func(msg *tg.Message) error {
		messages = append(messages, msg)
		return nil
	}); err != nil {
		return err
	}
	for _, msg := range slices.Backward(messages) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(msg); err != nil {
			return err
		}
	}
	return nil
}

// eachMessageByID 按消息 ID 拉取单条或多条。
//
// 必须走 query 包的 GetMessages：它会按 peer 类型分流（频道用 channels.getMessages，
// 其它用 messages.getMessages）。频道消息 ID 只在频道内唯一，直接调
// messages.getMessages 拿到的是无关消息——Telethon 的 _IDsIter 在同一处也是这么分流的。
func eachMessageByID(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, ids []int, fn func(*tg.Message) error) error {
	// messages.getMessages 的 ID 不限定对话，必须同时验证返回消息的 peer。
	// 收藏夹也不能例外：先解析自身 ID，再按普通用户 peer 匹配。
	target := peer
	if _, ok := peer.(*tg.InputPeerSelf); ok {
		users, err := api.UsersGetUsers(ctx, []tg.InputUserClass{&tg.InputUserSelf{}})
		if err != nil {
			return fmt.Errorf("获取自身用户 ID 失败: %w", err)
		}
		if len(users) != 1 {
			return fmt.Errorf("获取自身用户 ID 失败: 返回 %d 个用户", len(users))
		}
		self, ok := users[0].(*tg.User)
		if !ok || self.ID <= 0 {
			return fmt.Errorf("获取自身用户 ID 失败: 无有效用户")
		}
		target = &tg.InputPeerUser{UserID: self.ID}
	}

	raw, err := query.NewQuery(api).Messages().GetMessages(ctx, peer, ids...)
	if err != nil {
		return fmt.Errorf("按 ID 拉取消息失败: %w", err)
	}
	slices.SortFunc(raw, func(a, b tg.MessageClass) int { return cmp.Compare(a.GetID(), b.GetID()) })
	for _, m := range raw {
		msg, ok := m.(*tg.Message)
		if !ok {
			// 已删除或无权限的 ID 返回 MessageEmpty，跳过
			continue
		}
		if !messagePeerMatches(target, msg.PeerID) {
			continue
		}
		if err := fn(msg); err != nil {
			return err
		}
	}
	return nil
}

func messagePeerMatches(target tg.InputPeerClass, actual tg.PeerClass) bool {
	switch peer := target.(type) {
	case *tg.InputPeerUser:
		p, ok := actual.(*tg.PeerUser)
		return ok && p.UserID == peer.UserID
	case *tg.InputPeerChat:
		p, ok := actual.(*tg.PeerChat)
		return ok && p.ChatID == peer.ChatID
	case *tg.InputPeerChannel:
		p, ok := actual.(*tg.PeerChannel)
		return ok && p.ChannelID == peer.ChannelID
	default:
		return false
	}
}

func eachMessageByHistory(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, r MsgRange, since int, fn func(*tg.Message) error) error {
	const pageSize = 100

	if r.Mode == RangeMin && r.Min >= maxMessageID || r.Mode == RangeMax && r.Max <= 1 {
		return nil
	}
	minID, maxID := r.Min, r.Max
	if r.Mode == RangeBetween {
		minID = max(0, r.Min-1)
		if r.Max == maxMessageID {
			maxID = 0
		} else {
			maxID = r.Max + 1
		}
	}
	offsetID := 0
	for page := 0; page < maxHistoryPages; page++ {
		req := &tg.MessagesGetHistoryRequest{
			Peer:     peer,
			OffsetID: offsetID,
			MinID:    minID,
			MaxID:    maxID,
			Limit:    pageSize,
		}
		resp, err := api.MessagesGetHistory(ctx, req)
		if err != nil {
			return fmt.Errorf("拉取历史消息失败: %w", err)
		}

		msgs := messagesOf(resp)
		if len(msgs) == 0 {
			return nil
		}
		nextOffsetID := offsetID
		for _, m := range msgs {
			// 服务消息和已删除消息也占用历史记录，必须参与游标推进。
			if id := m.GetID(); id > 0 && (nextOffsetID == 0 || id < nextOffsetID) {
				nextOffsetID = id
			}
			// MessageEmpty 没有日期；普通消息和服务消息均保留 since 含边界语义。
			if dated, ok := m.AsNotEmpty(); ok && since > 0 && dated.GetDate() < since {
				return nil
			}
			if msg, ok := m.(*tg.Message); ok {
				if msg.ID <= minID || maxID > 0 && msg.ID >= maxID {
					continue
				}
				if err := fn(msg); err != nil {
					return err
				}
			}
		}
		// 分页依据原始 MessageClass 数量，不能用过滤后的普通消息数量。
		if len(msgs) < pageSize {
			return nil
		}
		if nextOffsetID == 0 || nextOffsetID == offsetID {
			return fmt.Errorf("历史消息分页游标未推进: offset_id=%d", offsetID)
		}
		offsetID = nextOffsetID
	}
	return fmt.Errorf("历史消息分页超过上限 %d，遍历未完成", maxHistoryPages)
}

// maxHistoryPages 限制单次执行最多翻多少页，避免异常情况下无限循环。
const maxHistoryPages = 10000

func messagesOf(resp tg.MessagesMessagesClass) []tg.MessageClass {
	switch v := resp.(type) {
	case *tg.MessagesMessages:
		return v.Messages
	case *tg.MessagesMessagesSlice:
		return v.Messages
	case *tg.MessagesChannelMessages:
		return v.Messages
	default:
		return nil
	}
}
