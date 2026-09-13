# NFViS 产品工程（Go ≥ 1.26）。M3/M4 前全部检查可跑于任意平台（底座 mock）。
GO ?= go
COVER_MIN ?= 70

.PHONY: check build vet cover test archtest prototype-check integration

# make check：提交前/CI 的统一自检入口（AGENTS.md「每次改动后的自检清单」）
check: vet cover archtest prototype-check

build:
	$(GO) build ./...

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

# 覆盖率门槛：内部核心包 ≥ 70%（协作规则 3；M3 DoD 含网络编排层）
# *_govpp.go 是薄 binary-API 适配层，单测无法真实驱动（govpp mock adapter 的 Connect 阻塞），
# 由 nfvis-vm 上的集成测试（make integration）覆盖，故不计入本地单测门槛。
GATED_PKGS := ./internal/config/ ./internal/model/ ./internal/schema/ ./internal/orchestrator/network/
COVER_EXCLUDE ?= _govpp.go
cover:
	COVER_MIN=$(COVER_MIN) COVER_EXCLUDE=$(COVER_EXCLUDE) GO=$(GO) bash contrib/scripts/check_coverage.sh $(GATED_PKGS)

# 依赖方向守护（骨架 §3.1：CLI 前端不得 import 事务引擎/API/编排/AAA）
# -count=1 必须保留：该测试经 exec 调 go list 读取依赖，Go 构建缓存看不到
# cmd/ 源码变化，不加会缓存命中而漏报。
archtest:
	$(GO) test -count=1 ./internal/archtest/

# 原型演示代码仍须全绿（AGENTS.md 自检清单）
prototype-check:
	cd prototype && $(GO) build ./... && $(GO) vet ./... && $(GO) test ./...

# 真机集成测试（M3）：需 VPP 运行环境（nfvis-vm）。无环境时跳过并提示，CI 不跑。
# 约定：build tag integration + 环境变量 NFVIS_VPP_SOCK（缺省 /run/vpp/api.sock）。
# 覆盖：M3-1 连接测试（internal/orchestrator）+ M3-10 主链路（test/integration：
# 建交换机→通流→改配置→收敛）。建议先 `pkill -x nfvisd` 避免与守护进程争用同一网口。
integration:
	@if [ -z "$$NFVIS_VPP_SOCK" ] && [ ! -S /run/vpp/api.sock ]; then \
		echo "跳过 integration：未找到 VPP socket（设置 NFVIS_VPP_SOCK 或在 nfvis-vm 上运行）"; \
		exit 0; \
	fi; \
	$(GO) test -tags integration -count=1 -v ./test/integration/... ./internal/orchestrator/...
