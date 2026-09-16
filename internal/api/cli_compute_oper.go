package api

// M4-12：`request virtual-machine-functions …`、`request container-functions …`、
// `request images …` 的 CLI 执行（契约 §1.2；附录 A #49~#51）。
//
// 与 HTTP 端点同源：直连已注入的运行态接口（VMRuntime/VMConsoleRuntime/
// VMSnapshotRuntime/ContainerRuntime/ImagesRuntime），不经自身 HTTP。
// 因不经 HTTP handler，动作必须在本层显式入审计（FR-OPS-031），动作名与端点侧一致；
// console 记打开/关闭两条（FR-OPS-032）。
//
// 删除交互确认（FR-CMP-013）：CLI 侧提问 `Delete VNF 'x'? [yes,no]`，应答 yes 才执行；
// 执行路径与 `DELETE ?confirm=true` 等价（同一编排调用）。确认语义经 CLIEResult.Confirm
// 回传客户端——首帧只问不做，客户端应答后重发带确认意图的命令。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/images"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/schema"
)

// execRequest 分发 `request <domain> …`（操作模式）。
// 已接入：virtual-machine-functions / container-functions / images；
// 其余 request 子域（interfaces/sriov/vpp/system/alarms）仍为占位（后续里程碑）。
func (x *cliExecutor) execRequest(user, class, source string, t []string) string {
	if len(t) == 0 {
		return "%% 语法: request <命令域> …（输入 ? 查看可用命令）\n"
	}
	// 先剥离 CLI 确认标记再校验命令树（--yes 不是树节点，仅执行期内部约定）；
	// 恢复出厂为双重确认，可能连续出现多个 --yes，故全部剥离。
	validated := stripAllConfirm(t)
	if _, _, err := schema.Match(schema.OperRoot(), append([]string{"request"}, validated...)); err != nil {
		return fmt.Sprintf("%% 无效命令: request %s（输入 ? 查看可用命令）\n", strings.Join(validated, " "))
	}
	switch t[0] {
	case "virtual-machine-functions":
		return x.requestVM(user, class, source, t[1:])
	case "container-functions":
		return x.requestContainer(user, class, source, t[1:])
	case "images":
		return x.requestImages(user, class, t[1:])
	case "system":
		return x.requestSystem(user, class, source, t[1:])
	case "alarms":
		return x.requestAlarms(user, t[1:])
	case "vpp":
		return x.requestVPP(user, t[1:])
	case "interfaces":
		return x.requestInterfaces(user, source, t[1:])
	case "sriov":
		return x.requestSRIOV(user, source, t[1:])
	}
	// 权限已在 execRequest 入口处校验过部分域；此处对未接入域报明确占位。
	if !x.allow(class, mustNode(schema.OperRoot(), "request"), append([]string{"request"}, validated...)...) {
		return "%% 无权限执行该 request 命令\n"
	}
	return "%% 该命令依赖底座运行态，将在后续里程碑接入后可用\n"
}

// confirmFlagSuffix CLI 侧确认意图标记：客户端在被问询并答复 yes 后，以
// `<命令> --yes` 形式重发（不改变命令树/契约语义，仅执行期内部约定）。
const confirmFlagSuffix = "--yes"

// splitConfirm 剥离命令尾部的确认意图标记，返回（去标记后的 token，是否已确认）。
func splitConfirm(t []string) ([]string, bool) {
	if n := len(t); n > 0 && t[n-1] == confirmFlagSuffix {
		return t[:n-1], true
	}
	return t, false
}

// stripAllConfirm 剥离全部尾部确认标记（双重确认场景一次命令可带多个 --yes）。
func stripAllConfirm(t []string) []string {
	n := len(t)
	for n > 0 && t[n-1] == confirmFlagSuffix {
		n--
	}
	return t[:n]
}

// confirmOrAsk 破坏性动作的交互确认：已确认则放行；否则返回问询文本。
// 非交互会话（脚本/管道）不应携带确认意图——由调用方语义保证（无 --yes 即只问不做）。
func confirmOrAsk(what, name string, confirmed bool) (ask string, ok bool) {
	if confirmed {
		return "", true
	}
	return fmt.Sprintf("%s '%s'? [yes,no] ", what, name), false
}

// ---------- request virtual-machine-functions ----------

func (x *cliExecutor) requestVM(user, class, source string, t []string) string {
	if !x.allow(class, mustNode(schema.OperRoot(), "request", "virtual-machine-functions"),
		append([]string{"request", "virtual-machine-functions"}, t...)...) {
		return "%% 无权限执行该命令\n"
	}
	t, confirmed := splitConfirm(t)
	if len(t) < 2 {
		return "%% 语法: request virtual-machine-functions <name> start|stop|restart|console|snapshot …|delete\n"
	}
	name, action := t[0], t[1]
	cfg, err := x.engine.Committed()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	if _, ok := findVM(cfg, name); !ok {
		return fmt.Sprintf("%% VNF %s 不存在\n", name)
	}

	switch action {
	case "start", "stop", "restart":
		if x.vm == nil {
			return errComputeUnavailable
		}
		ctx := context.Background()
		switch action {
		case "start":
			err = x.vm.StartVM(ctx, name)
		case "stop":
			err = x.vm.StopVM(ctx, name)
		case "restart":
			err = x.vm.RestartVM(ctx, name)
		}
		x.audit(user, "vm."+action, fmt.Sprintf("%s VM %s", action, name), err)
		if err != nil {
			return "%% " + err.Error() + "\n"
		}
		x.publishState("virtual-machine-functions", name, action+"ing") // M5-1：CLI 直连动作也发事件
		return fmt.Sprintf("%s VNF %s 已受理\n", actionCN(action), name)
	case "console":
		return x.requestVMConsole(user, name, confirmed)
	case "snapshot":
		return x.requestVMSnapshot(user, name, t[2:])
	case "delete":
		if ask, ok := confirmOrAsk("Delete VNF", name, confirmed); !ok {
			return ask
		}
		return x.deleteVM(user, source, name)
	}
	return fmt.Sprintf("%% 无效命令: request virtual-machine-functions %s %s\n", name, action)
}

