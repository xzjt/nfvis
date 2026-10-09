package network

// VPP 错误码判定：govpp 把非零 retval 转成 api.VPPApiError 返回，而非填入 Reply.Retval，
// 故幂等判断必须识错类型（如 VALUE_EXIST=-81）。

import (
	"errors"

	"go.fd.io/govpp/api"
)

// vppErrCode 提取 VPP vnet API 错误码。
func vppErrCode(err error) (int32, bool) {
	var e api.VPPApiError
	if errors.As(err, &e) {
		return int32(e), true
	}
	return 0, false
}

// vppErrIs 判断 err 是否为给定的 VPP 错误码之一。
func vppErrIs(err error, codes ...int32) bool {
	code, ok := vppErrCode(err)
	if !ok {
		return false
	}
	for _, c := range codes {
		if code == c {
			return true
		}
	}
	return false
}

// VPP_VALUE_EXIST 等常用码（避免各处散落魔数）。
const (
	vppValueExist int32 = -81 // VALUE_EXIST
	vppBdExists   int32 = -119
	vppTableExist int32 = -111
	// NAT44 EI 插件特性状态（幂等启用/关闭）
	vppFeatureAlreadyDisabled int32 = -169
	vppFeatureAlreadyEnabled  int32 = -170
	// 对象本就不在位（重复删除、接口/会话已消失）
	vppNoSuchEntry int32 = -6
	// 分类表不存在（真机实测：对已不在/未挂在接口上的分类表做解绑，VPP 报 `No such table (-65)`）
	vppNoSuchTable int32 = -65
	// 分类表不存在（**分类插件自己的码**）：`classify_table_info` / 删表对不存在的表报
	// `Classify table not found (-91)`。真机实测（nfvis-vm，VPP 26.06）：带链删会顺带删掉链上那张，
	// 随后对链上那张做「删前形状复核 / 属性读取」就收到 -91——只认 -6/-65/-81 会把「表已不存在」
	// 当硬错、中止整次提交（现场：风暴配置删不掉、policer 残留）。
	vppClassifyTableNotFound int32 = -91
)

// natRemovalBenignCode 判断 NAT44 系列**移除方向**的返回码是否表示「已是目标状态」，
// 是则按成功处理：
//   - -6   对象本就不在位（重复删除；接口被重建后旧索引上的特性已随之消失）；
//   - -81  该对象的形式此前已存在（历史实现即以此为 del 幂等判定）；
//   - -169 插件未启用（地址池/接口特性随插件关闭已无实体，删除无事可做）。
//
// 不容忍的后果是：一次无害的重复删除会把整个 ApplyNAT 中止、整批 apply 打回滚
// （round84 实测：`移除接口 3 的 NAT inside 特性: No such entry (-6)` → 整批补偿回滚）。
func natRemovalBenignCode(code int32) bool {
	return code == vppNoSuchEntry || code == vppValueExist || code == vppFeatureAlreadyDisabled
}

// natRemovalBenign 判断移除方向收到的错误是否表示「已是目标状态」（错误形式，
// govpp 通常把非零 retval 转成 api.VPPApiError 返回；Reply.Retval 形式见上）。
func natRemovalBenign(err error) bool {
	return vppErrIs(err, vppNoSuchEntry, vppValueExist, vppFeatureAlreadyDisabled)
}
