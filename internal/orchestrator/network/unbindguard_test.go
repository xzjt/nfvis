package network

// 发现 #13：仍被数据面占用的口不得直接解绑（实测会把 CLI 执行器占死 + 网卡留无驱动）。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCheckUnbindAllowed(t *testing.T) {
	// 在数据面中 → 拒绝，并给出正确顺序
	err := CheckUnbindAllowed("ens224", true)
	if !errors.Is(err, ErrIfaceInDataplane) {
		t.Fatalf("应被拒: %v", err)
	}
	for _, want := range []string{"ens224", "request vpp restart", "配置"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("拒绝文案应含 %q（要能照着做）: %v", want, err)
		}
	}
	// 已离开数据面 → 放行
	if err := CheckUnbindAllowed("ens224", false); err != nil {
		t.Fatalf("不在数据面时应放行: %v", err)
	}
	// 空名不判
	if err := CheckUnbindAllowed("", true); err != nil {
		t.Fatalf("空名不应被拒: %v", err)
	}
}

// 写超时：内核写阻塞时不得把调用者拖住（发现 #13 的根因之一）。
func TestDPDKBinderWriteTimeout(t *testing.T) {
	root, _ := fakeSysfs(t, "ens224", "0000-13-00.0", "vmxnet3")
	b, _ := newTestBinder(t, root)
	blocked := make(chan struct{})
	defer close(blocked)
	b.WriteTimeout = 200 * time.Millisecond
	b.WriteFile = func(string, []byte) error {
		<-blocked // 模拟内核侧永久阻塞
		return nil
	}
	start := time.Now()
	_, err := b.Bind(context.Background(), "ens224", "")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("阻塞的写应超时报错")
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Fatalf("错误应说明超时: %v", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("必须在时限内返回（实测 %s）", elapsed)
	}
}
