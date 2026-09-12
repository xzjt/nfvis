// nfvis-cli-proto — NFViS CLI 补全引擎交互原型。
// 模拟后端，仅演示三层能力：raw 模式行编辑、?/Tab 补全（含动态候选）、
// JunOS 风格配置事务（candidate/commit/commit confirmed/rollback/compare）。
// 真实实现中本文件的 engine 部分替换为对 nfvisd API 的调用（见工程骨架文档）。
package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	e := NewEngine()
	if len(os.Args) >= 3 && os.Args[1] == "-c" {
		out := e.Execute(strings.Join(os.Args[2:], " "))
		fmt.Print(out)
		return
	}
	RunREPL(e)
}
