//go:build linux

package netkernel

// 容器 vNIC 宿主端 veth 的**真实现**（[linux]）：
//   - 宿主端与容器端的建/删/入 bridge/置 up 走宿主 `ip`（经 Provider 的 Runner，与其它族同一
//     串行口径：p.ip/p.ipIdem/p.ipReq/p.ipBest 自带 p.mu）；
//   - 容器端移入容器网络命名空间用 `ip link set <容器端> netns <pid>`；
//   - 容器内的改名/置 up/可选 MAC 用 `nsenter -t <pid> -n ip link …`（进入容器 netns 执行）。
//
// 为什么用 nsenter 而不是 `ip -n <netns 名>`：容器 netns 没有产品自管的名字（Docker 的 netns
// 路径随容器生命周期变），只有**运行中进程的 pid** 是稳定抓手；nsenter 按 pid 进网，语义与
// `ip netns exec` 等价（同一 root 权限面）。
//
// 顺序约束（见 Attach）：改名与设 MAC 都要求设备处于 down，故 MAC 必须在置 up 之前落。

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ctVethCmdTimeout 单条内核命令的等待上界（`ip`/`nsenter` 正常毫秒级返回；上界只为防「底座
// 假死拖垮巡检/提交」，超时如实报错）。ctVethIO 的 seam 不带 ctx（与 DHCP tap 客户端同族），
// 故由实现自带这个有界上下文。
const ctVethCmdTimeout = 15 * time.Second

// defaultCtVethLayer 真实现（非 Linux 平台见 container_veth_other.go 的兜底）。
func defaultCtVethLayer(p *Provider) ctVethLayer { return ctVethSysLayer{p: p} }

type ctVethSysLayer struct{ p *Provider }

// Open 返回真实现的操作面（无 fd/连接可持有，故每次操作取一次即可）。
func (l ctVethSysLayer) Open() (ctVethIO, error) {
	if l.p == nil || l.p.run == nil {
		return nil, fmt.Errorf("容器 vNIC 宿主端底座的 Runner 未装配（装配缺陷，请上报）")
	}
	return ctVethSysIO{p: l.p}, nil
}

// ctVethSysIO 宿主 `ip` / `nsenter` 的 veth 操作面。
type ctVethSysIO struct{ p *Provider }

func (l ctVethSysIO) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), ctVethCmdTimeout)
}

// EnsurePair 确保 veth 对存在：宿主端在 ⇒ 整对在（veth 成对同生共死），**按名复用、绝不重建**
// ——重建会打断运行中容器的网络（契约红线，见文件头生命周期）。
//
// 只有对端在的异常态（宿主端被带外删过）先把孤儿对端清掉再建：否则 `ip link add` 会撞
// `File exists`、被幂等容错吞掉，留下「命令成功、宿主端其实不在」的假收敛。
func (l ctVethSysIO) EnsurePair(host, peer string) error {
	ok, err := l.Exists(host)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	ok, err = l.Exists(peer)
	if err != nil {
		return err
	}
	if ok {
		if err := l.Delete(peer); err != nil {
			return err
		}
	}
	ctx, cancel := l.ctx()
	defer cancel()
	// ipIdem：同名对在探测与创建之间被并发建出（竞态）时按已存在处理（真机重复 add 报
	// `RTNETLINK answers: File exists`）。
	return l.p.ipIdem(ctx, "link", "add", host, "type", "veth", "peer", "name", peer)
}

// SetMaster 把宿主端 enslave 到交换机内核 bridge（bridge 不在时如实失败）。
func (l ctVethSysIO) SetMaster(name, bridge string) error {
	ctx, cancel := l.ctx()
	defer cancel()
	return l.p.ipReq(ctx, "link", "set", "dev", name, "master", bridge)
}

// SetUp 置宿主端管理员 up（down 的口不转发也不收发帧）。
func (l ctVethSysIO) SetUp(name string) error {
	ctx, cancel := l.ctx()
	defer cancel()
	return l.p.ipReq(ctx, "link", "set", "dev", name, "up")
}

// Delete 删除宿主端（veth 成对：删一端即整对消失）。不存在按已达成（幂等；ipBest 的
// `Cannot find device` 容错口径，与既有清理路径同源）。
func (l ctVethSysIO) Delete(name string) error {
	ctx, cancel := l.ctx()
	defer cancel()
	return l.p.ipBest(ctx, "link", "del", name)
}

// Exists 设备是否存在（**按名核对**：索引会被复用，一律按名判身份）。
func (l ctVethSysIO) Exists(name string) (bool, error) {
	ctx, cancel := l.ctx()
	defer cancel()
	out, err := l.p.ip(ctx, "-j", "link", "show", "dev", name)
	if err != nil {
		if notFound(out, err) {
			return false, nil
		}
		return false, fmt.Errorf("读取内核接口 %s: %w（%s）", name, err, trimOut(out))
	}
	return linkNamePresent(out, name)
}

