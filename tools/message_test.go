package tools

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

// messageInvoker 在不使用会话与网络的情况下验证真实 tg.Client 和 query 路由。
type messageInvoker func(context.Context, bin.Encoder, bin.Decoder) error

func (f messageInvoker) Invoke(ctx context.Context, in bin.Encoder, out bin.Decoder) error {
	return f(ctx, in, out)
}

func TestEachMessageHistoryPagination(t *testing.T) {
	for _, kind := range []string{"mixed", "services", "service_tail", "empty_tail", "empty_page"} {
		for _, response := range []string{"messages", "slice", "channel"} {
			t.Run(kind+"/"+response, func(t *testing.T) {
				peer := &tg.InputPeerChannel{ChannelID: 7}
				page := make([]tg.MessageClass, 100)
				var want []int
				for i := range page {
					id := 200 - i
					switch {
					case kind == "empty_page" || kind == "empty_tail" && i == 99:
						page[i] = &tg.MessageEmpty{ID: id}
					case kind == "services" || kind == "mixed" && i%2 == 0 || kind == "service_tail" && i == 99:
						page[i] = &tg.MessageService{ID: id, Date: 1000}
					default:
						page[i] = &tg.Message{ID: id, Date: 1000}
						want = append(want, id)
					}
				}
				want = append(want, 100)
				calls := 0
				api := tg.NewClient(messageInvoker(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
					req, ok := in.(*tg.MessagesGetHistoryRequest)
					if !ok {
						t.Fatalf("unexpected request %T", in)
					}
					if calls > 1 {
						t.Fatal("unexpected extra page")
					}
					wantOffset := 0
					if calls == 1 {
						wantOffset = 101
					}
					if req.OffsetID != wantOffset || req.Limit != 100 || req.MinID != 10 || req.MaxID != 250 || req.Peer != peer {
						t.Fatalf("unexpected history request: %+v; want offset %d", req, wantOffset)
					}
					rows := page
					if calls == 1 {
						rows = []tg.MessageClass{&tg.Message{ID: 100, Date: 1000}}
					}
					calls++
					box := out.(*tg.MessagesMessagesBox)
					switch response {
					case "messages":
						box.Messages = &tg.MessagesMessages{Messages: rows}
					case "slice":
						box.Messages = &tg.MessagesMessagesSlice{Messages: rows}
					case "channel":
						box.Messages = &tg.MessagesChannelMessages{Messages: rows}
					}
					return nil
				}))
				var got []int
				err := EachMessage(context.Background(), api, peer, MsgRange{Mode: RangeBetween, Min: 10, Max: 250}, 0, func(m *tg.Message) error {
					got = append(got, m.ID)
					return nil
				})
				if err != nil || calls != 2 || !reflect.DeepEqual(got, want) {
					t.Fatalf("err=%v calls=%d got=%v want=%v", err, calls, got, want)
				}
			})
		}
	}
}

func TestEachMessageHistorySince(t *testing.T) {
	for _, serviceBoundary := range []bool{false, true} {
		t.Run(map[bool]string{false: "message", true: "service"}[serviceBoundary], func(t *testing.T) {
			rows := []tg.MessageClass{
				&tg.Message{ID: 10, Date: 101},
				&tg.MessageEmpty{ID: 9},
				&tg.MessageService{ID: 8, Date: 100},
				&tg.Message{ID: 7, Date: 100},
			}
			if serviceBoundary {
				rows = append(rows, &tg.MessageService{ID: 6, Date: 99})
			} else {
				rows = append(rows, &tg.Message{ID: 6, Date: 99})
			}
			// 即使由服务消息越过 since，下界之后的记录也不应再被访问。
			rows = append(rows, &tg.Message{ID: 5, Date: 100})
			calls := 0
			api := tg.NewClient(messageInvoker(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
				if _, ok := in.(*tg.MessagesGetHistoryRequest); !ok {
					t.Fatalf("unexpected request %T", in)
				}
				calls++
				out.(*tg.MessagesMessagesBox).Messages = &tg.MessagesMessages{Messages: rows}
				return nil
			}))
			var got []int
			err := EachMessage(context.Background(), api, &tg.InputPeerChat{ChatID: 7}, MsgRange{}, 100, func(m *tg.Message) error {
				got = append(got, m.ID)
				return nil
			})
			if err != nil || calls != 1 || !reflect.DeepEqual(got, []int{10, 7}) {
				t.Fatalf("err=%v calls=%d got=%v", err, calls, got)
			}
		})
	}
}

func TestEachMessageHistoryNoProgress(t *testing.T) {
	for _, id := range []int{0, 100} {
		t.Run(map[int]string{0: "invalid_cursor", 100: "repeated_page"}[id], func(t *testing.T) {
			calls := 0
			api := tg.NewClient(messageInvoker(func(_ context.Context, _ bin.Encoder, out bin.Decoder) error {
				calls++
				if calls > 2 {
					t.Fatal("non-advancing cursor was retried")
				}
				rows := make([]tg.MessageClass, 100)
				for i := range rows {
					rows[i] = &tg.MessageService{ID: id, Date: 100}
				}
				out.(*tg.MessagesMessagesBox).Messages = &tg.MessagesMessagesSlice{Messages: rows}
				return nil
			}))
			err := EachMessage(context.Background(), api, &tg.InputPeerChat{ChatID: 7}, MsgRange{}, 0, func(*tg.Message) error {
				t.Fatal("service message callback")
				return nil
			})
			if err == nil {
				t.Fatal("expected non-advancing cursor error")
			}
		})
	}
}

