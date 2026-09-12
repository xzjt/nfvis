// Package archtest 守护骨架文档 §3.1 的依赖方向规则。
//
// 规则不能只写在注释里：CLI 前端（cmd/nfvis-cli、internal/cli）与客户端 SDK
// （pkg/cliclient）不得 import 事务引擎（internal/config）、API 服务端
// （internal/api）与底座编排（internal/orchestrator）——薄客户端原则必须在
// 编译依赖上成立。本包用 `go list -deps` 读取真实编译依赖，误引入即测试失败。
package archtest

import (
	"os/exec"
	"strings"
	"testing"
)

const module = "github.com/xzjt/nfvis"

// banned 各包禁止依赖的内部包（含其子包）。键用完整 import 路径：
// 测试进程的工作目录是包目录，相对路径（./cmd/...）会解析错。
var banned = map[string][]string{
	module + "/cmd/nfvis-cli": {
		module + "/internal/config",
		module + "/internal/api",
		module + "/internal/orchestrator",
		module + "/internal/aaa",
	},
	module + "/pkg/cliclient": {
		module + "/internal/config",
		module + "/internal/api",
		module + "/internal/orchestrator",
		module + "/internal/aaa",
	},
}

func deps(t *testing.T, patterns ...string) []string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list", "-deps"}, patterns...)...)
	out, err := cmd.Output()
	if err != nil {
		msg := err.Error()
		if ee, ok := err.(*exec.ExitError); ok {
			msg += ": " + string(ee.Stderr)
		}
		t.Fatalf("go list -deps %s 失败: %s", strings.Join(patterns, " "), msg)
	}
	return strings.Fields(string(out))
}

func TestDependencyDirection(t *testing.T) {
	for pkg, bans := range banned {
		for _, dep := range deps(t, pkg) {
			for _, ban := range bans {
				if dep == ban || strings.HasPrefix(dep, ban+"/") {
					t.Errorf("%s 依赖了禁止的包 %s（骨架 §3.1 依赖方向：CLI 前端只能经 pkg/cliclient 访问守护进程）", pkg, dep)
				}
			}
		}
	}
}

// TestNoPrototypeImport 确保产品代码不引用演示原型（AGENTS.md 规则 3）。
func TestNoPrototypeImport(t *testing.T) {
	for _, dep := range deps(t,
		module+"/cmd/...", module+"/internal/...", module+"/pkg/...") {
		if strings.HasPrefix(dep, module+"/prototype") {
			t.Errorf("产品代码依赖了原型包 %s（prototype/ 仅演示，禁止产品逻辑互引）", dep)
		}
	}
}
