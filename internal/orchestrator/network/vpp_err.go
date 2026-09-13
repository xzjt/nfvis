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
)
