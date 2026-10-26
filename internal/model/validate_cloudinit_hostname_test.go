package model

// round49（外部走查）：VNF 的 `cloud-init.hostname` 零校验。
//
// 缺陷形态：任何字符串都能落进 seed 的 YAML（hostname: <原样>），而非法值要到 guest 侧才以
// 异常形态出现（主机名不生效/被 cloud-init 拒绝），提交与读视图全绿。本文件钉住提交期校验：
// 只接受 RFC1123 主机名形态（字母数字与 '-'、以 '.' 分段、总长 ≤253、单段 ≤63、段首尾非 '-'），
// 并额外拒绝「整体全为数字」（点分十进制会与 IP 地址混淆，RFC1123 §2.1 明确这种形态不是合法
// 主机名）。seed 生成侧的 YAML 转义由另一处负责（不在本条）。

import (
	"strings"
	"testing"
)

func vmWithHostname(h string) Config {
	c := validBase()
	c.VirtualMachineFunctions[0].CloudInit = &CloudInit{Hostname: h}
	return c
}

func TestValidateCloudInitHostnameRejectsInvalid(t *testing.T) {
	long254 := strings.Repeat("a", 254)
	longLabel := strings.Repeat("a", 64) + ".example.com"
	ok253 := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." +
		strings.Repeat("d", 61)
	if len(ok253) != 253 {
		t.Fatalf("夹具错误：%d", len(ok253))
	}
	for _, bad := range []string{
		"branch: blue",     // YAML 语法符号
		"a b",              // 空格
		"-x",               // 首段以 '-' 开头
		"x-",               // 首段以 '-' 结尾
		"web..example.com", // 空段
		"web.example.com.", // 尾点（空段）
		long254,            // 总长超限
		longLabel,          // 单段超限
		"192.168.1.1",      // 点分十进制（会被当 IP 地址）
		"12345",            // 整体全为数字
		"中文主机名",            // 非 ASCII
		"host_name",        // 下划线
	} {
		c := vmWithHostname(bad)
		got := Validate(c)
		found := false
		for _, e := range got {
			if strings.Contains(e.Path, "cloud_init.hostname") {
				found = true
			}
		}
		if !found {
			t.Fatalf("主机名 %q 应被拒（实际错误：%v）", bad, got)
		}
	}
	// 长度边界：253 合法
	mustNoErr(t, Validate(vmWithHostname(ok253)))
}

func TestValidateCloudInitHostnameAcceptsRFC1123(t *testing.T) {
	for _, good := range []string{"fw-1", "web.example.com", "a", "A-B-c", "0abc", "host123", "n1.example"} {
		if errs := Validate(vmWithHostname(good)); len(errs) != 0 {
			t.Fatalf("主机名 %q 应通过校验，实际 %v", good, errs)
		}
	}
	// 未声明 cloud-init / hostname 为空：不校验（沿用既有语义）
	c := validBase()
	c.VirtualMachineFunctions[0].CloudInit = &CloudInit{}
	mustNoErr(t, Validate(c))
}

// 报错文案要点名取值、给依据与示例（操作者据此改）。
func TestValidateCloudInitHostnameMessageHasHint(t *testing.T) {
	errs := Validate(vmWithHostname("branch: blue"))
	mustErrContaining(t, errs, "cloud_init.hostname", "web.example.com")
	mustErrContaining(t, errs, "cloud_init.hostname", "branch: blue")
}
