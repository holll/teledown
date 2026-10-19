package tools

// 本文件对应 Python 版 tools/tool.py 的会话解析部分：get_chat_id / get_chat_title / parse_user_ids。
//
// 解析交给 gotd 的 peers.Manager：它负责查 peers.Storage → 必要时发 RPC → 回填存储。
// access_hash 的持久化见 sessionstore.go，冷启动时扫会话列表的引导逻辑见 bootstrapPeers。

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/gotd/td/constant"
	"github.com/gotd/td/telegram/peers"
	"github.com/gotd/td/tg"
)

// Peer 是解析后的目标会话。
type Peer struct {
	Input tg.InputPeerClass
	Title string
	ID    int64
}

var numericIDRe = regexp.MustCompile(`^-?[1-9][0-9]{4,}$`)

// errPeerMissing 是内部哨兵：表示本地缓存与快路径都没解析出来，需要引导扫描。
var errPeerMissing = errors.New("peer 不在本地缓存中")

// PeerManager 组合 gotd 的 peers.Manager 与本地持久化存储。
type PeerManager struct {
	mgr     *peers.Manager
	storage *SessionStore
}

// NewPeerManager 构建解析器，peers.Storage 用会话库的 entities 表（见 sessionstore.go）。
func NewPeerManager(api *tg.Client, storage *SessionStore) *PeerManager {
	return &PeerManager{
		// 不设置 Cache：标题等展示信息由 channels.getChannels 的返回值提供，
		// Storage 只负责 access_hash（与 gotd 的设计一致）。
		mgr:     peers.Options{Storage: storage}.Build(api),
		storage: storage,
	}
}

// Resolve 解析 -id 参数，支持：
//   - me：收藏夹（Saved Messages）
//   - @username / username：用户名或 t.me 链接
//   - 数字 ID：-100XXXXXXXXXX（频道/超级群）、-XXXXXXXXXX（基础群）、正数（频道或用户）
func (pm *PeerManager) Resolve(ctx context.Context, arg string) (*Peer, error) {
	if arg == "" {
		return nil, fmt.Errorf("-id 不能为空")
	}
	if arg == "me" {
		return &Peer{Input: &tg.InputPeerSelf{}, Title: "Saved Messages"}, nil
	}

	// 数字必须自己拦：peers.Manager.Resolve 会把纯数字当手机号处理
	if numericIDRe.MatchString(arg) {
		id, kind, err := parsePeerID(arg)
		if err != nil {
			return nil, err
		}
		return pm.resolveByID(ctx, id, kind)
	}

	// 非数字交给 Manager：它自己处理 @用户名、t.me 链接、手机号
	p, err := pm.mgr.Resolve(ctx, arg)
	if err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", arg, err)
	}
	return peerFromManager(p), nil
}

// ForgetPeer 清除某个 ID 的本地缓存，access_hash 失效时调用。
func (pm *PeerManager) ForgetPeer(id int64) {
	if pm == nil {
		return
	}
	pm.storage.ForgetID(id)
}

// resolveByID 按类型提示依次尝试，最后才扫会话列表。
//
// kind 来自 ID 自身的形式（见 parsePeerID）：
//   - kindChannel（-100 开头）：只当频道解析，不必白试群和用户；
//   - kindChat（其它负数）：只当基础群解析，且 messages.getChats 不需要 access_hash，
//     一次廉价 RPC 即可，不用为它扫会话列表；
//   - kindUnknown（不带前缀的正数）：既可能是频道（本工具的主用例）也可能是用户，
//     按频道、群、用户的顺序逐个试。
//
// 只有以上都没命中，才扫会话列表把实体灌进存储后重试——getDialogs 是限流最严的接口，
// 应当尽量避免。
func (pm *PeerManager) resolveByID(ctx context.Context, id int64, kind peerKind) (*Peer, error) {
	switch kind {
	case kindChat:
		if p, err := pm.tryChat(ctx, id); err == nil {
			return p, nil
		}
	default:
		if p, err := pm.tryChannel(ctx, id); err == nil {
			return p, nil
		} else if !errors.Is(err, errPeerMissing) {
			return nil, err
		}
		if kind == kindUnknown {
			// 基础群不需要 access_hash，补一次廉价尝试
			if p, err := pm.tryChat(ctx, id); err == nil {
				return p, nil
			}
		}
	}

	// 冷启动：扫一次会话列表，把实体灌进 peers.Storage 后重试
	if err := pm.bootstrapPeers(ctx, id); err != nil {
		return nil, err
	}
	switch kind {
	case kindChat:
		if p, err := pm.tryChat(ctx, id); err == nil {
			return p, nil
		}
	default:
		if p, err := pm.tryChannel(ctx, id); err == nil {
			return p, nil
		} else if !errors.Is(err, errPeerMissing) {
			return nil, err
		}
	}

	if kind == kindUnknown {
		if u, err := pm.mgr.ResolveUserID(ctx, id); err == nil {
			return peerFromManager(u), nil
		}
	}
	return nil, fmt.Errorf("找不到 ID 为 %d 的会话（已按频道、群、用户依次尝试），请确认账号已加入该会话", id)
}