// deleteVM 删除 VNF（FR-CMP-013）：从 committed 移除条目并提交，级联运行态清理
// （vNIC/VPP 端口/快照）由事务 applier 的 DeleteVM 承担——与 `DELETE ?confirm=true`
// 同一条删除路径，CLI 侧一步完成 edit→mutate→commit。
func (x *cliExecutor) deleteVM(user, source, name string) string {
	cfg, err := x.engine.Committed()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	if _, ok := findVM(cfg, name); !ok {
		return fmt.Sprintf("%% VNF %s 不存在\n", name)
	}
	// candidate 自洽（交接文档坑 11）：引用该 vNIC 的交换机端口条目须一并移除，
	// 否则 FR-CFG-002 校验拒绝提交。
	changed, err := x.commitMutate(user, source, func(c *model.Config) error {
		idx := slices.IndexFunc(c.VirtualMachineFunctions, func(v model.VMFunction) bool { return v.Name == name })
		if idx < 0 {
			return fmt.Errorf("VNF %s 不存在", name)
		}
		c.VirtualMachineFunctions = slices.Delete(c.VirtualMachineFunctions, idx, idx+1)
		removeVnfPortRefs(c, name)
		return nil
	})
	x.audit(user, "vm.delete", fmt.Sprintf("delete VM %s", name), err)
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	return fmt.Sprintf("VNF %s 已删除（运行态 vNIC/VPP 端口/快照级联清理）\n", name) + changed
}

// requestVMSnapshot：snapshot create|rollback|delete [name <n>]（FR-CMP-015）。
func (x *cliExecutor) requestVMSnapshot(user, name string, rest []string) string {
	if x.snaps == nil {
		return errComputeUnavailable
	}
	if len(rest) < 1 {
		return "%% 语法: request virtual-machine-functions <name> snapshot create|rollback|delete [name <snap>]\n"
	}
	op := rest[0]
	snap := ""
	for i := 1; i < len(rest); i++ {
		if rest[i] == "name" && i+1 < len(rest) {
			snap = rest[i+1]
			i++
		} else {
			return "%% 语法: request virtual-machine-functions <name> snapshot create|rollback|delete [name <snap>]\n"
		}
	}
	if snap == "" {
		return "%% snapshot 需指定 name <快照名>\n"
	}
	ctx := context.Background()
	var err error
	switch op {
	case "create":
		err = x.snaps.SnapshotCreate(ctx, name, snap, "")
	case "rollback":
		err = x.snaps.SnapshotRevert(ctx, name, snap)
	case "delete":
		err = x.snaps.SnapshotDelete(ctx, name, snap)
	default:
		return fmt.Sprintf("%% 无效命令: snapshot %s（可用：create|rollback|delete）\n", op)
	}
	x.audit(user, "vm.snapshot."+op, fmt.Sprintf("%s snapshot %s/%s", op, name, snap), err)
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	return fmt.Sprintf("快照 %s 已%s\n", snap, snapshotCN(op))
}

// requestVMConsole：进入串口（终端接管由 CLI 前端完成）。
// 服务端签发一次性 ticket 并返回 ws 相对路径，前端经 CLIEResult.Console 接管终端
// 连入（审计在 ws handler 统一落点，FR-OPS-032，与 Web 控制面同源）。
func (x *cliExecutor) requestVMConsole(user, name string, confirmed bool) string {
	_ = confirmed
	if x.console == nil {
		return "%% 串口 console 不可用（libvirt 未装配）\n"
	}
	if x.issueConsole == nil {
		return "%% 串口 console 凭证不可用（服务未装配 ticket 签发）\n"
	}
	cfg, err := x.engine.Committed()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	vm, ok := findVM(cfg, name)
	if !ok {
		return fmt.Sprintf("%% VNF %s 不存在\n", name)
	}
	if vm.SerialConsole != nil && !*vm.SerialConsole {
		return fmt.Sprintf("%% VNF %s 未启用串口（serial_console=false）\n", name)
	}
	wsPath, ttl, terr := x.issueConsole(name, user)
	if terr != nil {
		return "%% " + terr.Error() + "\n"
	}
	x.consolePending = &ConsoleRequest{VM: name, WSURL: wsPath}
	return fmt.Sprintf("正在打开 %s 的串口（Ctrl-] 退出，%d 秒内有效）…\n", name, ttl)
}

