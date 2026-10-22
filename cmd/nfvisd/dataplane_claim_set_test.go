package main

// 修 BUG-05 的自死锁回归（真机复验时踩到，2026-10-22）：
//
// 第一版把「数据面占用」做成**每次检查都回读** `engine.Committed()`。而 `checkPCIDevices`
// 是在 commit 的 apply 阶段被调用的，`Engine.Commit` 全程持写锁、`Engine.Committed()` 也要
// 同一把锁 ⇒ **自死锁**。真机现场：`request virtual-machine-functions db13 start` 卡住，
// 随后连 `POST /login` 都 1m30s 超时；goroutine dump 里栈是
//   compute.(*Provider).checkPCIDevices → config.(*Engine).Committed → sync.Mutex.Lock
// 叠在 config.(*Engine).Commit 上。
//
// 修法：占用集改为**进程内快照**（`dataplaneClaimSet`），启动与每次提交后刷新；检查只读快照。
// 本用例钉住「Claim 绝不回调引擎」这一条——那是死锁的充分条件。

import (
	"sync"
	"testing"
	"time"

	"github.com/xzjt/nfvis/internal/model"
)

// Claim 只读快照：即使底层配置来源会永久阻塞，Claim 也必须立即返回。
func TestDataplaneClaimSetClaimNeverBlocks(t *testing.T) {
	var mu sync.Mutex
	entered := make(chan struct{}, 1)
	release := make(chan struct{})

	c := &dataplaneClaimSet{
		byBDF:    map[string]string{"0000:13:00.0": "vpp dpdk 单网卡覆盖（ens224）"},
		resolve:  func(string) (string, error) { return "", nil },
		srcOfCfg: nil, // 快照已就绪，Claim 不得再去取配置
	}
	// 模拟「配置来源被提交事务持锁卡住」：若 Claim 走配置来源就会挂在这里。
	c.srcOfCfg = func() (model.Config, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		mu.Lock()
		defer mu.Unlock()
		<-release
		return model.Config{}, nil
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		owner, ok := c.Claim("0000:13:00.0")
		if !ok || owner == "" {
			t.Errorf("快照里有的 BDF 应命中: %q %v", owner, ok)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("Claim 阻塞了——它不得回调配置来源（那正是 commit 期自死锁的成因）")
	}
	close(release)
}

// 未命中的 BDF 如实返回 false（不误判：直通没被占用的设备必须放行）。
func TestDataplaneClaimSetMissIsFalse(t *testing.T) {
	c := &dataplaneClaimSet{byBDF: map[string]string{"0000:13:00.0": "x"}}
	if _, ok := c.Claim("0000:03:00.0"); ok {
		t.Fatal("未占用的 BDF 不应判为占用")
	}
}

// nil 快照不 panic（未注入时行为与修复前一致）。
func TestDataplaneClaimSetNilSafe(t *testing.T) {
	var c *dataplaneClaimSet
	if _, ok := c.Claim("0000:13:00.0"); ok {
		t.Fatal("nil 快照应判未占用")
	}
	c.Refresh() // 不得 panic
}
