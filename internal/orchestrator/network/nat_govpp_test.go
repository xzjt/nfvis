package network

// nat_govpp.go 的幂等判定单测：add 方向命中「已存在」必须按成功处理（round84 R84-20）。
// 用假 api.Channel 注入 VPP 应答，不依赖真 VPP，开发机可跑。

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.fd.io/govpp/api"
	"go.fd.io/govpp/binapi/ip_types"
	"go.fd.io/govpp/binapi/nat44_ei"

	"github.com/xzjt/nfvis/internal/model"
)

// fakeAPIChannel 假 govpp API 通道：按请求类型应答并记录收到的请求。
type fakeAPIChannel struct {
	reply      func(msg api.Message) error                  // 应答错误（nil = 正常应答）
	fill       func(msg api.Message)                        // 应答前填充 reply 字段（可选，模拟 VPP 返回值）
	multi      func(msg api.Message) bool                   // 多请求逐条填充明细（返回 false = 明细已给完）
	multiByReq func(req api.Message) func(api.Message) bool // 按请求类型分派明细填充（可选，与 multi 互斥）
	multiErr   func(req api.Message) error                  // 多请求的应答错误（可选）
	sent       []api.Message
}

func (f *fakeAPIChannel) SendRequest(msg api.Message) api.RequestCtx {
	f.sent = append(f.sent, msg)
	rc := fakeRequestCtx{fill: f.fill}
	if f.reply != nil {
		rc.err = f.reply(msg)
	}
	return rc
}

func (f *fakeAPIChannel) SendMultiRequest(msg api.Message) api.MultiRequestCtx {
	f.sent = append(f.sent, msg)
	fill := f.multi
	if f.multiByReq != nil {
		fill = f.multiByReq(msg)
	}
	ctx := fakeMultiCtx{fill: fill}
	if f.multiErr != nil {
		ctx.err = f.multiErr(msg)
	}
	return ctx
}
func (f *fakeAPIChannel) SubscribeNotification(chan api.Message, api.Message) (api.SubscriptionCtx, error) {
	return nil, nil
}
func (f *fakeAPIChannel) SetReplyTimeout(time.Duration)          {}
func (f *fakeAPIChannel) CheckCompatiblity(...api.Message) error { return nil }
func (f *fakeAPIChannel) Close()                                 {}

type fakeRequestCtx struct {
	err  error
	fill func(api.Message)
}

func (c fakeRequestCtx) ReceiveReply(msg api.Message) error {
	if c.fill != nil {
		c.fill(msg)
	}
	return c.err
}

// fakeMultiCtx 假多请求应答：fill 逐条填充明细，填不出即视为结束（没有更多明细）。
type fakeMultiCtx struct {
	fill func(api.Message) bool
	err  error
}

func (c fakeMultiCtx) ReceiveReply(msg api.Message) (bool, error) {
	if c.err != nil {
		return false, c.err
	}
	if c.fill == nil || !c.fill(msg) {
		return true, nil
	}
	return false, nil
}

// natClientWithIfaceStub 组合客户端：接口索引走桩（不起 dump 请求），
// NAT 下发走真实 govpp 客户端（由假通道注入 VPP 应答）。
type natClientWithIfaceStub struct {
	*govppNatClient
	ifaces map[string]uint32
}

func (c natClientWithIfaceStub) SwInterfaceIndex(ifname string) (uint32, bool, error) {
	idx, ok := c.ifaces[ifname]
	return idx, ok, nil
}