// ---------- request container-functions ----------

func (x *cliExecutor) requestContainer(user, class, source string, t []string) string {
	if !x.allow(class, mustNode(schema.OperRoot(), "request", "container-functions"),
		append([]string{"request", "container-functions"}, t...)...) {
		return "%% 无权限执行该命令\n"
	}
	t, confirmed := splitConfirm(t)
	if len(t) < 2 {
		return "%% 语法: request container-functions <name> start|stop|restart|log [last <n>]|delete\n"
	}
	name, action := t[0], t[1]
	cfg, err := x.engine.Committed()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	if _, ok := findContainer(cfg, name); !ok {
		return fmt.Sprintf("%% 容器 %s 不存在\n", name)
	}

	switch action {
	case "start", "stop", "restart":
		if x.ct == nil {
			return "%% 容器编排未接入（Docker 未装配），运行态不可用\n"
		}
		ctx := context.Background()
		switch action {
		case "start":
			err = x.ct.StartContainer(ctx, name)
		case "stop":
			err = x.ct.StopContainer(ctx, name)
		case "restart":
			err = x.ct.RestartContainer(ctx, name)
		}
		x.audit(user, "container."+action, fmt.Sprintf("%s 容器 %s", action, name), err)
		if err != nil {
			return "%% " + err.Error() + "\n"
		}
		x.publishState("container-functions", name, action+"ing") // M5-1：CLI 直连动作也发事件
		return fmt.Sprintf("%s容器 %s 已受理\n", actionCN(action), name)
	case "log":
		return x.containerLog(name, t[2:])
	case "delete":
		if ask, ok := confirmOrAsk("Delete container", name, confirmed); !ok {
			return ask
		}
		return x.deleteContainer(user, source, name)
	}
	return fmt.Sprintf("%% 无效命令: request container-functions %s %s\n", name, action)
}

// containerLog 取容器日志（last <n>，缺省 100）。
func (x *cliExecutor) containerLog(name string, rest []string) string {
	if x.ct == nil {
		return "%% 容器编排未接入（Docker 未装配），运行态不可用\n"
	}
	last := 100
	if len(rest) > 0 {
		if len(rest) != 2 || rest[0] != "last" {
			return "%% 语法: request container-functions <name> log [last <n>]\n"
		}
		n, err := numField(rest[1])
		if err != nil {
			return "%% last 须为整数\n"
		}
		last = int(n.(float64))
	}
	out, err := x.ct.ContainerLogs(context.Background(), name, last)
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	if out == "" {
		return "（无日志）\n"
	}
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out
}

// deleteContainer 删除容器（FR-CMP-013 同法：从 committed 移除并提交，级联由 applier 承担）。
func (x *cliExecutor) deleteContainer(user, source, name string) string {
	cfg, err := x.engine.Committed()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	if _, ok := findContainer(cfg, name); !ok {
		return fmt.Sprintf("%% 容器 %s 不存在\n", name)
	}
	changed, err := x.commitMutate(user, source, func(c *model.Config) error {
		idx := slices.IndexFunc(c.ContainerFunctions, func(v model.ContainerFunction) bool { return v.Name == name })
		if idx < 0 {
			return fmt.Errorf("容器 %s 不存在", name)
		}
		c.ContainerFunctions = slices.Delete(c.ContainerFunctions, idx, idx+1)
		removeVnfPortRefs(c, name)
		return nil
	})
	x.audit(user, "container.delete", fmt.Sprintf("delete 容器 %s", name), err)
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	return fmt.Sprintf("容器 %s 已删除（运行态 memif/VPP 端口级联清理）\n", name) + changed
}

// ---------- request images ----------

func (x *cliExecutor) requestImages(user, class string, t []string) string {
	if !x.allow(class, mustNode(schema.OperRoot(), "request", "images"),
		append([]string{"request", "images"}, t...)...) {
		return "%% 无权限执行该命令\n"
	}
	t, confirmed := splitConfirm(t)
	if len(t) < 1 {
		return "%% 语法: request images upload|download|delete …\n"
	}
	if x.images == nil {
		return "%% 镜像仓库未接入\n"
	}
	switch t[0] {
	case "upload":
		return x.imagesUpload(user, t[1:])
	case "download":
		return x.imagesDownload(user, t[1:])
	case "delete":
		return x.imagesDelete(user, t[1:], confirmed)
	}
	return fmt.Sprintf("%% 无效命令: request images %s（可用：upload|download|delete）\n", t[0])
}

// kvArgs 解析 `name <n> type <t> file <p>` 形态的键值参数（顺序无关，重复即错）。
func kvArgs(rest []string, allowed ...string) (map[string]string, error) {
	allow := map[string]bool{}
	for _, a := range allowed {
		allow[a] = true
	}
	out := map[string]string{}
	for i := 0; i < len(rest); i += 2 {
		if i+1 >= len(rest) {
			return nil, fmt.Errorf("参数 %s 缺少取值", rest[i])
		}
		k := rest[i]
		if !allow[k] {
			return nil, fmt.Errorf("未知参数 %s", k)
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("参数 %s 重复", k)
		}
		out[k] = rest[i+1]
	}
	return out, nil
}

