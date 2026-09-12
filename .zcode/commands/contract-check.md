---
description: 校验 NFViS 契约一致性（OpenAPI 可解析、$ref 自洽、命令树↔API 映射、FR 引用、未决标记、go test）
---

对当前仓库执行 NFViS 契约一致性检查，按顺序执行并汇报结果：

1. `cd prototype && go build ./... && go vet ./... && go test ./...`（必须全绿；正式工程结构建立后改为 `make check`）。
2. 解析 `docs/NFViS-openapi.yaml`：yaml 语法、`#/components/schemas/...` 与 `#/components/responses/...` 引用全部可解析、无孤儿 schema。
3. 从 `docs/NFViS-CLI命令树完整设计.md` 提取所有 FR-xxx 引用，逐个确认在 `docs/NFViS-系统产品需求与目标架构规格书.md` 中有定义；反向检查规格书是否有重复定义。
4. `grep -rn "待评审\|TBD\|TODO" docs/*.md` 必须为空。
5. 若本次工作区有未提交改动，检查 CLI 命令树、OpenAPI 路径、规格书附录 B 映射表三者是否同步（有代码/文档改动但只动了一侧即为漂移）。

结果分为：通过项简述；失败项给出文件、行号与修复建议。机械性问题（格式、断链）可直接修复；契约漂移和设计问题只报告不要改。
