package netkernel

import (
	"context"
	"fmt"
	"strings"
)

// 本文件是各"接口级绑定族"（QoS / 端口镜像 / 风暴抑制 / 端口安全 / ACL）共用的命令底座。
//
// 设计：这些族的下发全部落在 `tc` / `bridge` / `nft` 三个工具上，各族只负责把声明翻译成
// 命令序列。把"怎么执行一条命令、失败怎么算"收敛到这里，族实现里不重复造。
//
// 幂等口径（真机实测）：`tc qdisc add` 对已存在的 qdisc 报 `Exclusivity flag on, cannot modify`
// 或 `File exists`；`tc filter add` 不报错但会**重复追加**（故一律"先删后加"）；
// `bridge vlan add/del` 本身幂等。因此各族应遵循：**先 best-effort 删掉自己的对象，再 add**。

// tc filter 的 pref 分配表（**单一真源**）。
//
// 同一设备的 clsact 是各绑定族**共用**的：一族按 pref 摘除自己的 filter 时，pref 一旦与他族
// 相同就会把别人的过滤器一起删掉。真机集成测试抓到过这个缺陷（QoS 与风暴抑制都用 pref 10，
// 风暴抑制的撤除把刚下发的限速 policer 删了）。新增族时**必须**在此表取一个未占用的号段。
//
// ⚠ pref 只解决"撤除时别误删别人"，**不解决"执行时别短路别人"**（R2-3，真机 spike）：
// 同一个 hook 上内核按 pref 升序评估，**首个返回判决 ≥0 的 filter 一命中就返回，后续 filter
// 不再被评估**。故各族的下发形态必须"不吞掉后续 filter"——police 用
// `conform-exceed drop/continue`（超限丢、未超限继续遍历）、mirred 尾随 `continue`。
// 新增族时同样要选一个"让包继续往下走"的判决，否则低 pref 的族会把高 pref 的族静默屏蔽
// （现象：两条命令都成功、`tc filter show` 都在位、高 pref 那边计数恒 0）。
// （例外：风暴抑制广播档故意裸 `drop` 终止遍历——广播帧只落广播档，见 storm_kernel.go 的取舍注释与前提）
const (
	tcPrefQoS        = 10 // QoS 端口限速（入/出向各一条，同 pref）
	tcPrefSpan       = 20 // 端口镜像
	tcPrefStormBcast = 30 // 风暴抑制：广播
	tcPrefStormMcast = 40 // 风暴抑制：组播
)

// ---------- `tc filter show` 输出的共用读视图解析（各族读实况都走这里） ----------

// tcFilterBlocks 把 `tc filter show` 输出按过滤器切成块（每块含头部行、匹配条件与 action 明细）。
//
// 为什么必须按条分块：同一条 hook 上可能同时挂着别的族的过滤器（本产品的 QoS/镜像/风暴抑制
// 共用同一个 clsact），任何"在整个输出上做包含判断"的读视图都会把别人的过滤器算成自己的。
// tc 对每条 filter 先打印一行头部（`filter protocol … pref N <分类器> chain 0`），
// 随后是匹配与 action 行——以头部行为界切块即可。
func tcFilterBlocks(out string) []string {
	var blocks []string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			blocks = append(blocks, strings.Join(cur, "\n"))
			cur = nil
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "filter protocol") {
			flush()
		}
		cur = append(cur, line)
	}
	flush()
	return blocks
}

// tcPoliceDrops 一块 filter 文本里的 police 动作是否是"超限丢弃"判决（本产品下发的形态）。
//
// 为什么认两种写法：本产品给 police 的判决有两种历史形态，iproute2 的打印也不同——
//   - 修复前：`… burst <n> drop`（police 只有整体判决）→ 打印 `action drop`；
//   - 修复后：`… conform-exceed drop/continue`（超限丢、未超限继续遍历同 hook 的后续 filter）→
//     iproute2 把 slash 形态打印成 `action drop/continue`（第一段 = 超限判决，第二段 = 未超限判决）。
//
// 读视图按**语义**识别，不按某一版字节串：两种写法都是"超限丢弃"，都要认；反向写法
// （`action continue/drop`：超限继续、未超限被丢）不是本产品的形态，不认（认了会把限速失效
// 的过滤器报成"在位"）。调用方一般还要自己确认分类器（matchall/flower + dst_mac 等）。
func tcPoliceDrops(block string) bool {
	s := strings.ToLower(block)
	if !strings.Contains(s, "police") {
		return false
	}
	if strings.Contains(s, "drop/continue") {
		return true // 新写法（conform-exceed drop/continue 的 slash 形态）
	}
	// 旧写法：police 只打印整体判决 `action drop`。反向写法打印 `continue/drop`，不含该子串。
	return strings.Contains(s, "action drop")
}

// tcDeviceHasFilters 设备上（任一 hook）是否还有 filter。
//
// 这是**共享 clsact 能否回收**的唯一判据：只查自己的 pref 会误判"空闲"，把别的族正在用的
// hook 连带撤掉。设备不存在 / 没有 qdisc 一律按"无 filter"处理（没有可回收的对象）。
func tcDeviceHasFilters(ctx context.Context, run Runner, dev string) (bool, error) {
	for _, dir := range []string{"ingress", "egress"} {
		out, err := tcRun(ctx, run, "filter", "show", "dev", dev, dir)
		if err != nil {
			if notFound(out, err) {
				return false, nil
			}
			return false, fmt.Errorf("tc filter show dev %s %s: %w（%s）", dev, dir, err, trimOut(out))
		}
		if strings.TrimSpace(out) != "" {
			return true, nil
		}
	}
	return false, nil
}