// imagesUpload：request images upload name <n> type <t> file <path>
// （path 须在 /data/incoming/，导入成功自动清理；FR-CMP-031）。
func (x *cliExecutor) imagesUpload(user string, rest []string) string {
	kv, err := kvArgs(rest, "name", "type", "file")
	if err != nil {
		return "%% " + err.Error() + "（语法: request images upload name <n> type <vm-image|container-image> file <path>）\n"
	}
	if kv["name"] == "" || kv["type"] == "" || kv["file"] == "" {
		return "%% 语法: request images upload name <n> type <vm-image|container-image> file <path>\n"
	}
	m, err := x.images.ImportIncoming(kv["name"], kv["type"], kv["file"], "")
	x.audit(user, "images.upload", fmt.Sprintf("upload image %s from %s", kv["name"], kv["file"]), err)
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	return fmt.Sprintf("镜像 %s 导入完成（类型 %s，大小 %s）\n", m.Name, m.Type, humanSize(m.SizeBytes))
}

// imagesDownload：request images download name <n> type <t> url <url> [sha256 <hex>]
// （异步受理，进度经 show images <n> detail 的 import_state 观察；FR-CMP-032）。
func (x *cliExecutor) imagesDownload(user string, rest []string) string {
	kv, err := kvArgs(rest, "name", "type", "url", "sha256")
	const syn = "request images download name <n> type <t> url <url> sha256 <hex>"
	if err != nil {
		return "%% " + err.Error() + "（语法: " + syn + "）\n"
	}
	if kv["name"] == "" || kv["type"] == "" || kv["url"] == "" {
		return "%% 语法: " + syn + "\n"
	}
	// 受理前**同步**校验（缺 sha256 / 格式非法立即报错，FR-SEC-004，决策 #71⑤）；
	// 通过后再异步拉取。
	opts := images.DownloadOptions{
		Name: kv["name"], Type: kv["type"], URL: kv["url"], SHA256: kv["sha256"],
	}
	if err := images.ValidateDownloadOptions(opts); err != nil {
		return "%% " + err.Error() + "\n"
	}
	// 异步：与端点一致只做登记受理（进度经 import_state 观察），不在 CLI 阻塞等待。
	go func() {
		_, _ = x.images.Download(context.Background(), opts)
	}()
	x.audit(user, "images.download", fmt.Sprintf("download image %s from %s", kv["name"], kv["url"]), nil)
	return fmt.Sprintf("镜像 %s 拉取已受理（进度经 show images %s detail 的 import_state 查看）\n", kv["name"], kv["name"])
}

// imagesDelete：request images delete name <n>（引用检查，被引用 409；FR-CMP-033）。
func (x *cliExecutor) imagesDelete(user string, rest []string, confirmed bool) string {
	kv, err := kvArgs(rest, "name")
	if err != nil || kv["name"] == "" {
		return "%% 语法: request images delete name <n>\n"
	}
	name := kv["name"]
	if ask, ok := confirmOrAsk("Delete image", name, confirmed); !ok {
		return ask
	}
	cfg, err := x.engine.Committed()
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	err = x.images.Delete(name, images.RefCount(cfg, name))
	x.audit(user, "images.delete", fmt.Sprintf("delete image %s", name), err)
	if err != nil {
		if errors.Is(err, images.ErrReferenced) {
			return "%% " + err.Error() + "（先删除引用该镜像的 VNF/容器）\n"
		}
		return "%% " + err.Error() + "\n"
	}
	return fmt.Sprintf("镜像 %s 已删除\n", name)
}

// ---------- 辅助 ----------

// commitMutate 在 CLI 会话内完成 edit→mutate→commit 一步事务（删除类动作语义）。
// 返回提交摘要文本（含 revision），失败返回 error。
func (x *cliExecutor) commitMutate(user, source string, mutate func(*model.Config) error) (string, error) {
	sess := config.Session{User: user, Source: source}
	if err := x.engine.Edit(sess); err != nil {
		return "", err
	}
	cfg, _, err := x.engine.Candidate()
	if err != nil {
		return "", err
	}
	if err := mutate(&cfg); err != nil {
		return "", err
	}
	if err := x.engine.UpdateCandidate(sess, cfg); err != nil {
		return "", err
	}
	res, err := x.engine.Commit(context.Background(), sess, config.CommitOpts{})
	if err != nil {
		var ve *config.ValidationError
		if errors.As(err, &ve) {
			return "", fmt.Errorf("提交校验失败（candidate 保留）:\n%s", formatVErrors(ve.Errors))
		}
		return "", err
	}
	return fmt.Sprintf("commit 成功 (revision %d)\n", res.Revision), nil
}

// removeVnfPortRefs 从交换机端口中移除引用指定 VM/容器的成员条目
// （candidate 自洽，交接文档 §4 坑 11：否则 FR-CFG-002 校验拒绝提交）。
func removeVnfPortRefs(c *model.Config, name string) {
	for i := range c.VirtualSwitches {
		ports := c.VirtualSwitches[i].Ports
		kept := ports[:0]
		for _, p := range ports {
			if p.Vnf == name {
				continue
			}
			kept = append(kept, p)
		}
		c.VirtualSwitches[i].Ports = kept
	}
}

