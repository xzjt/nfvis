package netkernel

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// 端口镜像（model.PortMirroring）在内核数据面的实现：tc mirred。
//
// VPP 侧走 span 插件；内核等价物是把源口某个 hook 上的流量复制一份发往分析口——
// 源口 clsact 的 ingress/egress hook 各挂一条 matchall filter + action mirred egress mirror。
// direction=both（或留空，与 VPP 侧口径一致）装两个 hook。
//
// 与 QoS 族共用同一套 clsact 底座（qosEnsureClsact / qosFilterDelete 等，见 qos_kernel.go），
// 但用**专用 pref**（spanFilterPref）区分绑定，两族装在同一设备上互不误删。
const spanFilterPref = tcPrefSpan

type spanManager struct{ run Runner }

func newSpanManager(run Runner) *spanManager {
	if run == nil {
		run = NewExecRunner()
	}
	return &spanManager{run: run}
}

// Apply 收敛一条镜像会话：源口指定方向上的流量复制到分析口。
//
// 幂等且能改向：先把本族在两 hook 上的 filter 都摘掉，再按声明重建——方向由 both 改单边时
// 旧的那半边也必须撤掉，只 add 会留下过期的镜像（数据面照旧复制，配置却已改小）。
//
// mirred 尾随的 `continue`（R2-3，真机 spike 实测）：同一条 clsact hook 上各族共用
// （QoS pref 10 / 镜像 20 / 风暴抑制 30/40），内核**首个返回判决 ≥0 的 filter 命中即返回**——
// mirred 默认判决是 `pipe`(3) ≥ 0，会把排在后面的风暴抑制整族静默屏蔽（镜像在复制、抑制计数
// 恒 0）。`continue` 让包判完镜像后继续遍历后续 filter，镜像本身照常发生。
func (m *spanManager) Apply(ctx context.Context, srcDev, analyzerDev, direction string) error {
	if srcDev == "" || analyzerDev == "" {
		return fmt.Errorf("镜像源接口与分析口都不能为空")
	}
	dirs, err := spanDirections(direction)
	if err != nil {
		return err
	}
	if err := qosEnsureClsact(ctx, m.run, srcDev); err != nil {
		return err
	}
	// 分析口要把镜像包发出去，必须先 up。真机实测：分析口 down 时 mirred filter 照样能装上，
	// 但镜像包发不出去（静默无镜像）——故这里必须成功，失败如实上报。
	if out, err := m.run.Run(ctx, "ip", "link", "set", "dev", analyzerDev, "up"); err != nil {
		return fmt.Errorf("ip link set dev %s up: %w（%s）", analyzerDev, err, trimOut(out))
	}
	for _, d := range []string{"ingress", "egress"} {
		if err := qosFilterDelete(ctx, m.run, srcDev, d, spanFilterPref); err != nil {
			return err
		}
	}
	for _, d := range dirs {
		if err := tcReq(ctx, m.run, "filter", "add", "dev", srcDev, d,
			"pref", strconv.Itoa(spanFilterPref),
			"matchall", "action", "mirred", "egress", "mirror", "dev", analyzerDev,
			"continue"); err != nil {
			return err
		}
	}
	return nil
}

// Delete 撤销一条镜像会话：源口两 hook 上的本族 filter 都摘掉。
//
// 对象级删除是整体收敛（镜像对象连同它的方向一起去掉），故不分方向、一律清两个 hook，
// 不留半边的残留 filter；direction 参数只为与接口签名一致。
func (m *spanManager) Delete(ctx context.Context, srcDev, direction string) error {
	if srcDev == "" {
		return fmt.Errorf("镜像源接口不能为空")
	}
	for _, d := range []string{"ingress", "egress"} {
		if err := qosFilterDelete(ctx, m.run, srcDev, d, spanFilterPref); err != nil {
			return err
		}
	}
	return qosMaybeDeleteClsact(ctx, m.run, srcDev)
}

// Bound 源口上是否已装本族镜像，返回分析口名。
//
// 读内核事实（`tc filter show`），不读进程内登记；读不到或不是本族的绑定一律如实返回未绑定。
// 解析只认"mirred 到某设备的镜像"这一动作本身，**不看尾随的判决词**：默认 `pipe`（修复前的
// 写法，会短路同 hook 的后续 filter）与显式 `continue`（修复后，镜像后继续遍历）都算本族绑定
// ——判决词只是执行语义的一部分（见 Apply 的注释），不影响"有没有这条镜像"这个事实。
func (m *spanManager) Bound(ctx context.Context, srcDev string) (string, bool) {
	if srcDev == "" {
		return "", false
	}
	for _, d := range []string{"ingress", "egress"} {
		out, err := tcRun(ctx, m.run, "filter", "show", "dev", srcDev, d)
		if err != nil {
			continue
		}
		if !qosHasPref(out, spanFilterPref) {
			continue
		}
		if dev, ok := spanMirrorAnalyzer(out); ok {
			return dev, true
		}
	}
	return "", false
}

// spanDirections 镜像方向 → 要装 filter 的 hook 列表。
//
// 留空按 both（与 VPP 侧缺省一致）；其余取值如实报错，不静默降级成 both。
func spanDirections(direction string) ([]string, error) {
	switch strings.ToLower(strings.TrimSpace(direction)) {
	case "", "both":
		return []string{"ingress", "egress"}, nil
	case "ingress":
		return []string{"ingress"}, nil
	case "egress":
		return []string{"egress"}, nil
	default:
		return nil, fmt.Errorf("镜像方向必须是 ingress、egress 或 both，得到 %q", direction)
	}
}

// spanMirredRe 从 `tc filter show` 输出里取出镜像分析口名。
//
// tc 输出行形如（尾随的判决词随写法变：修复前默认 `pipe`，修复后显式 `continue`）：
//
//	action order 1: mirred (Egress Mirror to device zspa1) pipe
//	action order 1: mirred (Egress Mirror to device zspa1) continue
//
// 只取括号里的分析口名，判决词不在匹配范围内（两种写法都认）。
var spanMirredRe = regexp.MustCompile(`mirred \(Egress Mirror to device (\S+)\)`)

// spanMirrorAnalyzer 解析出镜像分析口名。
func spanMirrorAnalyzer(out string) (string, bool) {
	m := spanMirredRe.FindStringSubmatch(out)
	if m == nil {
		return "", false
	}
	return m[1], true
}
