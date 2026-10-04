package network

// vpp_govpp.go 里两段**不依赖真 VPP** 的逻辑单测：
//   1. connCache：取数失败即失效重连（R84-3：VPP 重启后 stats 连接陈旧，统计永久不可用）
//      + 单次取数的硬上界（决策 #362：挂起读在上界内关闭句柄、清缓存，不重连重试）；
//   2. govppNoiseWriter：重连路径上 govpp 的整条结构体 dump 被过滤，其余日志原样透传。
// 句柄是假类型，故这些用例在开发机（Windows）也能跑——不必等真机集成测试。

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeConn 假连接句柄：只有身份（id），没有行为。
type fakeConn struct{ id int }

// TestConnCacheReusesEstablishedConn：正常路径只建一次连接，且不关闭。
func TestConnCacheReusesEstablishedConn(t *testing.T) {
	connects, closed := 0, 0
	c := newConnCache(func() (*fakeConn, error) {
		connects++
		return &fakeConn{id: connects}, nil
	}, func(*fakeConn) { closed++ })

	for i := 0; i < 3; i++ {
		if err := c.use(func(h *fakeConn) error {
			if h == nil {
				return errors.New("句柄为空")
			}
			return nil
		}); err != nil {
			t.Fatalf("第 %d 次取数应成功: %v", i+1, err)
		}
	}
	if connects != 1 {
		t.Errorf("连接应只建立一次，实际 %d 次", connects)
	}
	if closed != 0 {
		t.Errorf("成功路径不应关闭连接，实际 %d 次", closed)
	}
}