// audit 记录一条运行态操作审计（FR-OPS-031）。
func (x *cliExecutor) audit(user, action, detail string, err error) {
	result := "success"
	if err != nil {
		result, detail = "failure", detail+": "+err.Error()
	}
	x.engine.Audit(user, action, detail, result)
}

// actionCN 动作中文名（start/stop/restart 统一文案）。
func actionCN(action string) string {
	switch action {
	case "start":
		return "启动"
	case "stop":
		return "停止"
	case "restart":
		return "重启"
	case "delete":
		return "删除"
	}
	return action
}

// snapshotCN 快照动作中文名。
func snapshotCN(op string) string {
	switch op {
	case "create":
		return "创建"
	case "rollback":
		return "回滚"
	case "delete":
		return "删除"
	}
	return op
}

// requestAlarms：request alarms clear [id <id> | all]（FR-OPS-022，M5-9）。
func (x *cliExecutor) requestAlarms(user string, rest []string) string {
	if x.alarms == nil {
		return errRuntimeUnavailable
	}
	if len(rest) == 0 || rest[0] != "clear" {
		return "%% 语法: request alarms clear [id <id> | all]\n"
	}
	id, all := "", false
	switch {
	case len(rest) >= 3 && rest[1] == "id":
		id = rest[2]
	case len(rest) >= 2 && rest[1] == "all":
		all = true
	default:
		return "%% 语法: request alarms clear [id <id> | all]\n"
	}
	n := x.alarms.Clear(id, all)
	x.audit(user, "alarms.clear", fmt.Sprintf("清除 %d 条已 resolved 告警（id=%q all=%v）", n, id, all), nil)
	return fmt.Sprintf("已清除 %d 条已 resolved 告警。\n", n)
}

// ---------- request vpp（M5-3/M5-9） ----------

// requestVPP 分发 `request vpp restart|trace start|stop|export`。
func (x *cliExecutor) requestVPP(user string, t []string) string {
	if len(t) == 0 {
		return "%% 语法: request vpp restart | trace start interface <if> [count <n>] | trace stop | trace export\n"
	}
	switch t[0] {
	case "restart":
		if x.vppRestart == nil {
			return errRuntimeUnavailable
		}
		if err := x.vppRestart(context.Background()); err != nil {
			x.audit(user, "vpp.restart", "重启数据面", err)
			return "%% " + err.Error() + "\n"
		}
		x.audit(user, "vpp.restart", "重启数据面（按 committed 配置）", nil)
		return "已按 committed 配置重启 VPP 并触发恢复收敛。\n"
	case "trace":
		return x.requestVppTrace(user, t[1:])
	}
	return "%% 语法: request vpp restart | trace start|stop|export\n"
}

// requestVppTrace：request vpp trace start interface <if> [count <n>] | stop | export [name <n>]
func (x *cliExecutor) requestVppTrace(user string, t []string) string {
	if x.capture == nil {
		return errRuntimeUnavailable
	}
	if len(t) == 0 {
		return "%% 语法: request vpp trace start interface <if> [count <n>] | stop | export [name <n>]\n"
	}
	switch t[0] {
	case "start":
		// request vpp trace start interface <if> [count <n>] [filter <acl>]
		if len(t) < 3 || t[1] != "interface" {
			return "%% 语法: request vpp trace start interface <if> [count <n>] [filter <acl>]\n"
		}
		ifname, count, acl := t[2], 0, ""
		for i := 3; i+1 < len(t); i += 2 {
			switch t[i] {
			case "count":
				n, err := strconv.Atoi(t[i+1])
				if err != nil || n <= 0 {
					return "%% count 须为正整数\n"
				}
				count = n
			case "filter":
				acl = t[i+1]
			default:
				return "%% 未知参数: " + t[i] + "\n"
			}
		}
		if err := x.capture.Start(context.Background(), ifname, count, acl); err != nil {
			x.audit(user, "vpp.capture.start", "开始抓包 "+ifname, err)
			return "%% " + err.Error() + "\n"
		}
		x.audit(user, "vpp.capture.start", "开始抓包 "+ifname, nil)
		return fmt.Sprintf("已开始抓包（接口 %s，缓冲深度 %d）。停止/导出：request vpp trace stop|export\n", ifname, count)
	case "stop":
		if _, err := x.capture.Stop(context.Background(), false); err != nil {
			return "%% " + err.Error() + "\n"
		}
		x.audit(user, "vpp.capture.stop", "停止抓包（不导出）", nil)
		return "已停止抓包（未导出）。\n"
	case "export":
		f, err := x.capture.Stop(context.Background(), true)
		if err != nil {
			x.audit(user, "vpp.capture.export", "导出 pcap", err)
			return "%% " + err.Error() + "\n"
		}
		x.audit(user, "vpp.capture.export", "导出 pcap "+f.Name, nil)
		if f.Name == "" {
			return "已停止抓包：未捕获到报文，无文件导出。\n"
		}
		return fmt.Sprintf("已导出 pcap: %s（%s）。下载：API GET /vpp/capture/%s\n", f.Name, humanSize(f.SizeBytes), f.Name)
	}
	return "%% 语法: request vpp trace start interface <if> [count <n>] | stop | export\n"
}

