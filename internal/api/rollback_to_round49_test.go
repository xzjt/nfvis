package api

// round49（主会话收口的新缺陷）：`request system software rollback [to <版本>]` 的 `to` 此前被
// 命令树接受、被 CLI 执行器**静默忽略**（REST 也没有版本参数）——点名版本会拿到自动选版的结果。
// 本文件钉住三条出口：CLI 解析并真正点名、REST `?version=` 同源、装配不支持时不静默改道。

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/system"
)

// rbTargetSoftware 记录被点名的回退版本（可选能力 softwareVersionRollbacker）。
type rbTargetSoftware struct {
	stubSoftware
	got []string
}

func (f *rbTargetSoftware) RollbackTo(_ context.Context, version string) (system.SoftwareResult, error) {
	f.got = append(f.got, version)
	return system.SoftwareResult{
		Package:  "nfvis_" + version + "_amd64.deb",
		Previous: "3.0.31",
		Version:  version,
	}, nil
}

func TestCLIRollbackToPassesRequestedVersion(t *testing.T) {
	x, _ := newCLIKit(t)
	fake := &rbTargetSoftware{}
	x.sw = fake

	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request system software rollback to 3.0.29 --yes")
	if len(fake.got) != 1 || fake.got[0] != "3.0.29" {
		t.Fatalf("CLI 必须把点名的版本交给 RollbackTo，实际点名=%v；输出=%s", fake.got, res.Output)
	}
	if !strings.Contains(res.Output, "3.0.29") {
		t.Fatalf("回显应含目标版本（否则操作者看不出装的是哪个）：%s", res.Output)
	}
}

func TestCLIRollbackWithoutTargetUsesAuto(t *testing.T) {
	x, _ := newCLIKit(t)
	fake := &rbTargetSoftware{}
	x.sw = fake

	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "request system software rollback --yes")
	if len(fake.got) != 0 {
		t.Fatalf("不带 to 时不得调用 RollbackTo：%v", fake.got)
	}
	if strings.Contains(res.Output, "%%") {
		t.Fatalf("不带 to 的既有形态应照常成功：%s", res.Output)
	}
}

// 缺值/未知尾参必须报语法错，**不得**静默按自动选版照做。
func TestCLIRollbackRejectsMalformedTarget(t *testing.T) {
	for _, cmd := range []string{
		"request system software rollback to",
		"request system software rollback bogus 3.0.29",
		"request system software rollback to 3.0.29 extra",
	} {
		x, _ := newCLIKit(t)
		fake := &rbTargetSoftware{}
		x.sw = fake
		res := x.Execute("admin", aaa.ClassSuperUser, "ssh", cmd+" --yes")
		if !strings.Contains(res.Output, "%%") {
			t.Fatalf("%q 应报语法错误而不是照做：%s", cmd, res.Output)
		}
		if len(fake.got) != 0 {
			t.Fatalf("%q 不得调用 RollbackTo：%v", cmd, fake.got)
		}
	}
}

// REST 同源：`?version=` 点名；未给版本时仍走自动选版（回归）。
func TestRESTRollbackVersionQuery(t *testing.T) {
	fake := &rbTargetSoftware{}
	ts := newTestServerOpts(t, Options{Software: fake})
	token := loginAdmin(t, ts)

	status, _, body := cfgRequest(t, http.MethodPost,
		ts.URL+APIPrefix+"/system/software:rollback?version=3.0.29", token, nil, nil)
	if status != http.StatusAccepted {
		t.Fatalf("点名版本应 202：%d %s", status, body)
	}
	if len(fake.got) != 1 || fake.got[0] != "3.0.29" {
		t.Fatalf("REST 必须把 version 交给 RollbackTo：%v", fake.got)
	}

	status, _, body = cfgRequest(t, http.MethodPost,
		ts.URL+APIPrefix+"/system/software:rollback", token, nil, nil)
	if status != http.StatusAccepted {
		t.Fatalf("不带 version 的既有形态应 202：%d %s", status, body)
	}
	if len(fake.got) != 1 {
		t.Fatalf("不带 version 不得再调 RollbackTo：%v", fake.got)
	}
}

// 注（真机验证后的收口）：曾有一条「装配不支持指定版本时如实报错」的用例，对应当时把
// RollbackTo 做成**可选接口**、装配适配器漏转发只能在运行期报「不支持」。真机验证当场抓到
// 那个漏转发（`rollback to` 恒报不支持）后，RollbackTo 已升为 SoftwareRuntime 的成员方法：
// 适配器漏实现**编译期即失败**，该运行期分支与用例一并删除（`internal/api/system_sw.go` 的
// `var _ SoftwareRuntime = (*system.SoftwareManager)(nil)` 就是这条编译期保证）。
