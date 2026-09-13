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
