package api

// W5：无底座依赖的 API handlers（M2 收尾任务清单 W5）。
// resource-pools（FR-CMP-001~004，FR-SYS-002/010）、login-users（FR-SEC-002/003/008，
// 决策 #25）、system/status（§6.2）。

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/metrics"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/schema"
)

// ---------- resource-pools（FR-CMP-001~003） ----------

// handleGetResourcePools GET /api/v1/resource-pools：配置 + 运行态合并视图
// （FR-CMP-001~004、FR-SYS-010，决策 #39）：
//
//	hugepages[].{page_size,count} 配置；{total,allocated,free} 运行态（total=count）
//	cpu.{isolated_cores,numa} 配置；{vpp_reserved,allocated[{vnf,cores}],free} 运行态
//
// 运行态由 committed 配置经账本确定性重算；未配置资源池时返回空视图而非报错。
func (s *Server) handleGetResourcePools(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resourcePoolView(cfg))
}

// resourcePoolView 构造资源池「配置 + 运行态」视图（纯函数，便于直接单测）。
func resourcePoolView(cfg model.Config) map[string]any {
	ledger := model.NewPoolLedger(cfg)
	_ = ledger.Allocate(cfg) // 分配演练填充运行态用量（缺口不影响只读展示）

	hugepages := []map[string]any{}
	if cfg.ResourcePools != nil {
		for _, hp := range cfg.ResourcePools.Hugepages {
			allocated, free := 0, hp.Count
			if u := ledger.Hugepages[hp.PageSize]; u != nil {
				allocated, free = u.Allocated, u.Free
			}
			hugepages = append(hugepages, map[string]any{
				"page_size": hp.PageSize,
				"count":     hp.Count,
				"total":     hp.Count,
				"allocated": allocated,
				"free":      free,
			})
		}
		// 排序保证稳定输出
		sort.Slice(hugepages, func(i, j int) bool {
			return fmt.Sprint(hugepages[i]["page_size"]) < fmt.Sprint(hugepages[j]["page_size"])
		})
	}

	allocated := make([]map[string]any, 0, len(ledger.CPU.VMCores))
	for name, cores := range ledger.CPU.VMCores {
		allocated = append(allocated, map[string]any{"vnf": name, "cores": cores})
	}
	sort.Slice(allocated, func(i, j int) bool {
		return fmt.Sprint(allocated[i]["vnf"]) < fmt.Sprint(allocated[j]["vnf"])
	})
	cpu := map[string]any{
		"isolated_cores": emptyIfNil(ledger.CPU.Isolated),
		"vpp_reserved":   emptyIfNil(ledger.CPU.VppReserved),
		"free":           emptyIfNil(ledger.CPU.Free),
		"allocated":      allocated,
	}
	if cfg.ResourcePools != nil && cfg.ResourcePools.CPU != nil {
		numa := make([]map[string]any, 0, len(cfg.ResourcePools.CPU.Numa))
		for _, n := range cfg.ResourcePools.CPU.Numa {
			numa = append(numa, map[string]any{"node": n.Node, "cores": n.Cores})
		}
		cpu["numa"] = numa
	}
	return map[string]any{"hugepages": hugepages, "cpu": cpu}
}

// emptyIfNil 保证 JSON 输出为 [] 而非 null（契约数组字段）。
func emptyIfNil(xs []int) []int {
	if xs == nil {
		return []int{}
	}
	return xs
}

// handlePutResourcePools PUT /api/v1/resource-pools：修改资源池
// （写 candidate；Auto-Commit 时 commit 返回 reboot 警告，FR-SYS-002；
// 缩减校验由账本在 commit 阶段执行，FR-CMP-004）。
func (s *Server) handlePutResourcePools(w http.ResponseWriter, r *http.Request) {
	var in model.ResourcePool
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	s.mutateCandidate(w, r, http.StatusOK, func(cfg *model.Config) error {
		rp := in
		cfg.ResourcePools = &rp
		return nil
	})
}

