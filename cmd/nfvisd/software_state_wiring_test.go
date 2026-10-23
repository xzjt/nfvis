package main

// 修 BUG-17 的**装配**守护：`softwareController` 必须把 PackageState 转发出去。
//
// 为什么需要静态守护：api 侧用**可选接口**（类型断言）取这个能力，装配处漏转发不会编译失败，
// 只会静默退化成「无法核对」——而 internal/api 的单测都注入带该方法的假件，抓不到这个缺口
// （子会话实证：用一个复制生产接线的临时程序才照出来）。静态断言是这里唯一成本可控的守护。

import (
	"os"
	"strings"
	"testing"
)

func TestSoftwareControllerForwardsPackageState(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读 main.go: %v", err)
	}
	text := string(src)

	// 方法必须存在……
	if !strings.Contains(text, "func (c *softwareController) PackageState(") {
		t.Fatal("softwareController 缺少 PackageState 转发——api 读口会静默退化成「无法核对」")
	}
	// ……并且真的转给底层实现（不能返回空串糊弄）。
	idx := strings.Index(text, "func (c *softwareController) PackageState(")
	body := text[idx:]
	if end := strings.Index(body[1:], "\nfunc "); end > 0 {
		body = body[:end+1]
	}
	if !strings.Contains(body, "c.m.PackageState(") {
		t.Fatalf("PackageState 必须转发到 c.m.PackageState（否则读口拿不到事实）：\n%s", body)
	}
}
