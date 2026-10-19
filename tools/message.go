package tools

// 本文件对应 Python 版 tools/tool.py 的 get_history_message：--range 解析与消息遍历。

import (
	"context"
	"fmt"
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
	RangeBetween           // 10..200 或 s10s200
)

// MsgRange 描述要处理的消息范围。
type MsgRange struct {
	Mode RangeMode
	Min  int
	Max  int
	IDs  []int
}

// ParseRange 解析 --range 参数，规则与 Python 版 get_history_message 一致：
//
//	">100"      消息 ID 大于 100
//	"<100"      消息 ID 小于 100
//	"100"       单条消息
//	"1,2,3"     多条消息
//	"s10s200"   区间 10~200
//	"10..200"   区间 10~200（自动补 s 前缀）
//	""          全部
func ParseRange(raw string) (MsgRange, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return MsgRange{Mode: RangeAll}, nil
	}

	// 与 Python 版一致：先做 .. 到 s 前缀的归一化
	if strings.Contains(s, "..") && !strings.ContainsAny(s[:1], "><=s") {
		s = "s" + s
	}

	// 多选消息 ID
	if strings.Contains(s, ",") {
		var ids []int
		for _, part := range strings.Split(s, ",") {
			id, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil {
				return MsgRange{}, fmt.Errorf("--range 中的消息 ID %q 不是数字", part)
			}
			ids = append(ids, id)
		}
		if len(ids) == 0 {
			return MsgRange{}, fmt.Errorf("--range 未解析出任何消息 ID")
		}
		return MsgRange{Mode: RangeList, IDs: ids}, nil
	}

	head := s[:1]
	switch head {
	case ">", "<", "=":
		value, err := strconv.Atoi(s[1:])
		if err != nil {
			return MsgRange{}, fmt.Errorf("--range 参数 %q 不是有效格式", raw)
		}
		switch head {
		case ">":
			if value == 0 {
				return MsgRange{Mode: RangeAll}, nil
			}
			return MsgRange{Mode: RangeMin, Min: value}, nil
		case "<":
			return MsgRange{Mode: RangeMax, Max: value}, nil
		default:
			// "=100"，与 Python 版单选写法一致
			return MsgRange{Mode: RangeSingle, IDs: []int{value}}, nil
		}
	case "s":
		// 继续走下面的区间解析
	default:
		// 纯数字：单条消息（比 Python 版更直观，Python 会把它误解析成前导字符 + 剩余数字）
		value, err := strconv.Atoi(s)
		if err != nil {
			return MsgRange{}, fmt.Errorf("--range 参数 %q 不是有效格式", raw)
		}
		return MsgRange{Mode: RangeSingle, IDs: []int{value}}, nil
	}

	rest := s[1:]
	var lowRaw, highRaw string
	if strings.Contains(rest, "..") {
		lowRaw, highRaw, _ = strings.Cut(rest, "..")
	} else {
		var ok bool
		lowRaw, highRaw, ok = strings.Cut(rest, "s")
		if !ok {
			return MsgRange{}, fmt.Errorf("--range 区间 %q 缺少分隔符，应写成 10..200 或 s10s200", raw)
		}
	}
	low, err := strconv.Atoi(strings.TrimSpace(lowRaw))
	if err != nil {
		return MsgRange{}, fmt.Errorf("--range 下限 %q 不是数字", lowRaw)
	}
	high := 0
	if strings.TrimSpace(highRaw) != "" {
		high, err = strconv.Atoi(strings.TrimSpace(highRaw))
		if err != nil {
			return MsgRange{}, fmt.Errorf("--range 上限 %q 不是数字", highRaw)
		}
	}
	return MsgRange{Mode: RangeBetween, Min: low, Max: high}, nil
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

// EachMessage 按范围遍历消息，逐条回调。
// 回调返回错误时立即中断；大小未知的频道不会一次性把消息全部读进内存。
//
// since 为消息时间的下界（秒级时间戳，0 表示不限）：历史遍历按时间递减，
// 越过下界后可以立即收工，不必为「只看最近几个月」再翻完整个频道。
func EachMessage(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, r MsgRange, since int, fn func(*tg.Message) error) error {
	switch r.Mode {
	case RangeSingle, RangeList:
		// 按 ID 拉取的顺序由传入的 ID 决定，不能据此提前结束，日期过滤由回调负责
		return eachMessageByID(ctx, api, peer, r.IDs, fn)
	default:
		return eachMessageByHistory(ctx, api, peer, r, since, fn)
	}
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

	offsetID := 0
	for page := 0; page < maxHistoryPages; page++ {
		req := &tg.MessagesGetHistoryRequest{
			Peer:     peer,
			OffsetID: offsetID,
			MinID:    r.Min,
			MaxID:    r.Max,
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
