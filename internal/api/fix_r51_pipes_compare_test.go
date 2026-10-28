package api

// round51 评审 L15（CLI 半边）：`show configuration | compare` 的差异属于**持有者**。
//
// 与 REST `/configuration/diff` 同一口径：非持有者没有自己的 candidate，因而「与自己无差异」。
// 此前 `| compare` 只看「存在某个编辑会话」（Engine.CompareCandidate 的 requireEditingLocked
// 等于 candidate != nil），于是另一个 CLI 会话能读到**别人**的未提交草稿。
//
// 判据与 cfgShow（FR-CFG-009）同源：sessionHoldsLock（按会话稳定 ID；无 ID 时退回身份键）。

import (
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
)

func TestFixR51PipeCompareHidesOtherSessionsDraft(t *testing.T) {
	x, _ := newCLIKit(t)

	// 持有者：进入编辑态并改一处（未提交）。
	run(t, x, "admin", aaa.ClassSuperUser, "ssh",
		"configure",
		"set system hostname holder-node",
	)

	// ① 持有者自己看差异：能看到自己的草稿。
	got := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show configuration | compare")
	if !strings.Contains(got.Output, "holder-node") {
		t.Fatalf("持有者应能看到自己的 candidate 差异: %q", got.Output)
	}

	// ② 另一个会话（不同身份）：不得看到别人的草稿——空差异。
	other := x.Execute("ops", aaa.ClassSuperUser, "ssh", "show configuration | compare")
	if strings.Contains(other.Output, "holder-node") {
		t.Fatalf("非持有者不得看到他人候选: %q", other.Output)
	}
	if strings.TrimSpace(other.Output) != "" {
		t.Fatalf("非持有者的 `| compare` 应为空差异: %q", other.Output)
	}

	// ③ 非持有者的历史快照比较（`| compare rollback <n>`）不受影响：那是 committed 历史，不是
	//    他人草稿，故不得被本轮门控误伤成空。形态（是否给得出快照）取决于历史深度，这里只钉
	//    「不因门控而变成空输出」。
	hist := x.Execute("ops", aaa.ClassSuperUser, "ssh", "show configuration | compare rollback 1")
	if strings.TrimSpace(hist.Output) == "" {
		t.Fatalf("历史比较不该被本轮门控误伤成空输出: %q", hist.Output)
	}
}
