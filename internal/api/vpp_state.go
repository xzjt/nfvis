package api

// VPP 运行态快照（决策 #84）。
//
// `show virtual-switches` 与 `show interfaces physical` 此前取自 committed 配置：
// 契约 §1.1 却分别要求「成员端口及状态/计数」「驱动、链接状态、速率」。
// 实测证伪（真机）：VPP 里存在而配置里没有的 BD 不会出现在列表里；接口在 VPP 中已
// down 而 CLI 仍显示 up（该列取自配置的 enabled）。
//
// 与 REST 的分工（有意为之，与既有 `show interfaces physical` 同构）：
//   REST `/virtual-switches*` 返回**资源模型**（配置对象，供 API 客户端做声明式操作），
//   CLI `show …` 返回**运行态**（供人判读「现在实际是什么」）。
// 二者事实不同源、口径不同，不是漂移。

import "errors"

// errVppNotWired VPP 运行态未装配（区别于「查询失败」）。
var errVppNotWired = errors.New("VPP 未接入（编排器未装配）")

// VppStateRuntime VPP 运行态快照来源（底座交互藏在接口后，单测用假实现）。
type VppStateRuntime interface { // BridgeDomains 全部 bridge-domain 的运行态（含成员口）。
	BridgeDomains() ([]BridgeDomainState, error)
	// InterfaceStates 接口名 → 链接状态/速率/驱动。
	InterfaceStates() (map[string]InterfaceState, error)
}

// BridgeDomainState bridge-domain 运行态；名字取自 BD-Tag（产品建 BD 时以交换机名为 tag）。
type BridgeDomainState struct {
	ID      uint32
	Name    string
	Learn   bool
	Flood   bool
	UuFlood bool
	Forward bool
	ArpTerm bool
	MacAge  uint8
	Ports   []BridgeDomainPort
}

// BridgeDomainPort BD 成员口。
type BridgeDomainPort struct {
	SwIfIndex uint32
	Name      string
	Shg       uint8
}

// InterfaceState 接口运行态。
type InterfaceState struct {
	AdminUp   bool
	LinkUp    bool
	LinkSpeed uint32 // kbps（DPDK 口可能为 0）
	DevType   string // 设备类型/驱动名
}

// bdStates / ifaceStates 运行态取值：未接入或查询失败返回错误，由调用方明确提示
// （**不**退回「配置视图」——那正是本决策要修的错误来源）。
func (x *cliExecutor) bdStates() ([]BridgeDomainState, error) {
	if x.vppState == nil {
		return nil, errVppNotWired
	}
	return x.vppState.BridgeDomains()
}

func (x *cliExecutor) ifaceStates() (map[string]InterfaceState, error) {
	if x.vppState == nil {
		return nil, errVppNotWired
	}
	return x.vppState.InterfaceStates()
}
