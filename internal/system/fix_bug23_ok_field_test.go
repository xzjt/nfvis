package system

// BUG-23 守护（v3.0.29 外部测试报告）：`SoftwareResult.OK` 曾带 `json:"ok,omitempty"`——
// 失败时结论文件**没有 `ok` 键**，成功时才写 `"ok":true`。结论文件是对外产物（CLI 重连后
// 读它判成败，外部脚本也直接解析它），成功/失败的结构必须对称：否则 `jq '.ok == false'`
// 在失败记录上得到 null（而不是 true），Go 侧 `*bool` 同样误判。
//
// 判据（本缺陷的核心）：**成败两个方向的键集合完全一致**，且 `ok` 恒为明确的布尔值。
// 只断言「失败时为 false」不够——那样有人再加一个 omitempty 到别的恒在字段上仍会漏。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// resultKeys 从结论文件里读出 JSON 对象的顶层键集合（排序后，便于直接比较）。
func resultKeys(t *testing.T, dir string) (map[string]any, []string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "last-result.json"))
	if err != nil {
		t.Fatalf("应落盘结论：%v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("落盘内容应是合法 JSON 对象: %v\n%s", err, raw)
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return obj, keys
}

func TestBug23SoftwareResultOKFieldAlwaysPresent(t *testing.T) {
	base := SoftwareResult{
		Action:   "add",
		Version:  "3.0.29",
		Previous: "3.0.28",
		Package:  "nfvis_3.0.29_amd64.deb",
		Output:   "dpkg -i …",
		Status:   pkgStatusInstalled,
		Reason:   "（仅失败态才有）",
		Attempt:  "1792899852211982094",
		Time:     "2026-10-25T03:44:23Z",
	}

	// —— 失败态：`ok` 必须存在且为 false ——
	failDir := t.TempDir()
	failRes := base
	failRes.OK = false
	m := &SoftwareManager{dir: failDir}
	m.RecordResult(failRes)
	failObj, failKeys := resultKeys(t, failDir)
	v, exists := failObj["ok"]
	if !exists {
		t.Fatalf("失败结论必须含 ok 键（否则 jq '.ok == false' / Go *bool 会误判）；实得键集合 %v", failKeys)
	}
	if b, isBool := v.(bool); !isBool || b {
		t.Fatalf("失败结论的 ok 必须是布尔 false，实得 %#v", v)
	}

	// —— 成功态：`ok` 必须存在且为 true ——
	okDir := t.TempDir()
	okRes := base
	okRes.Status = pkgStatusInstalled
	okRes.OK = true
	m2 := &SoftwareManager{dir: okDir}
	m2.RecordResult(okRes)
	okObj, okKeys := resultKeys(t, okDir)
	if b, isBool := okObj["ok"].(bool); !isBool || !b {
		t.Fatalf("成功结论的 ok 必须是布尔 true，实得 %#v（原文键集合 %v）", okObj["ok"], okKeys)
	}

	// —— 核心判据：两个方向的键集合一致（不因成败而多/少键）——
	// 注意：本用例的输入把「有值字段」全部填满，故差异只可能来自 omitempty 的**零值**语义。
	// 这正是缺陷形态：失败的 OK 是零值（false），被 omitempty 吃掉。
	if len(failKeys) != len(okKeys) {
		t.Fatalf("成败结论的 JSON 结构必须对称：失败键 %v，成功键 %v", failKeys, okKeys)
	}
	for i := range failKeys {
		if failKeys[i] != okKeys[i] {
			t.Fatalf("成败结论的 JSON 结构必须对称：失败键 %v，成功键 %v", failKeys, okKeys)
		}
	}

	// —— 读口不受影响：`ok` 键的有无不改变「有无记录」的判据 ——
	if got, found := ReadLastResultAt(filepath.Join(failDir, "last-result.json")); !found || got.OK {
		t.Fatalf("读回应为「有记录且 ok=false」：found=%v res=%+v", found, got)
	}
}

// 空记录（全零值）也必须带 `ok`：结论文件是机器可读契约，键永远在。
// 这条同时替 `ReadLastResultAt` 的「有无记录」判据留证：它只看 version/status，
// 与本字段无关（故去掉 omitempty 不会让空记录被误当成「有记录」）。
func TestBug23OKKeyPresentOnZeroResult(t *testing.T) {
	dir := t.TempDir()
	m := &SoftwareManager{dir: dir}
	m.RecordResult(SoftwareResult{})
	obj, keys := resultKeys(t, dir)
	if v, exists := obj["ok"]; !exists {
		t.Fatalf("全零值结论也必须带 ok 键；实得键集合 %v", keys)
	} else if b, isBool := v.(bool); !isBool || b {
		t.Fatalf("全零值结论的 ok 应为 false，实得 %#v", v)
	}
	if _, found := ReadLastResultAt(filepath.Join(dir, "last-result.json")); found {
		t.Fatal("空记录不应被读口当成「有记录」（判据是 version/status，与 ok 无关）")
	}
}