// TestConnCacheReconnectsAfterReadFailure：陈旧连接（读取失败）被丢弃、重连一次后重试，
// 且新连接被缓存复用——这是 R84-3 的核心修复。
func TestConnCacheReconnectsAfterReadFailure(t *testing.T) {
	connects, closed := 0, 0
	c := newConnCache(func() (*fakeConn, error) {
		connects++
		return &fakeConn{id: connects}, nil
	}, func(*fakeConn) { closed++ })

	var seen []int
	err := c.use(func(h *fakeConn) error {
		seen = append(seen, h.id)
		if h.id == 1 {
			return errors.New("stats segment 已失效")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("重连后重试应成功: %v", err)
	}
	if connects != 2 {
		t.Errorf("应重连一次（共建立 2 个连接），实际 %d 个", connects)
	}
	if closed != 1 {
		t.Errorf("陈旧连接应被关闭一次，实际 %d 次", closed)
	}
	if len(seen) != 2 || seen[0] != 1 || seen[1] != 2 {
		t.Errorf("应先读旧连接再读新连接，实际 %v", seen)
	}

	// 新连接进入缓存：下一次取数不再重建
	if err := c.use(func(h *fakeConn) error {
		if h.id != 2 {
			return fmt.Errorf("应复用重连后的连接，实际 id=%d", h.id)
		}
		return nil
	}); err != nil {
		t.Fatalf("重连后的连接应被复用: %v", err)
	}
	if connects != 2 {
		t.Errorf("复用后不应再建立连接，实际 %d 个", connects)
	}
}

// TestConnCacheRetryFailureReported：重连后仍读失败必须如实报错（调用方据此降级），
// 且不能就此锁死——下一次调用仍会重新尝试。
func TestConnCacheRetryFailureReported(t *testing.T) {
	connects := 0
	c := newConnCache(func() (*fakeConn, error) {
		connects++
		return &fakeConn{id: connects}, nil
	}, func(*fakeConn) {})

	if err := c.use(func(*fakeConn) error { return errors.New("取数失败") }); err == nil {
		t.Fatal("重连后仍失败应报错")
	}
	if connects != 2 {
		t.Errorf("一次 use 内应重连一次，实际建立 %d 个连接", connects)
	}
	if err := c.use(func(*fakeConn) error { return errors.New("取数失败") }); err == nil {
		t.Fatal("后续调用仍应报错")
	}
	// 第 1 次 use：建连 + 重连（2 个）；第 2 次 use：复用第 2 个失败后重连（再 1 个）
	if connects != 3 {
		t.Errorf("失败不应被永久缓存（每次调用重新尝试），实际建立 %d 个连接", connects)
	}
}

// TestConnCacheConnectFailureRetriesNextTime：连接建不起来时报错、不执行读取，
// 且不缓存失败状态（VPP 起来后下一次调用能连上）。
func TestConnCacheConnectFailureRetriesNextTime(t *testing.T) {
	connects, fail := 0, true
	c := newConnCache(func() (*fakeConn, error) {
		connects++
		if fail {
			return nil, errors.New("socket 不存在")
		}
		return &fakeConn{id: connects}, nil
	}, nil) // closeFn 为 nil：丢弃时不应 panic

	read := func(*fakeConn) error { t.Fatal("连接未建立时不应执行读取"); return nil }
	if err := c.use(read); err == nil {
		t.Fatal("连接建立失败应报错")
	}
	fail = false
	if err := c.use(func(*fakeConn) error { return nil }); err != nil {
		t.Fatalf("VPP 恢复后应能连上: %v", err)
	}
	if connects != 2 {
		t.Errorf("应各尝试建立一次，实际 %d 次", connects)
	}
}

// TestConnCacheReconnectFailureKeepsBothErrors：首次取数失败 + 重连失败时，
// 两条原因都要在错误里（真机排障要看得出「连接建不起来」与「旧连接读失败」的区别）。
func TestConnCacheReconnectFailureKeepsBothErrors(t *testing.T) {
	connects := 0
	c := newConnCache(func() (*fakeConn, error) {
		connects++
		if connects > 1 {
			return nil, errors.New("VPP 未运行")
		}
		return &fakeConn{id: 1}, nil
	}, func(*fakeConn) {})

	err := c.use(func(*fakeConn) error { return errors.New("旧连接读失败") })
	if err == nil {
		t.Fatal("重连失败应报错")
	}
	for _, want := range []string{"VPP 未运行", "旧连接读失败"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误应含 %q，实际 %q", want, err.Error())
		}
	}
}

// TestConnCacheUseIsSerialized：并发取数不得同时进同一连接（半关闭连接被并发使用），
// 且并发下连接只建立一次。
func TestConnCacheUseIsSerialized(t *testing.T) {
	connects := 0
	c := newConnCache(func() (*fakeConn, error) {
		connects++
		return &fakeConn{id: connects}, nil
	}, func(*fakeConn) {})

	var mu sync.Mutex
	inUse := 0
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if err := c.use(func(*fakeConn) error {
					mu.Lock()
					inUse++
					overlap := inUse > 1
					mu.Unlock()
					if overlap {
						t.Errorf("同一连接被并发使用（inUse=%d）", inUse)
					}
					time.Sleep(time.Millisecond)
					mu.Lock()
					inUse--
					mu.Unlock()
					return nil
				}); err != nil {
					t.Errorf("取数应成功: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	if connects != 1 {
		t.Errorf("并发下连接应只建立一次，实际 %d 次", connects)
	}
}

// TestConnCacheUseBoundedTimeoutClosesAndDrops：阻塞读取（govpp statsclient 没有
// 自己的超时）必须在 bound 内返回可辨识的超时错误，当前句柄被 closeFn 关闭、缓存被
// 清（下次调用重建），且**不重连重试**（决策 #362；避免再叠一个上界）。
func TestConnCacheUseBoundedTimeoutClosesAndDrops(t *testing.T) {
	connects, closed := 0, 0
	c := newConnCache(func() (*fakeConn, error) {
		connects++
		return &fakeConn{id: connects}, nil
	}, func(*fakeConn) { closed++ })

	release := make(chan struct{})
	defer close(release) // 放行阻塞的 fn，避免测试结束仍挂着 goroutine

	start := time.Now()
	err := c.useBounded(100*time.Millisecond, func(*fakeConn) error {
		<-release // 永不自行返回的读取
		return nil
	})
	if err == nil {
		t.Fatal("阻塞读取应在上界内报错")
	}
	if !errors.Is(err, errConnCacheTimeout) {
		t.Fatalf("错误应可辨识为上界超时，实际: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("应在 100ms 上界附近返回，实际 %s", elapsed)
	}
	if closed != 1 {
		t.Errorf("上界到应关闭当前句柄一次，实际 %d 次", closed)
	}
	if connects != 1 {
		t.Errorf("上界路径不应重连重试，实际建立 %d 个连接", connects)
	}

	// 缓存已被清：下一次调用走 ensureLocked 重建
	if err := c.useBounded(time.Second, func(h *fakeConn) error {
		if h.id != 2 {
			return fmt.Errorf("应重建连接（id=2），实际 id=%d", h.id)
		}
		return nil
	}); err != nil {
		t.Fatalf("上界后下一次调用应重建并成功: %v", err)
	}
	if connects != 2 {
		t.Errorf("缓存应被清空并重建，实际建立 %d 个连接", connects)
	}
}

// TestConnCacheUseBoundedNormalPathUnchanged：正常读取不受硬上界影响；普通失败仍沿用
// 既有「失效重连一次后重试」语义（R84-3）——硬上界只针对「挂住不返回」，不改存量策略。
func TestConnCacheUseBoundedNormalPathUnchanged(t *testing.T) {
	connects, closed := 0, 0
	c := newConnCache(func() (*fakeConn, error) {
		connects++
		return &fakeConn{id: connects}, nil
	}, func(*fakeConn) { closed++ })

	if err := c.useBounded(time.Second, func(*fakeConn) error { return nil }); err != nil {
		t.Fatalf("正常读取应成功: %v", err)
	}
	if connects != 1 || closed != 0 {
		t.Errorf("正常路径应只建一次连接且不关闭，实际 connects=%d closed=%d", connects, closed)
	}

	seen := 0
	err := c.useBounded(time.Second, func(h *fakeConn) error {
		seen++
		if h.id == 1 {
			return errors.New("stats segment 已失效")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("普通失败应失效重连一次后重试成功: %v", err)
	}
	if connects != 2 || closed != 1 || seen != 2 {
		t.Errorf("既有重连语义应保留（connects=2 closed=1 seen=2），实际 connects=%d closed=%d seen=%d",
			connects, closed, seen)
	}
}

// TestConnCacheUseBoundedNilCloseFnNoPanic：closeFn 为 nil 时上界路径不 panic，
// 缓存同样被清（下次调用重建）。
func TestConnCacheUseBoundedNilCloseFnNoPanic(t *testing.T) {
	connects := 0
	c := newConnCache(func() (*fakeConn, error) {
		connects++
		return &fakeConn{id: connects}, nil
	}, nil)

	release := make(chan struct{})
	defer close(release)
	err := c.useBounded(50*time.Millisecond, func(*fakeConn) error {
		<-release
		return nil
	})
	if err == nil || !errors.Is(err, errConnCacheTimeout) {
		t.Fatalf("阻塞读取应返回上界超时错误，实际: %v", err)
	}
	if err := c.useBounded(time.Second, func(*fakeConn) error { return nil }); err != nil {
		t.Fatalf("上界后下一次调用应重建并成功: %v", err)
	}
	if connects != 2 {
		t.Errorf("缓存应被清空并重建，实际建立 %d 个连接", connects)
	}
}

// TestGovppNoiseWriterFiltersReplyDump：重连路径上的整条结构体 dump 被丢弃，
// 其余日志（含真正的错误）原样透传；丢弃也必须报「已写」长度，否则 logrus 会当成短写错误。
func TestGovppNoiseWriterFiltersReplyDump(t *testing.T) {
	var buf bytes.Buffer
	w := govppNoiseWriter{out: &buf}

	noisy := `time="2026-09-26T00:00:00Z" level=warning msg="ignoring received reply: &{seqNum:12 data:[1 2 3 4 5 6 7 8]}" chanId=0 expSeqNum=13`
	n, err := w.Write([]byte(noisy))
	if err != nil {
		t.Fatalf("丢弃噪音不应报错: %v", err)
	}
	if n != len(noisy) {
		t.Errorf("丢弃时也应报已写 %d 字节，实际 %d（会被 logrus 当短写）", len(noisy), n)
	}
	if buf.Len() != 0 {
		t.Errorf("噪音行应被丢弃，实际写出: %q", buf.String())
	}

	for _, keep := range []string{
		`time="2026-09-26T00:00:00Z" level=error msg="Unable to send message" error="broken pipe"`,
		`time="2026-09-26T00:00:00Z" level=warning msg="VPP is not responding, the health check exceeded threshold for timeouts (>3)"`,
	} {
		if _, err := w.Write([]byte(keep)); err != nil {
			t.Fatalf("非噪音行应正常写出: %v", err)
		}
		if !strings.Contains(buf.String(), keep) {
			t.Errorf("非噪音行应原样透传，实际 %q", buf.String())
		}
	}
}
