package network

// DHCP 租约表与最小状态机（决策 #359，v1）——**纯函数 + 进程内表**，不触碰 VPP/套接字，
// 供 provider 在单锁内串行调用，也便于单测逐条钉住语义（分配/续租/释放/decline 隔离/过期回收）。
//
// 状态机（v1 最小可用）：
//   - DISCOVER → allocate（offered，同 MAC 优先续用原地址，否则最小可用地址）；
//   - REQUEST → commit（active；地址不在本池或属他人 ⇒ 调用方回 NAK，不改表）；
//   - RELEASE → 释放（从表内移除，地址立即可复用）；
//   - DECLINE → 该地址标记 declined **隔离一个租期**（防客户端声明冲突后立即复用回环）；
//   - 到期（offered/active/declined）即从表内移除、地址回池。
//
// 分配策略：**同 MAC 优先续用原地址**（declined 之外的 existing），否则最小可用地址。

import (
	"net"
	"sort"
	"time"
)

// 租约状态（与 openapi DhcpLease.state 枚举逐字一致）。
const (
	dhcpLeaseOffered  = "offered"
	dhcpLeaseActive   = "active"
	dhcpLeaseDeclined = "declined"
)

// dhcpOfferHold OFFER 的保持时长（v1 内部口径，非配置项）：客户端拿到 OFFER 后未完成
// REQUEST 时，该地址在此时长后回池——避免大量 DISCOVER 长期占住地址。租约生效（ACK）后
// 的有效期按 lease-time（option 51），与 DISCOVER 探测无关。**如实登记**：契约未定义
// OFFER 保持时长，本实现取 2 分钟（够完成一次 DORA，又不至于让探测包耗尽小池）。
const dhcpOfferHold = 2 * time.Minute

// dhcpLease 一条租约（持久化到独立文件 /var/lib/nfvis/dhcp/<交换机名>.json 的元素）。
type dhcpLease struct {
	MAC       string    `json:"mac"`
	IP        string    `json:"ip"`
	State     string    `json:"state"`
	ExpiresAt time.Time `json:"expires_at"`
}

// dhcpLeaseTable 一台交换机的租约表（每 MAC 至多一条；按 IP 反查以判定「属他人」）。
type dhcpLeaseTable struct {
	poolLo uint32 // 池起始（含）
	poolHi uint32 // 池结束（含）
	byMAC  map[string]*dhcpLease
	byIP   map[string]*dhcpLease
	now    func() time.Time
}

func newDHCPLeaseTable(lo, hi uint32, now func() time.Time) *dhcpLeaseTable {
	if now == nil {
		now = time.Now
	}
	return &dhcpLeaseTable{poolLo: lo, poolHi: hi, byMAC: map[string]*dhcpLease{}, byIP: map[string]*dhcpLease{}, now: now}
}

// inPool 报告地址是否在池内（含两端）。
func (t *dhcpLeaseTable) inPool(v uint32) bool { return v >= t.poolLo && v <= t.poolHi }

// setPool 换池：范围外的租约直接移除（它们已不可能续租）；范围内未到期的保留。
func (t *dhcpLeaseTable) setPool(lo, hi uint32) {
	t.poolLo, t.poolHi = lo, hi
	for mac, l := range t.byMAC {
		v, ok := ipToU32(l.IP)
		if !ok || !t.inPool(v) {
			delete(t.byMAC, mac)
			delete(t.byIP, l.IP)
		}
	}
}

// sweep 清理已到期条目（含 declined），返回是否有变化。
func (t *dhcpLeaseTable) sweep() bool {
	now := t.now()
	changed := false
	for mac, l := range t.byMAC {
		if !l.ExpiresAt.After(now) {
			delete(t.byMAC, mac)
			delete(t.byIP, l.IP)
			changed = true
		}
	}
	return changed
}

// lookupMAC 取该 MAC 的未到期租约（含 declined；调用方按状态决定可否复用）。
func (t *dhcpLeaseTable) lookupMAC(mac string) *dhcpLease {
	t.sweep()
	return t.byMAC[mac]
}

// ownerOf 该地址当前的未到期租约归属（nil = 空闲）。
func (t *dhcpLeaseTable) ownerOf(ip string) *dhcpLease {
	t.sweep()
	return t.byIP[ip]
}

// lowestFree 最小可用地址（池内、无未到期租约）。
func (t *dhcpLeaseTable) lowestFree() (uint32, bool) {
	t.sweep()
	for v := t.poolLo; ; v++ {
		if t.byIP[u32ToIP(v).String()] == nil {
			return v, true
		}
		if v == t.poolHi {
			return 0, false
		}
	}
}

