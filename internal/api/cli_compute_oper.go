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
	"slices"
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
	// 先剥离 CLI 确认标记再校验命令树（--yes 不是树节点，仅执行期内部约定）。
	validated, _ := splitConfirm(t)
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
	if err != nil {
		return "%% " + err.Error() + "（语法: request images download name <n> type <t> url <url> [sha256 <hex>]）\n"
	}
	if kv["name"] == "" || kv["type"] == "" || kv["url"] == "" {
		return "%% 语法: request images download name <n> type <t> url <url> [sha256 <hex>]\n"
	}
	// 异步：与端点一致只做登记受理（进度经 import_state 观察），不在 CLI 阻塞等待。
	go func() {
		_, _ = x.images.Download(context.Background(), images.DownloadOptions{
			Name: kv["name"], Type: kv["type"], URL: kv["url"], SHA256: kv["sha256"],
		})
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
