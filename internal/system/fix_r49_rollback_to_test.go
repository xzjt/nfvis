package system

// round49（主会话收口的新缺陷）：命令树声明 `request system software rollback [to <版本>]`
// （internal/schema/tree_oper.go），但 CLI 把尾参 `to` **丢掉**、REST 也没有版本参数——
// 操作者点名回退到某个版本，拿到的却是「自动选版」的结果。这是「参数被接受但静默无效」，
// 比报错更危险：救援动作看起来按操作者的意思执行了。
//
// 本文件钉住 `RollbackTo(ctx, version)` 的语义：命中归档 → 装**该版本**；三类请求如实拒绝
// （不存在 / 等于当前 / 高于当前），且拒绝路径**不执行** `dpkg -i`。

import (
	"context"
	"strings"
	"testing"
)

func rollbackToFixture(t *testing.T, cur, target string, archived ...string) (*SoftwareManager, *fakeRunner, SoftwareResult, error) {
	t.Helper()
	shrinkingSettle(t)
	dir := t.TempDir()
	for _, v := range archived {
		writeDeb(t, dir, "nfvis_"+v+"_amd64.deb")
	}
	fr := &fakeRunner{replies: map[string]string{"dpkg-query": cur + "\n"}}
	m := NewSoftwareManager(dir, fr.run, func() string { return cur })
	res, err := m.RollbackTo(context.Background(), target)
	return m, fr, res, err
}

// 命中归档时必须装**点名的那个版本**，而不是版本序上最接近的其它包。
func TestRollbackToInstallsRequestedVersion(t *testing.T) {
	// 当前 3.0.31：自动选版会挑 3.0.30；点名 3.0.29 必须装 3.0.29。
	_, fr, res, err := rollbackToFixture(t, "3.0.31", "3.0.29", "3.0.29", "3.0.30")
	if err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}
	got := installedPaths(fr)
	if len(got) != 1 || !strings.Contains(got[0], "nfvis_3.0.29_amd64.deb") {
		t.Fatalf("应装点名的 3.0.29：installed=%v", got)
	}
	if res.Version != "3.0.29" && !strings.Contains(res.Package, "nfvis_3.0.29_amd64.deb") {
		t.Fatalf("结果应指向 3.0.29：%+v", res)
	}
	if res.Previous != "3.0.31" {
		t.Fatalf("res.Previous 的语义是「当前版本」：得 %q", res.Previous)
	}
}

// 预发布版本形态（本仓库真实发布序列）必须能被点名命中。
func TestRollbackToAcceptsPrereleaseTarget(t *testing.T) {
	_, fr, _, err := rollbackToFixture(t, "3.0.30", "3.0.30~dev1", "3.0.30~dev1", "3.0.29")
	if err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}
	if got := installedPaths(fr); len(got) != 1 || !strings.Contains(got[0], "3.0.30~dev1") {
		t.Fatalf("应装点名的 3.0.30~dev1：installed=%v", got)
	}
}

// 版本串两侧空白应被容忍（CLI 参数易带空格）。
func TestRollbackToTrimsTarget(t *testing.T) {
	_, fr, _, err := rollbackToFixture(t, "3.0.30", "  3.0.29  ", "3.0.29")
	if err != nil {
		t.Fatalf("RollbackTo: %v", err)
	}
	if got := installedPaths(fr); len(got) != 1 {
		t.Fatalf("应执行一次安装：installed=%v", got)
	}
}

// 归档里没有该版本：如实报错 + 列出可见版本 + **不装任何东西**。
func TestRollbackToUnknownVersionRefused(t *testing.T) {
	_, fr, _, err := rollbackToFixture(t, "3.0.31", "3.0.27", "3.0.29", "3.0.30")
	if err == nil {
		t.Fatal("归档里没有该版本时必须报错（不得静默改道到别的版本）")
	}
	if !strings.Contains(err.Error(), "3.0.27") || !strings.Contains(err.Error(), "3.0.29") {
		t.Fatalf("错误要同时点名目标与可见版本：%v", err)
	}
	if got := installedPaths(fr); len(got) != 0 {
		t.Fatalf("拒绝路径不得执行 dpkg -i：installed=%v", got)
	}
}

// 目标等于当前版本：回退无意义，如实拒绝。
func TestRollbackToCurrentVersionRefused(t *testing.T) {
	_, fr, _, err := rollbackToFixture(t, "3.0.30", "3.0.30", "3.0.30", "3.0.29")
	if err == nil || !strings.Contains(err.Error(), "已是当前版本") {
		t.Fatalf("目标=当前版本应如实拒绝：err=%v", err)
	}
	if got := installedPaths(fr); len(got) != 0 {
		t.Fatalf("拒绝路径不得执行 dpkg -i：installed=%v", got)
	}
}

// 目标高于当前版本：那是升级，如实拒绝并指向 software add（与自动选版同一口径）。
func TestRollbackToNewerVersionRefused(t *testing.T) {
	_, fr, _, err := rollbackToFixture(t, "3.0.30", "3.0.31", "3.0.31", "3.0.29")
	if err == nil || !strings.Contains(err.Error(), "高于当前版本") {
		t.Fatalf("目标高于当前版本应如实拒绝：err=%v", err)
	}
	if !strings.Contains(err.Error(), "software add") {
		t.Fatalf("拒绝文案要给替代路径（software add）：%v", err)
	}
	if got := installedPaths(fr); len(got) != 0 {
		t.Fatalf("拒绝路径不得执行 dpkg -i：installed=%v", got)
	}
}

// 空版本：如实拒绝（CLI 侧 `to` 缺值另有语法错，这里是系统层的第二道）。
func TestRollbackToEmptyVersionRefused(t *testing.T) {
	_, fr, _, err := rollbackToFixture(t, "3.0.30", "   ", "3.0.29")
	if err == nil || !strings.Contains(err.Error(), "不能为空") {
		t.Fatalf("空目标应如实拒绝：err=%v", err)
	}
	if got := installedPaths(fr); len(got) != 0 {
		t.Fatalf("拒绝路径不得执行 dpkg -i：installed=%v", got)
	}
}

// 真机验证补强（round49）：目标高于当前版本时，**归档里有没有这个包**都不该改变结论——
// 操作者点名 9.9.9 而归档无 9.9.9 时，此前会报「归档中没有该版本」，要多想一步才明白
// 「这本来就是升级」。现在先与当前版本比，直接给最贴切的原因。
func TestRollbackToNewerVersionRefusedEvenIfAbsentFromArchive(t *testing.T) {
	_, fr, _, err := rollbackToFixture(t, "3.0.30", "9.9.9", "3.0.29")
	if err == nil || !strings.Contains(err.Error(), "高于当前版本") {
		t.Fatalf("高于当前版本应直接拒绝（与归档内容无关）：err=%v", err)
	}
	if got := installedPaths(fr); len(got) != 0 {
		t.Fatalf("拒绝路径不得执行 dpkg -i：installed=%v", got)
	}
}