// tcDropClsactIfIdle 设备上再无任何 filter 时回收 clsact（各族的撤除路径共用）。
func tcDropClsactIfIdle(ctx context.Context, run Runner, dev string) error {
	busy, err := tcDeviceHasFilters(ctx, run, dev)
	if err != nil {
		return err
	}
	if busy {
		return nil
	}
	out, err := tcRun(ctx, run, "qdisc", "del", "dev", dev, "clsact")
	// 真机实测的三种"已经没有 qdisc"文案：Cannot find specified qdisc / Invalid handle /
	// Parent Qdisc doesn't exists.——都按已达成处理。
	if err == nil || notFound(out, err) || strings.Contains(strings.ToLower(out), "invalid handle") {
		return nil
	}
	return fmt.Errorf("tc qdisc del dev %s clsact: %w（%s）", dev, err, trimOut(out))
}

// tcRun 执行一条 tc 命令。
func tcRun(ctx context.Context, run Runner, args ...string) (string, error) {
	return run.Run(ctx, "tc", args...)
}

// tcBest 执行一条 tc 命令，把"对象不存在"当作已达成（清理路径用）。
func tcBest(ctx context.Context, run Runner, args ...string) error {
	out, err := tcRun(ctx, run, args...)
	if err == nil || notFound(out, err) {
		return nil
	}
	return fmt.Errorf("tc %s: %w（%s）", joinArgs(args), err, trimOut(out))
}

// tcIdem 执行一条 tc 命令，把"已存在"当作成功（创建路径用）。
func tcIdem(ctx context.Context, run Runner, args ...string) error {
	out, err := tcRun(ctx, run, args...)
	if err == nil || alreadyExists(out, err) {
		return nil
	}
	return fmt.Errorf("tc %s: %w（%s）", joinArgs(args), err, trimOut(out))
}

// tcReq 执行一条必须成功的 tc 命令。
func tcReq(ctx context.Context, run Runner, args ...string) error {
	out, err := tcRun(ctx, run, args...)
	if err == nil {
		return nil
	}
	return fmt.Errorf("tc %s: %w（%s）", joinArgs(args), err, trimOut(out))
}

// bridgeRun 执行一条 bridge 命令（VLAN 表 / 端口属性）。
func bridgeRun(ctx context.Context, run Runner, args ...string) (string, error) {
	return run.Run(ctx, "bridge", args...)
}

// bridgeBest 执行一条 bridge 命令，把"对象不存在"当作已达成。
func bridgeBest(ctx context.Context, run Runner, args ...string) error {
	out, err := bridgeRun(ctx, run, args...)
	if err == nil || notFound(out, err) {
		return nil
	}
	return fmt.Errorf("bridge %s: %w（%s）", joinArgs(args), err, trimOut(out))
}

// bridgeReq 执行一条必须成功的 bridge 命令。
func bridgeReq(ctx context.Context, run Runner, args ...string) error {
	out, err := bridgeRun(ctx, run, args...)
	if err == nil {
		return nil
	}
	return fmt.Errorf("bridge %s: %w（%s）", joinArgs(args), err, trimOut(out))
}

// nftRunOn 执行一条 nft 命令（供族实现复用；Provider 上的 nftRun 走同一底层）。
func nftRunOn(ctx context.Context, run Runner, args ...string) (string, error) {
	return run.Run(ctx, "nft", args...)
}

// nftIdemOn 执行一条 nft 命令，把"已存在"当作成功。
func nftIdemOn(ctx context.Context, run Runner, args ...string) error {
	out, err := nftRunOn(ctx, run, args...)
	if err == nil || alreadyExists(out, err) {
		return nil
	}
	return fmt.Errorf("nft %s: %w（%s）", joinArgs(args), err, trimOut(out))
}

// nftBestOn 执行一条 nft 命令，把"不存在"当作已达成。
func nftBestOn(ctx context.Context, run Runner, args ...string) error {
	out, err := nftRunOn(ctx, run, args...)
	if err == nil || notFound(out, err) || alreadyExists(out, err) {
		return nil
	}
	return fmt.Errorf("nft %s: %w（%s）", joinArgs(args), err, trimOut(out))
}

// nftReqOn 执行一条必须成功的 nft 命令。
func nftReqOn(ctx context.Context, run Runner, args ...string) error {
	out, err := nftRunOn(ctx, run, args...)
	if err == nil {
		return nil
	}
	return fmt.Errorf("nft %s: %w（%s）", joinArgs(args), err, trimOut(out))
}

// ensureTableChain 确保 nft 表与（基）链存在（幂等）。
func ensureTableChain(ctx context.Context, run Runner, family, table, chain, chainSpec string) error {
	if err := nftIdemOn(ctx, run, "add", "table", family, table); err != nil {
		return err
	}
	if chainSpec == "" {
		if err := nftIdemOn(ctx, run, "add", "chain", family, table, chain); err != nil {
			return err
		}
		return nil
	}
	// chainSpec 形如 "type filter hook prerouting priority filter ;"，由调用方给出。
	return nftIdemOn(ctx, run, "add", "chain", family, table, chain,
		"{", chainSpec, "}")
}

// deleteChain 删除 nft 链（不存在按已达成）。
func deleteChain(ctx context.Context, run Runner, family, table, chain string) error {
	return nftBestOn(ctx, run, "delete", "chain", family, table, chain)
}