// Attach 把容器端接进 `<pid>` 的网络命名空间：移入 → 容器内改名 → 可选 MAC → 置 up。
//
// **幂等**：容器端已不在宿主命名空间（前次 attach 已移入；或容器 restart 换了 netns、旧容器端
// 随旧 netns 消失——那种情况下宿主端也会一并消失，由 sync 按名重建）时按**容器内名字**核对：
// 存在即确保 MAC/up（重复移入/改名必然失败，会让 start/restart 路径误报）；容器里也没有它时
// 如实报错（该 veth 对是否已被带外删除？）。
func (l ctVethSysIO) Attach(peer string, pid int, niceName, mac string) error {
	if pid <= 0 {
		return fmt.Errorf("网络命名空间目标 pid 非法（%d）", pid)
	}
	inHost, err := l.Exists(peer)
	if err != nil {
		return err
	}
	if !inHost {
		return l.ensureAttachedInNetns(pid, peer, niceName, mac)
	}
	ctx, cancel := l.ctx()
	err = l.p.ipReq(ctx, "link", "set", "dev", peer, "netns", strconv.Itoa(pid))
	cancel()
	if err != nil {
		return fmt.Errorf("把容器端 %s 移入容器（pid %d）的网络命名空间失败: %w；容器是否仍在运行？", peer, pid, err)
	}
	// 刚移入 netns 的设备内核已置 down ⇒ 改名与设 MAC 此刻都能落（见文件头的顺序约束）。
	if err := l.nsenterIPReq(pid, "link", "set", "dev", peer, "name", niceName); err != nil {
		return fmt.Errorf("把容器端 %s 改名为 %s（容器 pid %d）失败（容器里是否已有同名接口？）: %w",
			peer, niceName, pid, err)
	}
	if mac != "" {
		if err := l.nsenterIPReq(pid, "link", "set", "dev", niceName, "address", mac); err != nil {
			return fmt.Errorf("设置容器内接口 %s 的 MAC %s 失败: %w", niceName, mac, err)
		}
	}
	return l.nsenterIPReq(pid, "link", "set", "dev", niceName, "up")
}

// ensureAttachedInNetns 容器端已不在宿主命名空间时的幂等路径（见 Attach 注释）。
// 设 MAC 要求设备 down（内核 eth_mac_addr 对运行中的设备返回 EBUSY）：down → 设 MAC → up。
func (l ctVethSysIO) ensureAttachedInNetns(pid int, peer, niceName, mac string) error {
	missing := func() error {
		return fmt.Errorf("容器端 %s 既不在宿主命名空间、容器（pid %d）里也没有接口 %s："+
			"该 veth 对是否已被带外删除？（宿主端在＝对在；缺失由 15s 巡检补建，容器 restart 后重新接入）",
			peer, pid, niceName)
	}
	out, err := l.nsenterIP(pid, "-j", "link", "show", "dev", niceName)
	if err != nil {
		if notFound(out, err) {
			return missing()
		}
		return fmt.Errorf("读取容器（pid %d）里的接口 %s 失败: %w（%s）", pid, niceName, err, trimOut(out))
	}
	exists, err := linkNamePresent(out, niceName)
	if err != nil {
		return fmt.Errorf("解析容器（pid %d）里的接口清单: %w", pid, err)
	}
	if !exists {
		return missing()
	}
	if mac != "" {
		if err := l.nsenterIPReq(pid, "link", "set", "dev", niceName, "down"); err != nil {
			return err
		}
		if err := l.nsenterIPReq(pid, "link", "set", "dev", niceName, "address", mac); err != nil {
			return err
		}
	}
	return l.nsenterIPReq(pid, "link", "set", "dev", niceName, "up")
}

// nsenterIP 在 `<pid>` 的网络命名空间里执行一条 `ip` 命令（读/写都经它；命令输出原样返回）。
func (l ctVethSysIO) nsenterIP(pid int, args ...string) (string, error) {
	full := append([]string{"-t", strconv.Itoa(pid), "-n", "ip"}, args...)
	ctx, cancel := l.ctx()
	defer cancel()
	l.p.mu.Lock()
	defer l.p.mu.Unlock()
	return l.p.run.Run(ctx, "nsenter", full...)
}

// nsenterIPReq 同 nsenterIP，但**必须成功**（失败如实上抛，附命令与底座输出）。
func (l ctVethSysIO) nsenterIPReq(pid int, args ...string) error {
	out, err := l.nsenterIP(pid, args...)
	if err != nil {
		return fmt.Errorf("nsenter -t %d -n ip %s: %w（%s）", pid, joinArgs(args), err, trimOut(out))
	}
	return nil
}

// linkNamePresent `ip -j link show` 的输出里是否存在该接口名（空输出＝没有）。
func linkNamePresent(out, name string) (bool, error) {
	if strings.TrimSpace(out) == "" {
		return false, nil
	}
	var rows []struct {
		Ifname string `json:"ifname"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return false, fmt.Errorf("解析接口清单: %w", err)
	}
	for _, r := range rows {
		if r.Ifname == name {
			return true, nil
		}
	}
	return false, nil
}
