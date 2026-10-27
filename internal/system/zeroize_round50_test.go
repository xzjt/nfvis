package system

// 决策 #517（round50 只读评审，接 #500 口径）：zeroize **部分失败如实上报**。
//
// 成因：Manager.Zeroize 逐个删镜像失败只把错误塞进 res.Reason 后 continue，最终仍 return res,nil；
// REST 返回 202 `zeroized`、CLI 打印「已恢复出厂」、runHighRisk 记 success，而残留的镜像文件/索引
// 仍在——操作者看到完全成功。
//
// 本组测试钉：有残留 ⇒ 返回**非 nil 错误**（逐条列出残留）、成功清空的配置不回滚；全部清空 ⇒ nil。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/images"
	"github.com/xzjt/nfvis/internal/model"
)

// TestZeroizeReportsPartialFailure：删镜像部分失败 ⇒ 非 nil 错误 + 残留清单；配置已清空不回滚。
func TestZeroizeReportsPartialFailure(t *testing.T) {
	imgs := &fakeImages{
		metas: []images.Meta{
			{Name: "ok.qcow2", Type: images.TypeVM, SizeBytes: 10},
			{Name: "stuck.qcow2", Type: images.TypeVM, SizeBytes: 20},
		},
		fail: map[string]error{"stuck.qcow2": errors.New("设备忙")},
	}
	m, eng := newTestManager(t, imgs)
	seedConfig(t, eng, withSuperUser(model.Config{
		System:     &model.SystemConfig{Hostname: "node1"},
		Interfaces: []model.InterfaceConfig{{Name: "ens2f0"}},
	}))

	res, err := m.Zeroize(context.Background(), "admin")
	if err == nil {
		t.Fatalf("有残留必须返回非 nil 错误（不得当成功）: %+v", res)
	}
	msg := err.Error()
	if !strings.Contains(msg, "残留") || !strings.Contains(msg, "stuck.qcow2") {
		t.Fatalf("错误应逐条列出残留对象与原因: %v", err)
	}
	if len(res.Residuals) != 1 || !strings.Contains(res.Residuals[0], "stuck.qcow2") {
		t.Fatalf("结果摘要应带残留清单（供两个出口复用）: %+v", res.Residuals)
	}
	if res.RemovedImages != 1 || len(imgs.deleted) != 1 || imgs.deleted[0] != "ok.qcow2" {
		t.Fatalf("已成功删除的镜像应如实计数: %+v %v", res, imgs.deleted)
	}
	// 已成功清空的配置**不回滚**（账号已复位就是复位了；把账号装回去会违背「恢复出厂」本身）。
	got, cerr := eng.Committed()
	if cerr != nil {
		t.Fatal(cerr)
	}
	if len(got.Interfaces) != 0 || got.System != nil {
		t.Fatalf("已清空的配置不得回滚: %+v", got)
	}
}

// TestZeroizeAllRemovedSucceeds：全部清空 ⇒ nil 错误、无残留、计数正确（回归）。
func TestZeroizeAllRemovedSucceeds(t *testing.T) {
	imgs := &fakeImages{metas: []images.Meta{
		{Name: "a.qcow2", Type: images.TypeVM},
		{Name: "alpine:3.20", Type: images.TypeContainer},
	}}
	m, eng := newTestManager(t, imgs)
	seedConfig(t, eng, withSuperUser(model.Config{
		System: &model.SystemConfig{Hostname: "node1"},
	}))

	res, err := m.Zeroize(context.Background(), "admin")
	if err != nil {
		t.Fatalf("全部清空应成功: %v", err)
	}
	if res.RemovedImages != 2 || len(res.Residuals) != 0 {
		t.Fatalf("成功路径应 nil 错误、无残留: %+v", res)
	}
}
