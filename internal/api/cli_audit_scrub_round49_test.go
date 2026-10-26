package api

// round49（收口 A 组点名的最后一条明文出口）：CLI `show log audit` 直读引擎审计，此前
// REST `GET /audit-logs` 与诊断包 audit.json 都已掩码、只有这条出口漏掉——命令里的口令/
// 令牌（`mysql -p…`、`--password=…`、`Authorization: Bearer …`）仍会原样上屏，只读账号
// 经 CLI 就能读到（FR-SEC-007）。本文件钉住 CLI 侧的**显示**与**结构化快照**两处。

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
)

func TestCLIShowLogAuditScrubsSecrets(t *testing.T) {
	x, engine := newCLIKit(t)
	engine.Audit("admin", "container.exec", "exec 容器 db: mysql -p987654 -h 10.0.0.1（退出码 0）", "success")

	res := x.Execute("admin", aaa.ClassSuperUser, "ssh", "show log audit")
	if strings.Contains(res.Output, "987654") {
		t.Fatalf("CLI show log audit 不得回显明文口令: %s", res.Output)
	}
	if !strings.Contains(res.Output, "«已隐藏»") {
		t.Fatalf("CLI 应留下脱敏占位（与 REST 同口径，脱敏≠掏空）: %s", res.Output)
	}
	if !strings.Contains(res.Output, "mysql -p") {
		t.Fatalf("非秘密部分应保留（审计的可追溯性）: %s", res.Output)
	}

	// 结构化快照（`| display json` / 管道用）必须同口径，否则换个出口又漏了。
	blob, err := json.Marshal(x.structured)
	if err != nil {
		t.Fatalf("序列化结构化快照: %v", err)
	}
	if strings.Contains(string(blob), "987654") {
		t.Fatalf("结构化快照不得含明文口令: %s", blob)
	}
	if !strings.Contains(string(blob), "«已隐藏»") {
		t.Fatalf("结构化快照应含脱敏占位: %s", blob)
	}
}