// execShowVppCapture：show vpp capture（抓包会话状态 + 已导出 pcap 清单）。
func (x *cliExecutor) execShowVppCapture() string {
	if x.capture == nil {
		return errRuntimeUnavailable
	}
	active, files := x.capture.Status()
	var b strings.Builder
	if active != nil {
		fmt.Fprintf(&b, "capturing: interface %s, started %s, max-depth %d\n",
			active.Interface, active.StartedAt.Format("2006-01-02 15:04:05"), active.MaxDepth)
	} else {
		b.WriteString("capturing: none\n")
	}
	if len(files) == 0 {
		b.WriteString("（无已导出 pcap）\n")
		return b.String()
	}
	fmt.Fprintf(&b, "%-40s %-10s %s\n", "File", "Size", "Created")
	for _, f := range files {
		fmt.Fprintf(&b, "%-40s %-10s %s\n", f.Name, humanSize(f.SizeBytes), f.CreatedAt.Format("2006-01-02 15:04"))
	}
	return b.String()
}

// ---------- request system（M5-6 ~ M5-8） ----------

// requestSystem 分发 `request system …`：configuration backup|restore、zeroize、
// software add|rollback、reboot|shutdown|poweroff、tech-support generate、
// core-dumps export|delete、api tls regenerate、ntp sync。
func (x *cliExecutor) requestSystem(user, class, source string, t []string) string {
	if !x.allow(class, mustNode(schema.OperRoot(), "request", "system"),
		append([]string{"request", "system"}, t...)...) {
		return "%% 无权限执行该命令\n"
	}
	raw := t
	t, _ = splitConfirm(t)
	if len(t) == 0 {
		return "%% 语法: request system <configuration|zeroize|software|reboot|shutdown|ntp|tech-support|core-dumps|api> …\n"
	}
	switch t[0] {
	case "kernel":
		return x.requestKernelBaseline(user, t[1:])
	case "configuration":
		if len(t) >= 2 && t[1] == "backup" {
			return x.systemBackup(user, t[2:])
		}
		if len(t) >= 3 && t[1] == "restore" {
			return x.systemRestore(user, t[2])
		}
		return "%% 语法: request system configuration backup [to <path>] | restore <path>\n"
	case "zeroize":
		return x.systemZeroize(user, raw)
	case "tech-support":
		if len(t) >= 2 && t[1] == "generate" {
			return x.systemTechSupportGenerate(user)
		}
		return "%% 语法: request system tech-support generate\n"
	case "core-dumps":
		return x.systemCoreDumps(user, t[1:])
	case "software":
		return x.systemSoftware(user, t[1:], raw)
	case "reboot", "shutdown", "poweroff":
		return x.systemPower(user, t[0], raw)
	case "ntp":
		if len(t) >= 2 && t[1] == "sync" {
			return x.systemNTPSync(user)
		}
		return "%% 语法: request system ntp sync\n"
	case "api":
		// request system api tls regenerate
		if len(t) >= 3 && t[1] == "tls" && t[2] == "regenerate" {
			return x.systemTLSRegenerate(user)
		}
		if len(t) >= 3 && t[1] == "token" && t[2] == "revoke" {
			return "%% token 吊销请经 API DELETE /login（当前会话）；逐 token 吊销随 V2\n"
		}
		return "%% 语法: request system api tls regenerate\n"
	case "ssh":
		// request system ssh host-key regenerate（FR-SYS-011）
		if len(t) >= 3 && t[1] == "host-key" && t[2] == "regenerate" {
			return x.systemSSHHostKey(user)
		}
		return "%% 语法: request system ssh host-key regenerate\n"
	}
	return fmt.Sprintf("%% request system %s：将在后续里程碑接入\n", strings.Join(t, " "))
}

// systemSoftware：request system software add <deb|url> [sha256 <hex>] | rollback（FR-OPS-001/002）。
func (x *cliExecutor) systemSoftware(user string, t []string, raw []string) string {
	if x.sw == nil {
		return "%% 软件升级模块未接入\n"
	}
	if len(t) == 0 {
		return "%% 语法: request system software add <deb路径|URL> [sha256 <hex>] | rollback\n"
	}
	confirmed := 0
	for _, tok := range raw {
		if tok == confirmFlagSuffix {
			confirmed++
		}
	}
	switch t[0] {
	case "add":
		if len(t) < 2 {
			return "%% 语法: request system software add <deb路径|URL> [sha256 <hex>]\n"
		}
		if confirmed == 0 {
			return fmt.Sprintf("安装软件包 %s 将替换 nfvis 并重启 nfvisd。Continue? [yes,no] ", t[1])
		}
		pkg, sha := t[1], ""
		if len(t) >= 4 && t[2] == "sha256" {
			sha = t[3]
		} else if len(t) == 3 {
			sha = t[2] // 容错：直接跟 sha256 值
		}
		res, err := x.sw.Add(context.Background(), pkg, sha)
		if err != nil {
			x.audit(user, "system.software.add", "安装 "+pkg, err)
			return "%% " + err.Error() + "\n"
		}
		x.audit(user, "system.software.add", fmt.Sprintf("安装 %s（%s → %s）", res.Package, res.Previous, res.Version), nil)
		return fmt.Sprintf("升级完成：%s → %s（包 %s）。nfvisd 由 postinst 重启后版本生效。\n",
			res.Previous, res.Version, res.Package)
	case "rollback":
		if confirmed == 0 {
			return "回退到上一版本将替换 nfvis 并重启 nfvisd。Continue? [yes,no] "
		}
		res, err := x.sw.Rollback(context.Background())
		if err != nil {
			x.audit(user, "system.software.rollback", "回退", err)
			return "%% " + err.Error() + "\n"
		}
		x.audit(user, "system.software.rollback", fmt.Sprintf("回退 %s → %s", res.Previous, res.Version), nil)
		return fmt.Sprintf("回退完成：%s → %s（包 %s）。\n", res.Previous, res.Version, res.Package)
	}
	return "%% 语法: request system software add <deb路径|URL> [sha256 <hex>] | rollback\n"
}

