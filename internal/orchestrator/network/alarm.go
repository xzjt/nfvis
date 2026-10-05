package network

// M3-8：恢复收敛的告警落点（FR-OPS-010）。
//
// 进程内告警表：恢复收敛无法补齐的项（如配置引用的物理口已被移除）记入告警而非
// 阻塞启动，经 GET /alarms 暴露。M5 事件总线/持久化告警表就绪后替换本实现，
// 上层接口（List）保持不变。

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xzjt/nfvis/internal/clocksync"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// 告警严重级别（契约 Alarm.severity）。
const (
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityError    = "error"
	SeverityCritical = "critical"
)

// 告警状态（契约 Alarm.state）。
const (
	AlarmActive   = "active"
	AlarmResolved = "resolved"
)

// recoveryScope 恢复收敛产生的告警作用域（Sync 只收敛本作用域的活动项）。
const recoveryScope = "recovery"

// 恢复收敛告警码。
const (
	// AlarmUnconverged 对象下发失败（可能为暂时性错误），严重级别 warning。
	AlarmUnconverged = "RECOVERY_UNCONVERGED"
	// AlarmIfaceMissing 配置引用的接口已不存在（不可收敛），严重级别 error。
	AlarmIfaceMissing = "RECOVERY_IFACE_MISSING"
	// AlarmTableLeftover 数据面存在**配置未声明**的 IP 表：删表延后（NAT 用过的表 VPP 不释放
	// 引用）或提交补偿失败留下的残渣（决策 #192）。它不阻塞任何配置，但「配置与数据面不一致」
	// 必须有人看得到——此前只出现在当次提交输出里，事后无从查证（round86 R86-9）。
	// 残留随数据面重启消失（VPP 的 IP 表是运行态），恢复收敛据此自动消警。
	AlarmTableLeftover = "VRF_TABLE_LEFTOVER"
	// AlarmACLLeftover 数据面存在**配置未声明**的 ACL（tag 不在配置里）：提交补偿失败留下的
	// 残渣（决策 #321，把非 VRF 表类残渣纳入与 #192 同一份对账视野）。与 VRF_TABLE_LEFTOVER
	// 同口径：不靠进程内记忆，恢复收敛/巡检按数据面事实重建，随对象消失自动消警、跨 nfvisd 重启可见。
	AlarmACLLeftover = "ACL_LEFTOVER"
	// AlarmBDLeftover 数据面存在**配置未声明**的 bridge-domain（BD-Tag 不在配置里）：
	// 同上（决策 #321）。BD 名取 BD-Tag，无名时以 BD ID 标识。
	AlarmBDLeftover = "BRIDGE_DOMAIN_LEFTOVER"
	// AlarmVPPAutostartFailed nfvisd 启动时未能**发起**拉起 VPP（决策 #348）：发起动作失败
	// 或被取消。就绪由既有连接重试循环接管（未就绪时数据面不可用、`show vpp` 显示未连接）。
	// 严重级别 warning；VPP 恢复在线即自动消解（与 #329/#346 同口径）。
	AlarmVPPAutostartFailed = "VPP_AUTOSTART_FAILED"
)

// ResidueCodes 全部「残渣对账」告警码（消解只在本集合内进行，不误伤恢复收敛的其它告警）。
func ResidueCodes() []string {
	return []string{AlarmTableLeftover, AlarmACLLeftover, AlarmBDLeftover}
}

// IsResidueCode 报告该告警码是否为残渣对账码。
func IsResidueCode(code string) bool {
	switch code {
	case AlarmTableLeftover, AlarmACLLeftover, AlarmBDLeftover:
		return true
	}
	return false
}