// add 方向收到 -81（Value already exists）必须幂等成功：重放（恢复收敛、连接重建后的全量
// 下发）必然重复下发同一对象；此前只容忍 del 方向，于是重放时**整个 ApplyNAT 中止**，
// 只留一条 WARN + 未收敛项，而配置与 show nat 看起来完全正常。
func TestNatGovppAddIdempotentOnValueExist(t *testing.T) {
	calls := []struct {
		name string
		call func(c NatClient) error
	}{
		{"地址池", func(c NatClient) error { return c.NATAddressRange(true, "203.0.113.10", "203.0.113.20", 0) }},
		{"接口地址", func(c NatClient) error { return c.NATInterfaceAddr(true, 2) }},
		{"静态映射", func(c NatClient) error { return c.NATStatic(true, "10.0.0.5", "203.0.113.1") }},
	}
	for _, tc := range calls {
		ch := &fakeAPIChannel{reply: func(api.Message) error { return api.VPPApiError(vppValueExist) }}
		if err := tc.call(&govppNatClient{ch: ch}); err != nil {
			t.Errorf("%s add 命中 -81 应幂等成功，实际 %v", tc.name, err)
		}
		// 其它错误照旧上抛：不得把真错误当幂等吞掉
		ch = &fakeAPIChannel{reply: func(api.Message) error { return errors.New("boom") }}
		if err := tc.call(&govppNatClient{ch: ch}); err == nil {
			t.Errorf("%s 非 -81 错误必须上抛", tc.name)
		}
	}
}