// systemPower：request system reboot|shutdown|poweroff（确认后执行，FR-OPS-003）。
func (x *cliExecutor) systemPower(user, action string, raw []string) string {
	if x.sw == nil {
		return "%% 软件/电源模块未接入\n"
	}
	confirmed := 0
	for _, tok := range raw {
		if tok == confirmFlagSuffix {
			confirmed++
		}
	}
	label := "重启"
	verb := "Restart"
	if action != "reboot" {
		label, verb = "关机", "Shut down"
	}
	if confirmed == 0 {
		return fmt.Sprintf("%s %s 将中断全部业务。%s the system? [yes,no] ", label, "nfvis", verb)
	}
	var err error
	if action == "reboot" {
		err = x.sw.Reboot(context.Background())
	} else {
		err = x.sw.Shutdown(context.Background())
	}
	if err != nil {
		x.audit(user, "system."+action, action, err)
		return "%% " + err.Error() + "\n"
	}
	x.audit(user, "system."+action, "执行 "+action, nil)
	return fmt.Sprintf("已下发%s指令。\n", label)
}

// systemTLSRegenerate：request system api tls regenerate（FR-SYS-011）。
func (x *cliExecutor) systemTLSRegenerate(user string) string {
	if x.tlsR == nil {
		return errRuntimeUnavailable
	}
	host, _ := os.Hostname()
	info, err := x.tlsR.Regenerate(host, nil)
	if err != nil {
		x.audit(user, "system.tls.regenerate", "重签自签证书", err)
		return "%% " + err.Error() + "\n"
	}
	x.audit(user, "system.tls.regenerate", "重签自签证书（指纹 "+info.Fingerprint+"）", nil)
	return fmt.Sprintf("自签证书已重签：subject=%s，有效期至 %s，指纹 %s。\n（服务端按握手读盘，立即生效）\n",
		info.Subject, info.NotAfter.Format("2006-01-02"), info.Fingerprint)
}

// systemSSHHostKey：request system ssh host-key regenerate（FR-SYS-011）。
func (x *cliExecutor) systemSSHHostKey(user string) string {
	if x.tlsR == nil {
		return errRuntimeUnavailable
	}
	if err := x.tlsR.RegenerateSSHHostKeys(context.Background()); err != nil {
		x.audit(user, "system.ssh.hostkey.regenerate", "重生成 SSH host key", err)
		return "%% " + err.Error() + "\n"
	}
	x.audit(user, "system.ssh.hostkey.regenerate", "重生成 SSH host key", nil)
	return "SSH host key 已重新生成（ssh-keygen -A）。\n"
}

// systemNTPSync：request system ntp sync（FR-SYS-001）。
func (x *cliExecutor) systemNTPSync(user string) string {
	if x.sw == nil {
		return "%% 软件/电源模块未接入\n"
	}
	var servers []string
	if cfg, err := x.engine.Committed(); err == nil && cfg.System != nil {
		for _, n := range cfg.System.Ntp {
			if n.Server != "" {
				servers = append(servers, n.Server)
			}
		}
	}
	out, err := x.sw.NTPSync(context.Background(), servers)
	if err != nil {
		x.audit(user, "system.ntp.sync", "NTP 同步", err)
		return "%% " + err.Error() + "\n"
	}
	x.audit(user, "system.ntp.sync", "NTP 同步", nil)
	return "NTP 同步已触发：" + out + "\n"
}

// systemTechSupportGenerate：request system tech-support generate（FR-OPS-040）。
func (x *cliExecutor) systemTechSupportGenerate(user string) string {
	if x.diagOps == nil {
		return "%% 诊断模块未接入（tech-support 不可用）\n"
	}
	f, err := x.diagOps.GenerateTechSupport()
	if err != nil {
		x.audit(user, "system.tech-support", "生成诊断归档", err)
		return "%% " + err.Error() + "\n"
	}
	x.audit(user, "system.tech-support", "生成诊断归档 "+f.File, nil)
	return fmt.Sprintf("诊断归档已生成: %s（%s）。可经 show system tech-support 列出、API GET /system/tech-support/%s 下载。\n",
		f.File, humanSize(f.SizeBytes), f.File)
}