// Alarm 一条告警（契约 components/schemas/Alarm）。
type Alarm struct {
	ID         string     `json:"id"`
	Severity   string     `json:"severity"`
	Code       string     `json:"code"`
	Message    string     `json:"message"`
	Source     string     `json:"source,omitempty"`
	RaisedAt   time.Time  `json:"raised_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	State      string     `json:"state"`
	// TimeSynced 记录该告警（创建/重新激活）时宿主时钟是否已与 NTP 同步（NFR-006，
	// 三态：true 已同步 / false 未同步 / nil 未知）。与审计记录同一套语义，由
	// clocksync.Mark 单一事实源计算——探针未接入时为 nil，**不谎称已同步**。
	TimeSynced *bool `json:"time_synced,omitempty"`
}

// AlarmStore 进程内告警表（并发安全）。
type AlarmStore struct {
	mu    sync.Mutex
	seq   int
	byKey map[string]*Alarm // scope\x00code\x00source → 告警

	now    func() time.Time // 注入时钟（测试确定性）
	notify func(Alarm)      // 变更通知（M5-1 事件总线；锁内调用须快速返回）
	clock  func() bool      // 时钟同步探针（NFR-006；未注入 ⇒ 标记为未知）
}

// SetClockProbe 注入「宿主时钟是否已同步」探针（NFR-006，与审计侧同一 system.ClockSynced）。
// 告警创建或重新激活时按**同一时刻的事实**打三态标记；未注入时保持 nil（未知，不谎称已同步）。
func (s *AlarmStore) SetClockProbe(probe func() bool) {
	s.mu.Lock()
	s.clock = probe
	s.mu.Unlock()
}

// SetNotifier 注入告警变更通知（新增/重新激活时 state=active，消警时 state=resolved）。
func (s *AlarmStore) SetNotifier(f func(Alarm)) {
	s.mu.Lock()
	s.notify = f
	s.mu.Unlock()
}

func (s *AlarmStore) emitLocked(a *Alarm) {
	if s.notify != nil {
		s.notify(*a)
	}
}

// NewAlarmStore 构造空告警表。
func NewAlarmStore() *AlarmStore {
	return &AlarmStore{byKey: map[string]*Alarm{}, now: time.Now}
}

func alarmKey(scope, code, source string) string {
	return scope + "\x00" + code + "\x00" + source
}

// Raise 记录一条活动告警（同 scope+code+source 幂等：已存在则更新级别与消息；
// 已 resolved 后再次触发则重新激活并刷新 raised_at）。
func (s *AlarmStore) Raise(scope, severity, code, message, source string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := alarmKey(scope, code, source)
	if a, ok := s.byKey[k]; ok {
		wasResolved := a.State == AlarmResolved
		a.Severity, a.Message = severity, message
		if wasResolved {
			a.State, a.ResolvedAt, a.RaisedAt = AlarmActive, nil, s.now()
			// 重新激活刷新了 RaisedAt，故按**当时**的探针事实重打标，不沿用旧值（NFR-006）。
			a.TimeSynced = clocksync.Mark(s.clock)
			s.emitLocked(a)
		}
		return
	}
	s.seq++
	a := &Alarm{
		ID: fmt.Sprintf("alm-%05d", s.seq), Severity: severity, Code: code,
		Message: message, Source: source, RaisedAt: s.now(), State: AlarmActive,
		TimeSynced: clocksync.Mark(s.clock),
	}
	s.byKey[k] = a
	s.emitLocked(a)
}

// Resolve 将同 scope+code+source 的活动告警置为 resolved；返回是否命中。
func (s *AlarmStore) Resolve(scope, code, source string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.byKey[alarmKey(scope, code, source)]
	if !ok || a.State == AlarmResolved {
		return false
	}
	t := s.now()
	a.State, a.ResolvedAt = AlarmResolved, &t
	s.emitLocked(a)
	return true
}

// Sync 用 failures 覆盖 scope 内的活动告警集合：不在 failures 中的活动项自动置 resolved。
// failures 无需携带 scope/id/时间（由本方法填充）。
func (s *AlarmStore) Sync(scope string, failures []Alarm) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := make(map[string]bool, len(failures))
	for _, f := range failures {
		k := alarmKey(scope, f.Code, f.Source)
		keep[k] = true
		if a, ok := s.byKey[k]; ok {
			a.Severity, a.Message = f.Severity, f.Message
			if a.State == AlarmResolved {
				a.State, a.ResolvedAt, a.RaisedAt = AlarmActive, nil, s.now()
				a.TimeSynced = clocksync.Mark(s.clock) // 与 Raise 同口径：重新激活即重打标
				s.emitLocked(a)
			}
			continue
		}
		s.seq++
		al := &Alarm{
			ID: fmt.Sprintf("alm-%05d", s.seq), Severity: f.Severity, Code: f.Code,
			Message: f.Message, Source: f.Source, RaisedAt: s.now(), State: AlarmActive,
			TimeSynced: clocksync.Mark(s.clock),
		}
		s.byKey[k] = al
		s.emitLocked(al)
	}
	prefix := scope + "\x00"
	for k, a := range s.byKey {
		if a.State == AlarmActive && !keep[k] && strings.HasPrefix(k, prefix) {
			t := s.now()
			a.State, a.ResolvedAt = AlarmResolved, &t
			s.emitLocked(a)
		}
	}
}

// Clear 删除已 resolved 的告警（all=true 清全部已 resolved；否则按 id 匹配）。返回删除数量。
// 活动告警不删除（须先恢复/消警，FR-OPS-022）。
func (s *AlarmStore) Clear(id string, all bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, a := range s.byKey {
		if a.State != AlarmResolved {
			continue
		}
		if all || (id != "" && a.ID == id) {
			delete(s.byKey, k)
			n++
		}
	}
	return n
}

// ActiveOf 返回 scope 内全部活动告警的识别信息（按 code、source 升序，便于对账与测试确定）。
//
// 用途：对象从配置里删除后，检查函数不再遍历到它，其告警便无人 Resolve（round86 真机缺陷）。
// 检查函数在查询运行态成功之后按期望集合对账（见 orchestrator.ResolveStale）清掉这类滞留告警。
func (s *AlarmStore) ActiveOf(scope string) []orchestrator.AlarmRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := scope + "\x00"
	out := make([]orchestrator.AlarmRef, 0, 4)
	for k, a := range s.byKey {
		if a.State != AlarmActive || !strings.HasPrefix(k, prefix) {
			continue
		}
		out = append(out, orchestrator.AlarmRef{Code: a.Code, Source: a.Source})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Code == out[j].Code {
			return out[i].Source < out[j].Source
		}
		return out[i].Code < out[j].Code
	})
	return out
}

// List 按状态过滤告警（active|resolved|all，缺省/非法值按 active），按 raised_at 升序。
func (s *AlarmStore) List(state string) []Alarm {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Alarm, 0, len(s.byKey))
	for _, a := range s.byKey {
		switch state {
		case AlarmResolved:
			if a.State != AlarmResolved {
				continue
			}
		case "all":
		default:
			if a.State != AlarmActive {
				continue
			}
		}
		c := *a
		if a.ResolvedAt != nil {
			t := *a.ResolvedAt
			c.ResolvedAt = &t
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RaisedAt.Equal(out[j].RaisedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].RaisedAt.Before(out[j].RaisedAt)
	})
	return out
}