// round84 R84-27：VPP 26.06 的 nat44_ei_user_session_dump 是**按用户** dump（要带具体内网
// 地址 + 该用户的租户 VRF），发 0.0.0.0/vrf 0 时 VPP 只当「查地址 0.0.0.0 那个用户」→ 恒 0 条，
// 而 NAT 明明在转发。本测试钉住正确调用序列：先一次 user_dump 列用户，再逐用户按其地址 +
// **user_dump 返回的** VRF 查会话，并把各用户的会话汇总。
func TestNatGovppSessionsDumpPerUser(t *testing.T) {
	users := []struct {
		ip       string
		vrf      uint32
		sessions int
	}{
		{"192.168.200.10", 8922296, 2},
		{"192.168.200.11", 8922296, 1},
	}
	userIdx := 0
	ch := &fakeAPIChannel{multiByReq: func(req api.Message) func(api.Message) bool {
		switch r := req.(type) {
		case *nat44_ei.Nat44EiUserDump:
			return func(msg api.Message) bool {
				if userIdx >= len(users) {
					return false
				}
				d, ok := msg.(*nat44_ei.Nat44EiUserDetails)
				if !ok {
					return false
				}
				addr, err := ip_types.ParseIP4Address(users[userIdx].ip)
				if err != nil {
					return false
				}
				d.IPAddress, d.VrfID = addr, users[userIdx].vrf
				d.Nsessions = uint32(users[userIdx].sessions)
				userIdx++
				return true
			}
		case *nat44_ei.Nat44EiUserSessionDump:
			// 只有地址与租户 VRF 都对得上才答得出来（真 VPP 查不到用户即空应答）
			match := -1
			for i, u := range users {
				addr, err := ip_types.ParseIP4Address(u.ip)
				if err == nil && r.IPAddress == addr && r.VrfID == u.vrf {
					match = i
				}
			}
			got := 0
			return func(msg api.Message) bool {
				if match < 0 || got >= users[match].sessions {
					return false
				}
				d, ok := msg.(*nat44_ei.Nat44EiUserSessionDetails)
				if !ok {
					return false
				}
				in, _ := ip_types.ParseIP4Address(users[match].ip)
				out, _ := ip_types.ParseIP4Address("192.168.155.62")
				d.InsideIPAddress, d.OutsideIPAddress = in, out
				d.InsidePort, d.OutsidePort = uint16(570+got), uint16(19948+got)
				d.Protocol, d.TotalPkts = 1, uint32(got+1)
				got++
				return true
			}
		}
		return nil
	}}

	rows, err := (&govppNatClient{ch: ch}).NATSessions()
	if err != nil {
		t.Fatalf("NATSessions: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("两个用户的会话应汇总为 3 条，实际 %d 条: %+v", len(rows), rows)
	}
	// 调用序列：1 次 user_dump + 每用户 1 次 session_dump，且请求里带的是该用户的地址与租户 VRF
	if len(ch.sent) != 1+len(users) {
		t.Fatalf("应为 1 次用户 dump + %d 次会话 dump，实际 %d 个请求", len(users), len(ch.sent))
	}
	if _, ok := ch.sent[0].(*nat44_ei.Nat44EiUserDump); !ok {
		t.Fatalf("首个请求必须是用户 dump，实际 %T", ch.sent[0])
	}
	for i, u := range users {
		req, ok := ch.sent[1+i].(*nat44_ei.Nat44EiUserSessionDump)
		if !ok {
			t.Fatalf("第 %d 个请求应为会话 dump，实际 %T", i+1, ch.sent[1+i])
		}
		addr, err := ip_types.ParseIP4Address(u.ip)
		if err != nil {
			t.Fatalf("解析 %s: %v", u.ip, err)
		}
		if req.IPAddress != addr || req.VrfID != u.vrf {
			t.Fatalf("会话 dump 必须带用户的地址与租户 VRF（应为 %s/%d，实际 %s/%d）",
				u.ip, u.vrf, req.IPAddress, req.VrfID)
		}
	}
	// 字段映射（内网/外网地址端口、包数）
	for _, r := range rows {
		if r.OutsideIP != "192.168.155.62" || r.InsideIP != "192.168.200.10" && r.InsideIP != "192.168.200.11" {
			t.Fatalf("会话字段映射不符: %+v", r)
		}
		if r.InsidePort < 570 || r.OutsidePort < 19948 || r.Packets == 0 {
			t.Fatalf("会话端口/计数不符: %+v", r)
		}
	}
}

// 一个用户都没有（NAT 还没有任何用户）→ 如实返回空，且不再发会话 dump；空结果不是错误。
func TestNatGovppSessionsNoUsers(t *testing.T) {
	ch := &fakeAPIChannel{multi: func(api.Message) bool { return false }}
	rows, err := (&govppNatClient{ch: ch}).NATSessions()
	if err != nil {
		t.Fatalf("空用户表不是错误: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("无用户应返回空，实际 %+v", rows)
	}
	if len(ch.sent) != 1 {
		t.Fatalf("无用户时不应再发会话 dump，实际发了 %d 个请求", len(ch.sent))
	}
}

// dump 读取失败必须上抛，不得静默返回空：静默空 = 重新变成「答非所问」。
func TestNatGovppSessionsDumpErrorPropagates(t *testing.T) {
	// ① 列用户就失败
	ch := &fakeAPIChannel{multiErr: func(api.Message) error { return errors.New("连接已断") }}
	if _, err := (&govppNatClient{ch: ch}).NATSessions(); err == nil {
		t.Fatal("列用户失败必须上抛")
	}
	// ② 列用户成功、会话 dump 失败
	userIdx := 0
	ch = &fakeAPIChannel{
		multiByReq: func(req api.Message) func(api.Message) bool {
			if _, ok := req.(*nat44_ei.Nat44EiUserDump); !ok {
				return nil
			}
			return func(msg api.Message) bool {
				if userIdx > 0 {
					return false
				}
				d, ok := msg.(*nat44_ei.Nat44EiUserDetails)
				if !ok {
					return false
				}
				addr, _ := ip_types.ParseIP4Address("192.168.200.10")
				d.IPAddress, d.VrfID = addr, 8922296
				userIdx++
				return true
			}
		},
		multiErr: func(req api.Message) error {
			if _, ok := req.(*nat44_ei.Nat44EiUserSessionDump); ok {
				return errors.New("会话 dump 失败")
			}
			return nil
		},
	}
	if _, err := (&govppNatClient{ch: ch}).NATSessions(); err == nil {
		t.Fatal("会话 dump 失败必须上抛")
	}
}

// 地址池的增删必须带 inside（租户）转发域的 vrf_id：该值会被 VPP 折算成池地址的 FIB 索引，
// in2out 分配端口只认与入接口同一张表的池地址；传 0（默认表）或 outside 表时对不上，
// 包进了 NAT 却分配不出端口——`show errors` 见 out of ports、会话恒为 0、无任何报错（R84-24）。
func TestNatGovppAddressRangeCarriesVRF(t *testing.T) {
	ch := &fakeAPIChannel{}
	if err := (&govppNatClient{ch: ch}).NATAddressRange(true, "203.0.113.10", "203.0.113.20", 7); err != nil {
		t.Fatalf("NATAddressRange: %v", err)
	}
	if len(ch.sent) != 1 {
		t.Fatalf("应下发 1 个请求，实际 %d", len(ch.sent))
	}
	req, ok := ch.sent[0].(*nat44_ei.Nat44EiAddDelAddressRange)
	if !ok {
		t.Fatalf("请求类型不符: %T", ch.sent[0])
	}
	if req.VrfID != 7 {
		t.Fatalf("地址池必须落在 outside 转发域（vrf_id 应为 7，实际 %d）", req.VrfID)
	}
	if !req.IsAdd {
		t.Fatal("首轮应为 add")
	}
}

// 地址池转发域读回：VPP 的 address dump 每条带 tenant VRF（~0 = 与 VRF 无关的接口地址）。
// 收敛要靠它判断「同一地址是否已被按别的转发域下发过」。
func TestNatGovppAddressVRFs(t *testing.T) {
	rows := []struct {
		ip  string
		vrf uint32
	}{{"192.168.155.62", 9726253}, {"192.168.155.61", ^uint32(0)}}
	i := 0
	ch := &fakeAPIChannel{multi: func(msg api.Message) bool {
		if i >= len(rows) {
			return false
		}
		d, ok := msg.(*nat44_ei.Nat44EiAddressDetails)
		if !ok {
			return false
		}
		addr, err := ip_types.ParseIP4Address(rows[i].ip)
		if err != nil {
			return false
		}
		d.IPAddress, d.VrfID = addr, rows[i].vrf
		i++
		return true
	}}
	got, err := (&govppNatClient{ch: ch}).NATAddressVRFs()
	if err != nil {
		t.Fatalf("NATAddressVRFs: %v", err)
	}
	if len(got) != 2 || got["192.168.155.62"] != 9726253 || got["192.168.155.61"] != ^uint32(0) {
		t.Fatalf("读回应为 192.168.155.62→9726253 / 192.168.155.61→~0，实际 %v", got)
	}
}

// 移除方向容忍「已是目标状态」：对象本就不在（-6 No such entry）、形式已存在（-81）、
// 插件已关（-169）。不容忍则一次无害的重复删除会把整批 apply 打回滚
// （round84 实测：移除接口的 NAT inside 特性报 -6 → 整批补偿回滚）。
// add 方向不得容忍 -6（那是真错误：对象本就不存在，目标状态并没有达成）。
func TestNatGovppRemovalToleratesAlreadyGone(t *testing.T) {
	removals := []struct {
		name string
		call func(c NatClient) error
	}{
		{"地址池", func(c NatClient) error { return c.NATAddressRange(false, "203.0.113.10", "203.0.113.20", 7) }},
		{"接口特性", func(c NatClient) error { return c.NATFeature(3, true, false) }},
		{"接口地址", func(c NatClient) error { return c.NATInterfaceAddr(false, 3) }},
		{"静态映射", func(c NatClient) error { return c.NATStatic(false, "10.0.0.5", "203.0.113.1") }},
	}
	for _, tc := range removals {
		for _, code := range []int32{vppNoSuchEntry, vppValueExist, vppFeatureAlreadyDisabled} {
			ch := &fakeAPIChannel{reply: func(api.Message) error { return api.VPPApiError(code) }}
			if err := tc.call(&govppNatClient{ch: ch}); err != nil {
				t.Errorf("%s 移除命中 %d 应幂等成功，实际 %v", tc.name, code, err)
			}
		}
		// 其它错误照旧上抛
		ch := &fakeAPIChannel{reply: func(api.Message) error { return api.VPPApiError(-114) }}
		if err := tc.call(&govppNatClient{ch: ch}); err == nil {
			t.Errorf("%s 移除遇到非幂等错误必须上抛", tc.name)
		}
	}
	// add 方向遇 -6 必须上抛（对象不存在 ≠ 已达目标状态）
	adds := []struct {
		name string
		call func(c NatClient) error
	}{
		{"地址池", func(c NatClient) error { return c.NATAddressRange(true, "203.0.113.10", "203.0.113.20", 7) }},
		{"接口特性", func(c NatClient) error { return c.NATFeature(3, true, true) }},
		{"接口地址", func(c NatClient) error { return c.NATInterfaceAddr(true, 3) }},
		{"静态映射", func(c NatClient) error { return c.NATStatic(true, "10.0.0.5", "203.0.113.1") }},
	}
	for _, tc := range adds {
		ch := &fakeAPIChannel{reply: func(api.Message) error { return api.VPPApiError(vppNoSuchEntry) }}
		if err := tc.call(&govppNatClient{ch: ch}); err == nil {
			t.Errorf("%s add 遇 -6 必须上抛（不得当成幂等）", tc.name)
		}
	}
}

// 地址池/接口地址已在位（-81）时 ApplyNAT 必须走完全部步骤，而不是停在原地——否则
// inside/outside 特性都不在，NAT 静默失效。两条路径：地址池承担外部地址（池地址与出接口
// 地址错开）、无池时以出接口地址为外部地址。
func TestApplyNATContinuesWhenObjectsAlreadyExist(t *testing.T) {
	withPool := model.NatConfig{
		SourcePools: []model.NatSourcePool{{Name: "pool-a", AddressRange: "192.168.155.62"}},
		Rules: []model.NatRule{{Seq: 10, MatchSource: "192.168.200.0/24", VirtualSwitch: "vs-nat",
			Action: model.NatAction{SourcePool: "pool-a", Interface: "ens224"}}},
	}
	noPool := model.NatConfig{Rules: []model.NatRule{{Seq: 10, MatchSource: "192.168.200.0/24",
		VirtualSwitch: "vs-nat", Action: model.NatAction{Interface: "ens224"}}}}

	for _, tc := range []struct {
		name string
		cfg  model.NatConfig
	}{{"地址池已在位", withPool}, {"接口地址已在位", noPool}} {
		ch := &fakeAPIChannel{reply: func(msg api.Message) error {
			switch msg.(type) {
			case *nat44_ei.Nat44EiAddDelAddressRange, *nat44_ei.Nat44EiAddDelInterfaceAddr:
				return api.VPPApiError(vppValueExist) // VPP：该地址/接口已在位
			}
			return nil
		}}
		p := NewNatProviderFunc(func() (NatClient, error) {
			return natClientWithIfaceStub{&govppNatClient{ch: ch},
				map[string]uint32{"ens192": 1, "ens224": 2}}, nil
		})
		p.SetInsideResolver(func(string) []uint32 { return []uint32{5} }) // vNIC 已登记为 inside
		if err := p.ApplyNAT(context.Background(), tc.cfg); err != nil {
			t.Fatalf("%s：重放不得中止: %v", tc.name, err)
		}
		var inside, outside bool
		for _, msg := range ch.sent {
			f, ok := msg.(*nat44_ei.Nat44EiInterfaceAddDelFeature)
			if !ok || !f.IsAdd {
				continue
			}
			if uint32(f.SwIfIndex) == 5 && f.Flags == nat44_ei.NAT44_EI_IF_INSIDE {
				inside = true
			}
			if uint32(f.SwIfIndex) == 2 && f.Flags == nat44_ei.NAT44_EI_IF_OUTSIDE {
				outside = true
			}
		}
		if !inside || !outside {
			t.Fatalf("%s：后续步骤必须照常完成（inside=%v outside=%v）", tc.name, inside, outside)
		}
	}
}
