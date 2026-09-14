package api

// M5-9 收尾：`request interfaces <ifname> enable|disable`（契约 §1.2 → 等价接口配置 PUT）
// 与 `request sriov create-vfs|delete-vfs`（FR-NET-004）的守护进程侧实现。
//
// enable/disable 经 candidate+commit 一步事务落地（契约把该命令映射为 PUT /interfaces/{n}）；
// SR-IOV 经 SRIOVSetter（sysfs sriov_numvfs 写入，PF 不支持时由底座明确报错，vmxnet3 即此类）。

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/xzjt/nfvis/internal/model"
)

// requestInterfaces：request interfaces <ifname> enable|disable。
func (x *cliExecutor) requestInterfaces(user, source string, t []string) string {
	if len(t) < 2 || (t[1] != "enable" && t[1] != "disable") {
		return "%% 语法: request interfaces <ifname> enable|disable\n"
	}
	ifname, action := t[0], t[1]
	enable := action == "enable"
	summary, err := x.commitMutate(user, source, func(c *model.Config) error {
		for i := range c.Interfaces {
			if c.Interfaces[i].Name != ifname {
				continue
			}
			v := enable
			c.Interfaces[i].Enabled = &v
			return nil
		}
		return fmt.Errorf("接口 %s 未在配置中声明（先 set interfaces %s …）", ifname, ifname)
	})
	if err != nil {
		return "%% " + err.Error() + "\n"
	}
	return fmt.Sprintf("接口 %s 已置为 %s；%s", ifname, action, summary)
}

// requestSRIOV：request sriov create-vfs <ifname> count <n> | delete-vfs <ifname> vf <n>。
func (x *cliExecutor) requestSRIOV(user, source string, t []string) string {
	if x.sriov == nil {
		return "%% SR-IOV 不可用（编排器未装配）\n"
	}
	if len(t) < 2 {
		return "%% 语法: request sriov create-vfs <ifname> count <n> | delete-vfs <ifname> vf <n>\n"
	}
	ifname := t[1]
	switch t[0] {
	case "create-vfs":
		if len(t) < 4 || t[2] != "count" {
			return "%% 语法: request sriov create-vfs <ifname> count <n>\n"
		}
		n, err := strconv.Atoi(t[3])
		if err != nil || n <= 0 {
			return "%% count 必须为正整数\n"
		}
		if err := x.sriov.SetVFCount(context.Background(), ifname, n); err != nil {
			return "%% " + err.Error() + "\n"
		}
		return fmt.Sprintf("接口 %s SR-IOV VF 数量已置为 %d\n", ifname, n)
	case "delete-vfs":
		if len(t) < 4 || t[2] != "vf" {
			return "%% 语法: request sriov delete-vfs <ifname> vf <n>\n"
		}
		if _, err := strconv.Atoi(t[3]); err != nil {
			return "%% vf 编号必须为整数\n"
		}
		// VF 回收语义：按配置中的当前 VF 数减一（删除指定编号需 PF 侧逐 VF 操作，
		// V1 以“数量”为配置面，见决策 #10/契约 §1.2）。
		cur := 0
		if cfg, err := x.engine.Committed(); err == nil {
			for _, ifc := range cfg.Interfaces {
				if ifc.Name == ifname && ifc.Sriov != nil {
					cur = ifc.Sriov.VFCount
				}
			}
		}
		if cur == 0 {
			return fmt.Sprintf("%% 接口 %s 当前未配置 VF（无可回收）\n", ifname)
		}
		next := cur - 1
		if err := x.sriov.SetVFCount(context.Background(), ifname, next); err != nil {
			return "%% " + err.Error() + "\n"
		}
		return fmt.Sprintf("接口 %s SR-IOV VF 数量 %d → %d\n", ifname, cur, next)
	}
	return fmt.Sprintf("%% 无效命令: request sriov %s（可用：create-vfs|delete-vfs）\n", strings.Join(t, " "))
}