func TestEachMessageByIDPeer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		peer  tg.InputPeerClass
		match tg.PeerClass
	}{
		{"user", &tg.InputPeerUser{UserID: 7}, &tg.PeerUser{UserID: 7}},
		{"chat", &tg.InputPeerChat{ChatID: 7}, &tg.PeerChat{ChatID: 7}},
		{"channel", &tg.InputPeerChannel{ChannelID: 7, AccessHash: 42}, &tg.PeerChannel{ChannelID: 7}},
		{"self", &tg.InputPeerSelf{}, &tg.PeerUser{UserID: 7}},
	} {
		for _, mode := range []RangeMode{RangeSingle, RangeList} {
			t.Run(tc.name+"/"+map[RangeMode]string{RangeSingle: "single", RangeList: "list"}[mode], func(t *testing.T) {
				selfCalls, messageCalls := 0, 0
				ids := []int{11}
				if mode == RangeList {
					ids = []int{11, 12, 13, 14, 15, 16, 17, 18, 19, 20}
				}
				api := tg.NewClient(messageInvoker(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
					switch req := in.(type) {
					case *tg.UsersGetUsersRequest:
						if tc.name != "self" || len(req.ID) != 1 {
							t.Fatalf("unexpected self lookup: %+v", req)
						}
						if _, ok := req.ID[0].(*tg.InputUserSelf); !ok {
							t.Fatalf("self lookup input %T", req.ID[0])
						}
						selfCalls++
						out.(*tg.UserClassVector).Elems = []tg.UserClass{&tg.User{ID: 7, Self: true}}
						return nil
					case *tg.MessagesGetMessagesRequest:
						if tc.name == "channel" {
							t.Fatal("channel must use channels.getMessages")
						}
						checkMessageIDs(t, req.ID, ids)
					case *tg.ChannelsGetMessagesRequest:
						ch, ok := req.Channel.(*tg.InputChannel)
						if tc.name != "channel" || !ok || ch.ChannelID != 7 || ch.AccessHash != 42 {
							t.Fatalf("unexpected channel request %+v", req)
						}
						checkMessageIDs(t, req.ID, ids)
					default:
						t.Fatalf("unexpected request %T", in)
					}
					messageCalls++
					rows := []tg.MessageClass{&tg.MessageEmpty{ID: 11}, &tg.MessageService{ID: 12}}
					for i, p := range []tg.PeerClass{
						&tg.PeerUser{UserID: 8}, &tg.PeerChat{ChatID: 8}, &tg.PeerChannel{ChannelID: 8},
						&tg.PeerUser{UserID: 7}, &tg.PeerChat{ChatID: 7}, &tg.PeerChannel{ChannelID: 7}, nil,
					} {
						rows = append(rows, &tg.Message{ID: 13 + i, PeerID: p})
					}
					rows = append(rows, &tg.Message{ID: 20, PeerID: tc.match})
					if mode == RangeSingle {
						// 单个请求的 ID 也可能属于其他会话。
						rows = []tg.MessageClass{&tg.Message{ID: 11, PeerID: &tg.PeerUser{UserID: 8}}}
					}
					out.(*tg.MessagesMessagesBox).Messages = &tg.MessagesMessages{Messages: rows}
					return nil
				}))
				var got []int
				err := EachMessage(context.Background(), api, tc.peer, MsgRange{Mode: mode, IDs: ids}, 999, func(m *tg.Message) error {
					got = append(got, m.ID)
					return nil
				})
				want := []int{16, 20}
				if tc.name == "chat" {
					want[0] = 17
				} else if tc.name == "channel" {
					want[0] = 18
				}
				if mode == RangeSingle {
					want = nil
				}
				wantSelf := 0
				if tc.name == "self" {
					wantSelf = 1
				}
				if err != nil || selfCalls != wantSelf || messageCalls != 1 || !reflect.DeepEqual(got, want) {
					t.Fatalf("err=%v selfCalls=%d messageCalls=%d got=%v want=%v", err, selfCalls, messageCalls, got, want)
				}
			})
		}
	}
}

func checkMessageIDs(t *testing.T, got []tg.InputMessageClass, want []int) {
	t.Helper()
	var ids []int
	for _, m := range got {
		id, ok := m.(*tg.InputMessageID)
		if !ok {
			t.Fatalf("unexpected input message %T", m)
		}
		ids = append(ids, id.ID)
	}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("requested IDs=%v want=%v", ids, want)
	}
}

func TestEachMessageSelfLookupFailure(t *testing.T) {
	lookupErr := errors.New("self lookup failed")
	for _, tc := range []struct {
		name  string
		users []tg.UserClass
		err   error
	}{
		{name: "rpc_error", err: lookupErr},
		{name: "empty"},
		{name: "deleted", users: []tg.UserClass{&tg.UserEmpty{ID: 7}}},
		{name: "zero_id", users: []tg.UserClass{&tg.User{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			api := tg.NewClient(messageInvoker(func(_ context.Context, in bin.Encoder, out bin.Decoder) error {
				if _, ok := in.(*tg.UsersGetUsersRequest); !ok {
					t.Fatalf("must resolve self before fetching messages, got %T", in)
				}
				calls++
				out.(*tg.UserClassVector).Elems = tc.users
				return tc.err
			}))
			err := EachMessage(context.Background(), api, &tg.InputPeerSelf{}, MsgRange{Mode: RangeSingle, IDs: []int{1}}, 0, func(*tg.Message) error {
				t.Fatal("callback after failed self lookup")
				return nil
			})
			if err == nil || calls != 1 || tc.err != nil && !errors.Is(err, tc.err) {
				t.Fatalf("err=%v lookup calls=%d", err, calls)
			}
		})
	}
}