// tryChannel 用 Manager 解析频道；「找不到」统一归一成 errPeerMissing，
// 顺带清掉可能已过期的本地条目，让后续引导扫描能重新写入。
func (pm *PeerManager) tryChannel(ctx context.Context, id int64) (*Peer, error) {
	ch, err := pm.mgr.ResolveChannelID(ctx, id)
	if err == nil {
		return peerFromManager(ch), nil
	}
	if isPeerMissing(err) {
		pm.storage.ForgetChannel(id)
		return nil, errPeerMissing
	}
	return nil, err
}

// tryChat 解析基础群。任何错误都按「不是一个群」处理：
// messages.getChats 对非群 ID 只返回空列表，gotd 会包成普通错误（不是 tgerr），
// 无法按错误码区分，只能继续往下试其它类型。
func (pm *PeerManager) tryChat(ctx context.Context, id int64) (*Peer, error) {
	chat, err := pm.mgr.ResolveChatID(ctx, id)
	if err != nil {
		return nil, errPeerMissing
	}
	return peerFromManager(chat), nil
}

// isPeerMissing 判断错误是否属于「本地没有 / 凭据失效」，而不是网络或权限问题。
func isPeerMissing(err error) bool {
	var notFound *peers.PeerNotFoundError
	return errors.As(err, &notFound) ||
		tg.IsChannelInvalid(err) ||
		tg.IsChannelPrivate(err)
}

// peerFromManager 把 peers.Peer 转成本工具使用的 Peer。
func peerFromManager(p peers.Peer) *Peer {
	title := p.VisibleName()
	if u, ok := p.(peers.User); ok {
		// 与 Python 版 get_chat_title 的用户标题格式一致：username(显示名)
		if username, has := u.Username(); has {
			title = fmt.Sprintf("%s(%s)", username, title)
		}
	}
	return &Peer{
		Input: p.InputPeer(),
		Title: sanitizeTitle(title),
		ID:    p.ID(),
	}
}

// bootstrapPeers 扫会话列表，把实体灌进 peers.Storage，用于缓存未命中的首次解析。
//
// messages.getDialogs 是 Telegram 限流最严的接口之一，所以这里：
// 命中目标立即停止翻页；翻页游标不前进时立即终止（否则会重复请求同一页）。
// Manager.Apply 会自动跳过 min 实体——它们没有可用的 access_hash，
// 写进缓存反而会污染后续解析。
func (pm *PeerManager) bootstrapPeers(ctx context.Context, wantID int64) error {
	const pageSize = 100
	const maxPages = 200 // 安全上限

	offset := tg.InputPeerClass(&tg.InputPeerEmpty{})
	for page := 0; page < maxPages; page++ {
		resp, err := pm.mgr.API().MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
			OffsetPeer: offset,
			Limit:      pageSize,
		})
		if err != nil {
			return fmt.Errorf("读取会话列表失败: %w", err)
		}

		var (
			dialogs []tg.DialogClass
			chats   []tg.ChatClass
			users   []tg.UserClass
		)
		switch v := resp.(type) {
		case *tg.MessagesDialogs:
			dialogs, chats, users = v.Dialogs, v.Chats, v.Users
		case *tg.MessagesDialogsSlice:
			dialogs, chats, users = v.Dialogs, v.Chats, v.Users
		case *tg.MessagesDialogsNotModified:
			return nil
		default:
			return fmt.Errorf("未知的会话列表响应 %T", resp)
		}

		if err := pm.mgr.Apply(ctx, users, chats); err != nil {
			return fmt.Errorf("写入 peer 缓存失败: %w", err)
		}
		if containsPeerID(chats, users, wantID) {
			return nil
		}
		if len(dialogs) < pageSize {
			return nil // 已经翻到底
		}

		last, ok := dialogs[len(dialogs)-1].(*tg.Dialog)
		if !ok || last.Peer == nil {
			return nil
		}
		// 翻页游标需要用带 access_hash 的 InputPeer，从本次响应里的实体补全
		next := toInputPeer(last.Peer, chats, users)
		if samePeer(offset, next) {
			return nil // 游标没前进，继续请求只会拿到同一页
		}
		offset = next
	}
	return nil
}