// allocate 为 DISCOVER 选一个地址并记为 offered（同 MAC 优先续用原地址）。
// 无可分配地址时 ok=false（调用方据此置池耗尽告警且不回 OFFER）。
func (t *dhcpLeaseTable) allocate(mac string) (string, bool) {
	now := t.now()
	t.sweep()
	if l := t.byMAC[mac]; l != nil && l.State != dhcpLeaseDeclined {
		v, ok := ipToU32(l.IP)
		if ok && t.inPool(v) {
			l.State = dhcpLeaseOffered
			l.ExpiresAt = now.Add(dhcpOfferHold)
			return l.IP, true
		}
	}
	v, ok := t.lowestFree()
	if !ok {
		return "", false
	}
	ip := u32ToIP(v).String()
	t.byMAC[mac] = &dhcpLease{MAC: mac, IP: ip, State: dhcpLeaseOffered, ExpiresAt: now.Add(dhcpOfferHold)}
	t.byIP[ip] = t.byMAC[mac]
	return ip, true
}

// commit 把某地址的租约置为 active（REQUEST→ACK）。租期到 now+lease。
func (t *dhcpLeaseTable) commit(mac, ip string, lease time.Duration) {
	now := t.now()
	t.sweep()
	if old := t.byMAC[mac]; old != nil && old.IP != ip {
		delete(t.byIP, old.IP) // 换地址：旧地址回池
	}
	l := t.byMAC[mac]
	if l == nil {
		l = &dhcpLease{MAC: mac, IP: ip}
		t.byMAC[mac] = l
	}
	l.IP, l.State, l.ExpiresAt = ip, dhcpLeaseActive, now.Add(lease)
	t.byIP[ip] = l
}

// release 释放该 MAC 的租约（RELEASE）；返回是否有变化。
func (t *dhcpLeaseTable) release(mac string) bool {
	t.sweep()
	l := t.byMAC[mac]
	if l == nil {
		return false
	}
	delete(t.byMAC, mac)
	delete(t.byIP, l.IP)
	return true
}

// decline 把该地址标记 declined 并隔离一个租期（DECLINE；地址不属于本表时也记录，
// 避免该地址被立刻分配给其他客户端）。返回是否有变化。
func (t *dhcpLeaseTable) decline(ip string, lease time.Duration) bool {
	t.sweep()
	v, ok := ipToU32(ip)
	if !ok || !t.inPool(v) {
		return false
	}
	l := t.byIP[ip]
	if l == nil {
		// 以「地址」为主体记录：MAC 为空（不冒认客户端身份）。
		l = &dhcpLease{IP: ip}
		t.byIP[ip] = l
	}
	if l.State == dhcpLeaseDeclined && l.ExpiresAt.After(t.now()) {
		return false
	}
	l.State, l.ExpiresAt = dhcpLeaseDeclined, t.now().Add(lease)
	if l.MAC != "" {
		t.byMAC[l.MAC] = l
	}
	return true
}

// free 池内是否存在空闲地址（无未到期租约）。
func (t *dhcpLeaseTable) free() bool {
	_, ok := t.lowestFree()
	return ok
}

// activeCount 生效租约数（state=active；offered/declined 不计，与 openapi active_leases 同口径）。
func (t *dhcpLeaseTable) activeCount() int {
	t.sweep()
	n := 0
	for _, l := range t.byMAC {
		if l.State == dhcpLeaseActive {
			n++
		}
	}
	return n
}

// snapshot 按 IP 升序导出未到期租约（读视图/持久化共用；declined 以地址为主体也在内）。
func (t *dhcpLeaseTable) snapshot() []dhcpLease {
	t.sweep()
	out := make([]dhcpLease, 0, len(t.byIP))
	for _, l := range t.byIP {
		out = append(out, *l)
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := ipToU32(out[i].IP)
		b, _ := ipToU32(out[j].IP)
		return a < b
	})
	return out
}

// restore 从持久化数据恢复（只收池内、未到期的条目；同 IP 冲突时保前者）。
func (t *dhcpLeaseTable) restore(leases []dhcpLease) {
	now := t.now()
	for _, l := range leases {
		v, ok := ipToU32(l.IP)
		if !ok || !t.inPool(v) || !l.ExpiresAt.After(now) {
			continue
		}
		switch l.State {
		case dhcpLeaseOffered, dhcpLeaseActive, dhcpLeaseDeclined:
		default:
			continue
		}
		if t.byIP[l.IP] != nil {
			continue
		}
		c := l
		t.byIP[c.IP] = &c
		if c.MAC != "" {
			t.byMAC[c.MAC] = &c
		}
	}
}

// ipToU32 / u32ToIP 池区间计算（与 model.DHCPServerPoolRange 同一算法；本文件不 import
// model 之外的第三方，保持纯函数可测）。
func ipToU32(s string) (uint32, bool) {
	ip := net.ParseIP(s)
	if ip == nil {
		return 0, false
	}
	v4 := ip.To4()
	if v4 == nil {
		return 0, false
	}
	return uint32(v4[0])<<24 | uint32(v4[1])<<16 | uint32(v4[2])<<8 | uint32(v4[3]), true
}

func u32ToIP(v uint32) net.IP {
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}
