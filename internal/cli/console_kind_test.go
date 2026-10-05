package cli

// 决策 #358：容器终端与 VM 串口走**同一条接管路径**，只有文案后缀不同——这里把后缀钉住。

import (
	"testing"

	"github.com/xzjt/nfvis/pkg/cliclient"
)

func TestConsoleKindLabel(t *testing.T) {
	if got := consoleKindLabel(&cliclient.ConsoleRequest{Kind: "container"}); got != " 容器终端" {
		t.Fatalf("container 应回「容器终端」，得 %q", got)
	}
	for _, r := range []*cliclient.ConsoleRequest{{Kind: ""}, {Kind: "vm"}, nil} {
		if got := consoleKindLabel(r); got != " 串口" {
			t.Fatalf("缺省/VM 应回「串口」，得 %q", got)
		}
	}
}

// 决策 #375（R142 B10）：会话显示名（无前后缀）单源——既作 SDK 拨号失败文案，也被 label 复用。
func TestConsoleKindName(t *testing.T) {
	if got := consoleKindName(&cliclient.ConsoleRequest{Kind: "container"}); got != "容器终端" {
		t.Fatalf("container 应回「容器终端」，得 %q", got)
	}
	for _, r := range []*cliclient.ConsoleRequest{{Kind: ""}, {Kind: "vm"}, nil} {
		if got := consoleKindName(r); got != "串口" {
			t.Fatalf("缺省/VM 应回「串口」，得 %q", got)
		}
	}
}