// containsPeerID 判断本页实体里有没有目标 ID，用于命中后提前结束翻页。
// 频道、基础群、用户三种都要看：-id 可能属于任意一种。
func containsPeerID(chats []tg.ChatClass, users []tg.UserClass, wantID int64) bool {
	for _, c := range chats {
		switch ch := c.(type) {
		case *tg.Channel:
			if ch.ID == wantID {
				return true
			}
		case *tg.Chat:
			if ch.ID == wantID {
				return true
			}
		}
	}
	for _, u := range users {
		if user, ok := u.(*tg.User); ok && user.ID == wantID {
			return true
		}
	}
	return false
}

// peerKind 是数字 ID 隐含的会话类型。
type peerKind int

const (
	kindUnknown peerKind = iota // 不带前缀的正数：可能是频道（常见）也可能是用户
	kindChannel                 // Telegram 私密频道/超级群：-100XXXXXXXXXX
	kindChat                    // 基础群：-XXXXXXXXXX
)

// parsePeerID 把用户输入的数字归一成 MTProto ID，并给出类型提示。
//
// 这里用 constant.TDLibPeerID 的算术编码，而不是剥字符串前缀：
// 频道是 -1000000000000 - p、基础群是 -p、用户是 p，
// 12 位频道 ID（如 -1997852516352 → 997852516352）用字符串剥 "-100" 会算错。
func parsePeerID(arg string) (int64, peerKind, error) {
	raw, err := strconv.ParseInt(strings.TrimSpace(arg), 10, 64)
	if err != nil {
		return 0, kindUnknown, fmt.Errorf("无法解析 ID %q: %w", arg, err)
	}

	switch td := constant.TDLibPeerID(raw); {
	case td.IsChannel():
		return td.ToPlain(), kindChannel, nil
	case td.IsChat():
		return td.ToPlain(), kindChat, nil
	case td.IsUser():
		// 正数在 TDLib 语义里是用户，但用户常直接写不带前缀的频道 ID，
		// 所以这里不当作确定类型，交给调用方按频道优先依次尝试。
		return td.ToPlain(), kindUnknown, nil
	}
	return 0, kindUnknown, fmt.Errorf("无法识别的 ID %q", arg)
}

// toInputPeer 把会话列表里的 Peer 转成带 access_hash 的 InputPeer，用于翻页游标。
func toInputPeer(p tg.PeerClass, chats []tg.ChatClass, users []tg.UserClass) tg.InputPeerClass {
	switch v := p.(type) {
	case *tg.PeerChannel:
		for _, c := range chats {
			if ch, ok := c.(*tg.Channel); ok && ch.ID == v.ChannelID {
				return &tg.InputPeerChannel{ChannelID: ch.ID, AccessHash: ch.AccessHash}
			}
		}
		return &tg.InputPeerChannel{ChannelID: v.ChannelID}
	case *tg.PeerUser:
		for _, u := range users {
			if user, ok := u.(*tg.User); ok && user.ID == v.UserID {
				return &tg.InputPeerUser{UserID: user.ID, AccessHash: user.AccessHash}
			}
		}
		return &tg.InputPeerUser{UserID: v.UserID}
	case *tg.PeerChat:
		return &tg.InputPeerChat{ChatID: v.ChatID}
	}
	return &tg.InputPeerEmpty{}
}

// samePeer 判断两个 InputPeer 是否指向同一个会话，用于检测翻页卡住。
func samePeer(a, b tg.InputPeerClass) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	switch x := a.(type) {
	case *tg.InputPeerEmpty:
		_, ok := b.(*tg.InputPeerEmpty)
		return ok
	case *tg.InputPeerChannel:
		y, ok := b.(*tg.InputPeerChannel)
		return ok && x.ChannelID == y.ChannelID && x.AccessHash == y.AccessHash
	case *tg.InputPeerUser:
		y, ok := b.(*tg.InputPeerUser)
		return ok && x.UserID == y.UserID && x.AccessHash == y.AccessHash
	case *tg.InputPeerChat:
		y, ok := b.(*tg.InputPeerChat)
		return ok && x.ChatID == y.ChatID
	}
	return false
}