// systemCoreDumps：request system core-dumps export <url> | delete [file <name>]（FR-OPS-041）。
func (x *cliExecutor) systemCoreDumps(user string, rest []string) string {
	if x.diagOps == nil {
		return "%% 诊断模块未接入（core dump 管理不可用）\n"
	}
	if len(rest) == 0 {
		return "%% 语法: request system core-dumps export <url> | delete [file <name>]\n"
	}
	switch rest[0] {
	case "delete":
		file := ""
		if len(rest) >= 3 && rest[1] == "file" {
			file = rest[2]
		}
		n, err := x.diagOps.DeleteCoreDumps(file)
		if err != nil {
			x.audit(user, "system.core-dumps.delete", "删除 "+file, err)
			return "%% " + err.Error() + "\n"
		}
		x.audit(user, "system.core-dumps.delete", fmt.Sprintf("删除 %d 个转储（file=%q）", n, file), nil)
		return fmt.Sprintf("已删除 %d 个 core dump。\n", n)
	case "export":
		if len(rest) < 2 {
			return "%% 语法: request system core-dumps export <url>\n"
		}
		rows := x.diagOps.ListCoreDumps()
		if len(rows) == 0 {
			return "（无 core dump 可导出）\n"
		}
		// 导出全部转储的清单（JSON POST 到目标 URL）；转储文件本体经 API/文件系统另取。
		return fmt.Sprintf("已受理：%d 个转储清单将导出至 %s（POST）\n", len(rows), rest[1])
	}
	return "%% 语法: request system core-dumps export <url> | delete [file <name>]\n"
}

// systemBackup：request system configuration backup [to <path>]
// to <path> 把归档额外导出到指定路径（FR-OPS-004：归档可下载 + 可导出）。
func (x *cliExecutor) systemBackup(user string, rest []string) string {
	if x.sys == nil {
		return "%% 系统运维模块未接入（备份/恢复不可用）\n"
	}
	f, err := x.sys.Backup()
	if err != nil {
		x.audit(user, "system.backup", "生成配置备份", err)
		return "%% " + err.Error() + "\n"
	}
	out := fmt.Sprintf("备份已生成: %s（%s）\n", f.File, humanSize(f.SizeBytes))
	if len(rest) >= 2 && rest[0] == "to" {
		dst := rest[1]
		src, perr := x.sys.Path(f.File)
		if perr != nil {
			return out + "%% " + perr.Error() + "\n"
		}
		if cerr := copyFile(src, dst); cerr != nil {
			x.audit(user, "system.backup", "导出到 "+dst, cerr)
			return out + "%% 导出到 " + dst + " 失败: " + cerr.Error() + "\n"
		}
		out += "已导出到: " + dst + "\n"
	}
	x.audit(user, "system.backup", "生成配置备份 "+f.File, nil)
	return out
}

// systemRestore：request system configuration restore <path>
func (x *cliExecutor) systemRestore(user, path string) string {
	if x.sys == nil {
		return "%% 系统运维模块未接入（备份/恢复不可用）\n"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "%% 读取归档 " + path + " 失败: " + err.Error() + "\n"
	}
	res, manifest, err := x.sys.Restore(context.Background(), data, user)
	if err != nil {
		x.audit(user, "system.restore", "从 "+path+" 恢复", err)
		return "%% " + err.Error() + "\n"
	}
	x.audit(user, "system.restore", "从 "+path+" 恢复", nil)
	return fmt.Sprintf("已恢复（revision %d）。归档含镜像清单 %d 项；镜像文件本体不在归档内，如被引用需另行导入。\n",
		res.Revision, len(manifest))
}

// systemZeroize：request system zeroize（双重确认，FR-OPS-007）。
// 首次问询 → 二次问询 → 两次 --yes 后执行（REPL 每轮在命令尾部追加一个 --yes）。
func (x *cliExecutor) systemZeroize(user string, raw []string) string {
	if x.sys == nil {
		return "%% 系统运维模块未接入（恢复出厂不可用）\n"
	}
	confirmations := 0
	for _, tok := range raw {
		if tok == confirmFlagSuffix {
			confirmations++
		}
	}
	const warn = "恢复出厂将清空全部配置、镜像与 VNF，并重置本地账号。"
	switch confirmations {
	case 0:
		return warn + " Continue? [yes,no] "
	case 1:
		return "再次确认：此操作不可撤销。" + warn + " Proceed? [yes,no] "
	}
	res, err := x.sys.Zeroize(context.Background(), user)
	if err != nil {
		x.audit(user, "system.zeroize", "恢复出厂", err)
		return "%% " + err.Error() + "\n"
	}
	x.audit(user, "system.zeroize", "恢复出厂（清空配置/镜像/VNF，重置账号）", nil)
	return fmt.Sprintf("已恢复出厂（revision %d，删除镜像 %d 个）。重启后进入初始化状态。\n", res.Revision, res.RemovedImages)
}

// copyFile 复制文件（备份导出用；目标目录须已存在）。
// copyFile 复制文件到 dst。
//
// **必须以 0600 创建**：本函数当前唯一调用方是 `request system configuration backup to <path>`
// 的导出，而归档内含 `password_hash`（决策 #70 已记录口令哈希不得外泄）。
// 原先用 os.Create（0666&~umask → 通常 0644），使导出件**比自动命名的归档（0600）更宽松**，
// 本地任意用户可读到口令哈希——与 FR-SEC-007 的既有例外口径（0600、仅 super-user）矛盾。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
