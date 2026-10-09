package network

import (
	"errors"
	"testing"

	"go.fd.io/govpp/api"
)

func TestVPPErrHelpers(t *testing.T) {
	if !vppErrIs(api.VPPApiError(vppValueExist), vppValueExist) {
		t.Fatalf("应识别 VALUE_EXIST")
	}
	if !vppErrIs(api.VPPApiError(vppBdExists), vppBdExists, vppValueExist) {
		t.Fatalf("应识别 BD 已存在")
	}
	if vppErrIs(errors.New("普通错误"), vppValueExist) {
		t.Fatalf("普通错误不应误判")
	}
	if code, ok := vppErrCode(api.VPPApiError(vppTableExist)); !ok || code != vppTableExist {
		t.Fatalf("提取错误码: %d %v", code, ok)
	}
	if _, ok := vppErrCode(errors.New("x")); ok {
		t.Fatalf("非 VPP 错误不应提取到码")
	}
}

// 分类插件自己的「表不存在」码（-91）必须被 stormAbsentCode 认作「对象本就不在」。
//
// 真机由来（nfvis-vm，VPP 26.06）：风暴抑制带链删会顺带删掉链上那张分类表，随后对链上那张做
// 「删前形状复核 / 属性读取」就收到 `Classify table not found (-91)`；只认 -6/-65/-81 会把
// 「表已不存在」当硬错，中止整次提交——现场表现为**风暴配置删不掉、policer 残留**（dev 修复版实测）。
func TestStormAbsentCodeIncludesClassifyTableNotFound(t *testing.T) {
	if !stormAbsentCode(api.VPPApiError(vppClassifyTableNotFound)) {
		t.Fatalf("-91（分类表不存在）应被 stormAbsentCode 认作已达成")
	}
	if !stormAbsentCode(api.VPPApiError(vppNoSuchEntry)) || !stormAbsentCode(api.VPPApiError(vppNoSuchTable)) {
		t.Fatalf("既有 -6/-65 判定不应回归")
	}
	if stormAbsentCode(errors.New("普通错误")) {
		t.Fatalf("普通错误不应被认作已达成")
	}
}
