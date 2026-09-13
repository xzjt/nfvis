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
)

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
}

// AlarmStore 进程内告警表（并发安全）。
type AlarmStore struct {
	mu    sync.Mutex
	seq   int
	byKey map[string]*Alarm // scope\x00code\x00source → 告警

	now    func() time.Time // 注入时钟（测试确定性）
	notify func(Alarm)      // 变更通知（M5-1 事件总线；锁内调用须快速返回）
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
			s.emitLocked(a)
		}
		return
	}
	s.seq++
	a := &Alarm{
		ID: fmt.Sprintf("alm-%05d", s.seq), Severity: severity, Code: code,
		Message: message, Source: source, RaisedAt: s.now(), State: AlarmActive,
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
				s.emitLocked(a)
			}
			continue
		}
		s.seq++
		al := &Alarm{
			ID: fmt.Sprintf("alm-%05d", s.seq), Severity: f.Severity, Code: f.Code,
			Message: f.Message, Source: f.Source, RaisedAt: s.now(), State: AlarmActive,
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