// ---------- login-users（FR-SEC-002/003/008，决策 #25） ----------

// loginUsersOf 返回 committed 的 system.login（不存在则返回空结构）。
func loginUsersOf(cfg model.Config) model.SystemLogin {
	if cfg.System != nil && cfg.System.Login != nil {
		return *cfg.System.Login
	}
	return model.SystemLogin{}
}

// handleGetLoginUsers GET /api/v1/system/login-users：用户与 class 列表
// （口令哈希永不回显，FR-SEC-007）。
func (s *Server) handleGetLoginUsers(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	l := loginUsersOf(cfg)
	users := make([]map[string]any, 0, len(l.Users))
	for _, u := range l.Users {
		users = append(users, map[string]any{"name": u.Name, "class": effectiveClassOf(u)})
	}
	classes := make([]map[string]any, 0, len(l.Classes))
	for _, c := range l.Classes {
		classes = append(classes, map[string]any{"name": c.Name, "allow": c.Allow, "deny": c.Deny})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users, "classes": classes})
}

func effectiveClassOf(u model.LoginUserConfig) string {
	if u.Class != "" {
		return u.Class
	}
	return aaa.ClassReadOnly
}

// mutateLoginUsers 在 candidate 上修改 system.login 并直提。
//
// 用户/口令变更**必须**生效（FR-SEC-008），故本族每个入口都是「取锁 → 写候选 → 立即提交」
// 的一次性事务：收尾时按 endOneShot 的判据交还会话锁（决策 #151）——此前提交后仍持有
// 全局编辑锁，其它会话随后的配置写被 409 挡住（round76 三次复现）。
func (s *Server) mutateLoginUsers(w http.ResponseWriter, r *http.Request, status int, mutate func(*model.SystemLogin) error) {
	sess := sessionFromIdentity(r)
	if err := s.engine.Edit(sess); err != nil {
		mapEngineError(w, err)
		return
	}
	defer endOneShot(s.engine, sess, s.log)
	cfg, _, err := s.engine.Candidate()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	if cfg.System == nil {
		cfg.System = &model.SystemConfig{}
	}
	if cfg.System.Login == nil {
		cfg.System.Login = &model.SystemLogin{}
	}
	if err := mutate(cfg.System.Login); err != nil {
		var ce conflictError
		if errors.As(err, &ce) {
			writeError(w, http.StatusConflict, "CONFLICT", err.Error(), nil)
			return
		}
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	if err := s.engine.UpdateCandidate(sess, cfg); err != nil {
		mapEngineError(w, err)
		return
	}
	// 用户/口令变更强制直提生效（FR-SEC-008，入审计）
	opts := CommitOptsFrom(r)
	opts.Message = "login-users 变更"
	res, err := s.engine.Commit(r.Context(), sess, opts)
	if err != nil {
		mapEngineError(w, err)
		return
	}
	w.Header().Set("X-NFVIS-Committed", "true")
	writeJSON(w, status, commitResponseOf(res))
}

// handlePostLoginUser POST /api/v1/system/login-users：创建用户或 class。
func (s *Server) handlePostLoginUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name     string   `json:"name"`
		Kind     string   `json:"kind"` // user | class
		Password string   `json:"password"`
		Class    string   `json:"class"`
		Allow    []string `json:"allow"`
		Deny     []string `json:"deny"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	if in.Name == "" {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", "name 必填", nil)
		return
	}
	s.mutateLoginUsers(w, r, http.StatusCreated, func(l *model.SystemLogin) error {
		if in.Kind == "class" {
			for _, c := range l.Classes {
				if c.Name == in.Name {
					return conflictError("class " + in.Name + " 已存在")
				}
			}
			l.Classes = append(l.Classes, model.ClassDef{Name: in.Name, Allow: in.Allow, Deny: in.Deny})
			return nil
		}
		for _, u := range l.Users {
			if u.Name == in.Name {
				return conflictError("用户 " + in.Name + " 已存在")
			}
		}
		if in.Password == "" {
			return errors.New("新用户必须设置初始口令")
		}
		if bad := aaa.CheckPasswordPolicy(in.Password, l.PasswordPolicy); len(bad) > 0 {
			return errors.New("口令不满足策略：" + strings.Join(bad, "；"))
		}
		hash, err := aaa.HashPassword(in.Password)
		if err != nil {
			return err
		}
		l.Users = append(l.Users, model.LoginUserConfig{Name: in.Name, PasswordHash: hash, Class: in.Class})
		return nil
	})
}

// handlePutLoginUser PUT /api/v1/system/login-users/{name}：改 class / 重置口令。
func (s *Server) handlePutLoginUser(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var in struct {
		Class    string `json:"class"`
		Password string `json:"password"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	s.mutateLoginUsers(w, r, http.StatusOK, func(l *model.SystemLogin) error {
		for i := range l.Users {
			if l.Users[i].Name != name {
				continue
			}
			if in.Class != "" {
				l.Users[i].Class = in.Class
			}
			if in.Password != "" {
				if bad := aaa.CheckPasswordPolicy(in.Password, l.PasswordPolicy); len(bad) > 0 {
					return errors.New("口令不满足策略：" + strings.Join(bad, "；"))
				}
				hash, err := aaa.HashPassword(in.Password)
				if err != nil {
					return err
				}
				l.Users[i].PasswordHash = hash
			}
			return nil
		}
		return conflictError("用户 " + name + " 不存在")
	})
}

