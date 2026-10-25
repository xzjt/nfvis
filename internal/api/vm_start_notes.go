package api

// O-1（v3.0.29 外部测试报告，观察项；用户要求修）：VM **未声明任何 vNIC** 时 start 照常成功，
// 起来后没有任何网络——guest 里的 cloud-init/DHCP 会一直重试（串口刷 `failed N/20: request
// failed`），而 CLI/REST 与日志都没有一句提示。操作者只看到「已受理」，以为一切正常。
//
// 处置口径（**不阻断启动**）：
//   - 启动本身是合法的（无网卡的 VM 有它的用途：离线计算、调试、先建后接线），拦下来是**行为
//     变更**，不在本次修复范围；
//   - 要做的是让「系统状态正确、用户认知错误」不再发生：在 start **成功**的返回里追加一条
//     提示，说清现象（无网络连通性、guest 内会持续重试）与照做路径（怎么声明 vNIC）。
//
// 落点选择（为什么在这里而不是编排层）：
//   - `StartVMChecked` 的 `VMStartProbe.Hints` 只在**失败**路径上被渲染（`probeStart` 组装、
//     `vmStartFailureMessage/CLIText` 消费），成功路径返回的是 `VMStartProbe{OK: true}`——
//     把提示塞进 Hints 需要改 probe 的语义与两处渲染，且 REST 成功响应本来就只有
//     `{"status","name"}`（契约明写「不新增响应字段」）；
//   - 而「本 VM 声明里有没有 vNIC」是**配置事实**，api 层手里就有 committed 配置（CLI 的
//     `cfg`、REST 的 `findVM(cfg, name)`），不需要再穿一层编排接口；
//   - 因此提示在 api 层生成、由 CLI 与 REST 各自拼到**成功输出**上，判定函数单源
//     （`vmNoVNICHint`），避免两处各写一份而漂移。
//
// 与既有行为不重复：此前没有任何一层在成功路径上提过 vNIC 缺失（`vmDefinitionWarnings`
// 是**提交期**对「已变更的运行域定义」的提示，且只在 vNIC 声明**发生变化**时才出，新建一台
// 无 vNIC 的 VM 不会命中；`startRecoveryHints` 只在启动**失败**时给）。

import (
	"fmt"

	"github.com/xzjt/nfvis/internal/model"
)

// vmNoVNICHint 该 VM 未声明任何 vNIC 时返回给操作者的提示；有 vNIC 时返回空串。
//
// 措辞三要素（缺一条操作者就还是不知道该做什么）：
//   - 事实：未声明任何 vNIC ⇒ 无网络连通性；
//   - 现象：guest 内的 cloud-init/DHCP 会持续重试（免得有人去查 guest 日志找根因）；
//   - 出路：`set virtual-machine-functions <name> interfaces nic0 …`（带本 VM 名，可直接照抄）。
func vmNoVNICHint(vm model.VMFunction) string {
	if len(vm.Interfaces) > 0 {
		return ""
	}
	return fmt.Sprintf("注：本 VM 未声明任何 vNIC，将无网络连通性（guest 内的 cloud-init/DHCP 会持续重试）；"+
		"如需网络，请先 set virtual-machine-functions %s interfaces nic0 type vhost-user（并按需把它接入交换机）后重新 start",
		vm.Name)
}