// handleDeleteLoginUser DELETE /api/v1/system/login-users/{name}：
// 不能删除自己，也不能删除最后一个 super-user（409）。
func (s *Server) handleDeleteLoginUser(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	info, _ := Identity(r)
	s.mutateLoginUsers(w, r, http.StatusOK, func(l *model.SystemLogin) error {
		if name == info.User {
			return conflictError("不能删除当前登录用户")
		}
		sups := 0
		for _, u := range l.Users {
			if effectiveClassOf(u) == aaa.ClassSuperUser {
				sups++
			}
		}
		idx := -1
		for i := range l.Users {
			if l.Users[i].Name == name {
				idx = i
			}
		}
		if idx < 0 {
			return conflictError("用户 " + name + " 不存在")
		}
		if effectiveClassOf(l.Users[idx]) == aaa.ClassSuperUser && sups <= 1 {
			return conflictError("不能删除最后一个 super-user 用户")
		}
		l.Users = append(l.Users[:idx], l.Users[idx+1:]...)
		return nil
	})
}

// dispatchLoginUsersPost POST /system/login-users/{tail...}：
// 分发 {name}:change-password（冒号后缀无法表达为 ServeMux 通配符）。
func (s *Server) dispatchLoginUsersPost(w http.ResponseWriter, r *http.Request) {
	tail := r.PathValue("tail")
	if name, ok := strings.CutSuffix(tail, ":change-password"); ok && name != "" {
		req := r.WithContext(r.Context())
		s.handleChangePassword(w, req, name)
		return
	}
	writeError(w, http.StatusNotFound, "NOT_FOUND", "未知操作: "+tail, nil)
}

// handleChangePassword POST /api/v1/system/login-users/{name}:change-password：
// 口令自助修改（FR-SEC-008，验证旧口令 + 策略；入审计）。
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request, name string) {
	info, _ := Identity(r)
	if info.User != name {
		// 仅本人或 super-user（FR-SEC-008）
		if !s.aaa.Authorize(info.Class, schema.ClassSuperUser) {
			writeError(w, http.StatusForbidden, "FORBIDDEN", "只能修改本人口令", nil)
			return
		}
	}
	var in struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := decodeBody(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	if err := s.aaa.ChangePassword(name, in.OldPassword, in.NewPassword); err != nil {
		writeError(w, http.StatusBadRequest, "VALIDATION_FAILED", err.Error(), nil)
		return
	}
	hash, err := aaa.HashPassword(in.NewPassword)
	if err != nil {
		mapEngineError(w, err)
		return
	}
	s.mutateLoginUsers(w, r, http.StatusOK, func(l *model.SystemLogin) error {
		for i := range l.Users {
			if l.Users[i].Name == name {
				l.Users[i].PasswordHash = hash
				return nil
			}
		}
		return conflictError("用户 " + name + " 不存在")
	})
}

// ---------- system/status（§6.2，运行态字段 M3 补齐） ----------

var startTime = time.Now()

// handleGetSystemStatus GET /api/v1/system/status。
func (s *Server) handleGetSystemStatus(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.engine.Committed()
	if err != nil {
		mapEngineError(w, err)
		return
	}
	hostname := ""
	if cfg.System != nil {
		hostname = cfg.System.Hostname
	}
	val := map[string]float64{}
	for _, smp := range metrics.HostMetrics() {
		if len(smp.Labels) == 0 {
			val[smp.Name] = smp.Value
		}
	}
	// uptime_seconds 取**主机**运行时长（/proc/uptime → nfvis_system_uptime_seconds，
	// 与 CLI `show system uptime` 同源）；非 Linux 取不到时退回守护进程运行时长
	// （字段不缺席，但语义以契约描述为准）。此前这里是 time.Since(startTime)，
	// 页面"运行时长"显示的是守护进程活了多久而非机器活了多久（round39 可视验收发现）。
	uptimeSec := int(time.Since(startTime).Seconds())
	if up, ok := val["nfvis_system_uptime_seconds"]; ok {
		uptimeSec = int(up)
	}
	out := map[string]any{
		"hostname":       hostname,
		"uptime_seconds": uptimeSec,
		"config_ready":   true,
	}
	// 决策 #116：契约声明了 cpu/memory/hugepages/storage，此前只回上面三项（响应形状与契约
	// 不符，照契约开发的客户端一律取空）。这里补齐，且**数据源与 CLI 同源**：
	//   cpu/memory/storage ← internal/metrics.HostMetrics()（与 show system cpu|memory|storage 同一来源）
	//   hugepages          ← resourcePoolView(cfg)（与 GET /resource-pools 同一来源）
	// 宿主指标是 Linux 采集（非 Linux 为空实现）：取不到就**不给该子对象**，不编造零值。
	pools := resourcePoolView(cfg)
	cpu := map[string]any{}
	if n, ok := val["nfvis_system_cpu_online_count"]; ok {
		cpu["total"] = int(n)
	}
	if u, ok := val["nfvis_system_cpu_utilization_ratio"]; ok {
		cpu["usage_percent"] = math.Round(u*1000) / 10
	}
	if pc, ok := pools["cpu"].(map[string]any); ok {
		cpu["isolated"] = pc["isolated_cores"]
	}
	if len(cpu) > 0 {
		out["cpu"] = cpu
	}
	if total, ok := val["nfvis_system_memory_total_bytes"]; ok {
		mem := map[string]any{"total_mb": int(total / (1 << 20))}
		if avail, ok2 := val["nfvis_system_memory_available_bytes"]; ok2 {
			mem["used_mb"] = int((total - avail) / (1 << 20))
		}
		out["memory"] = mem
	}
	if total, ok := val["nfvis_system_disk_total_bytes"]; ok {
		st := map[string]any{"total_bytes": int64(total)}
		if free, ok2 := val["nfvis_system_disk_free_bytes"]; ok2 {
			st["free_bytes"] = int64(free)
		}
		if ratio, ok2 := val["nfvis_system_disk_used_ratio"]; ok2 {
			st["used_ratio"] = math.Round(ratio*10000) / 10000
		}
		out["storage"] = st
	}
	out["hugepages"] = pools["hugepages"]
	writeJSON(w, http.StatusOK, out)
}
