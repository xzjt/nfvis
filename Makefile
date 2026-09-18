# NFViS 产品工程（Go ≥ 1.26）。M3/M4 前全部检查可跑于任意平台（底座 mock）。
GO ?= go
COVER_MIN ?= 70

.PHONY: check build vet cover test archtest docscheck toolcheck prototype-check integration deb e2e

VERSION ?= 1.0.0
ARCH ?= amd64

# 可复现构建的时间锚（附录 A #88）：默认取 **HEAD 提交时间**（同一 commit + 同一 VERSION
# ⇒ 同一 deb），而不是「打包那一刻」。此前两次打包哈希必然不同，成因有两处，都要治：
#   ① tar 成员 mtime —— `install`/`go build` 刚落盘的文件带的是当前时间，随打包时刻漂移；
#   ② gzip 头里的时间戳 —— 由 SOURCE_DATE_EPOCH 关掉（`dpkg-deb` 认这个标准变量）。
# 不设它也能"重打包比对哈希"以外的验证方式，但设了才能用哈希判断「源码是否一致」。
# ⚠️ 由 `git archive` 导出的工作树（交接文档 §3.3 的同步流程）**没有 .git**，会回落到 0：
#    仍然是可复现的，但包内时间戳会是 1970——**发布请显式传真实提交时间**：
#      make deb VERSION=1.1.8 SOURCE_DATE_EPOCH=$(git log -1 --format=%ct)
SOURCE_DATE_EPOCH ?= $(shell git log -1 --format=%ct 2>/dev/null || echo 0)

# make check：提交前/CI 的统一自检入口（AGENTS.md「每次改动后的自检清单」）
# 必须含 test：CI 只跑 make check，缺此项则 internal/api（含契约↔路由守护）、aaa、
# cli、state 的测试在 CI 完全不执行，守护形同虚设。
check: vet test cover archtest docscheck toolcheck prototype-check

build:
	$(GO) build ./...

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

# 覆盖率门槛：内部核心包 ≥ 70%（协作规则 3；M3 DoD 含网络编排层，M4 DoD 含计算编排）
# *_govpp.go / *_libvirt.go / *_docker.go 是薄底座适配层，单测无法真实驱动，
# 由 nfvis-vm 上的集成测试（make integration）覆盖，故不计入本地单测门槛；
# 同包内的纯函数（domain XML 组装、账本校验）仍纳入门槛。
GATED_PKGS := ./internal/config/ ./internal/model/ ./internal/schema/ ./internal/orchestrator/network/ ./internal/orchestrator/compute/ ./internal/orchestrator/container/ ./internal/images/
COVER_EXCLUDE ?= _govpp.go _libvirt.go _docker.go
cover:
	COVER_MIN=$(COVER_MIN) COVER_EXCLUDE="$(COVER_EXCLUDE)" GO=$(GO) bash contrib/scripts/check_coverage.sh $(GATED_PKGS)

# 依赖方向守护（骨架 §3.1：CLI 前端不得 import 事务引擎/API/编排/AAA）
# -count=1 必须保留：该测试经 exec 调 go list 读取依赖，Go 构建缓存看不到
# cmd/ 源码变化，不加会缓存命中而漏报。
archtest:
	$(GO) test -count=1 ./internal/archtest/

# 原型演示代码仍须全绿（AGENTS.md 自检清单）
prototype-check:
	cd prototype && $(GO) build ./... && $(GO) vet ./... && $(GO) test ./...

# 文档一致性守护：
#  - AGENTS.md 声明的决策条数须与附录 A 实际条数一致（该数字曾三次滞后：24→28→34，故自动校验）
#  - 嵌入二进制的 openapi.json 须与契约 docs/NFViS-openapi.yaml 一致（FR-API-002，决策 #69）
docscheck:
	bash contrib/scripts/check_decisions_count.sh
	bash contrib/scripts/check_openapi_json_sync.sh

# 工具自校准：真机三件套的**判定模式**必须「已知正确 → PASS、已知错误 → FAIL」。
# 由来（发现 #9）：`cli-fulltest` 的失败判定曾用未锚定的 `校验失败` 扫全文，而 `show log audit`
# 会回显历史（某条旧审计的 detail 就含「校验失败: …」）→ 同一条命令在不同审计历史下结论不同。
# 这类"工具自身出错制造的假红"比假绿更伤信任（决策 #85），故把判定模式的自校准纳入 make check。
toolcheck:
	bash contrib/scripts/cli-fulltest-selftest.sh

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

# deb 打包（M5-10）。**须在 Linux 上执行**（dpkg-deb；交叉编译出的二进制为 linux/amd64），
# 例如 nfvis-vm：make deb VERSION=1.0.1
# **可复现**（附录 A #88）：同一 commit + 同一 VERSION 连打两次，产物 sha256 一致——
# 故哈希可用于判断「源码是否一致」（见 SOURCE_DATE_EPOCH 处的说明）。
# 产物 build/nfvis_<VERSION>_<ARCH>.deb，内含：
#   /usr/bin/{nfvisd,nfvis-cli}、/lib/systemd/system/nfvis.service、
#   /usr/share/doc/nfvis/（契约 OpenAPI + 命令树 + 规格书 + 用户手册 + 命令全表 + M5 验收记录）、
#   /usr/share/nfvis/installer/{nfvis-baseline.sh,nfvis-ssh-harden.sh}（安装期内核基线 / SSH 强化）、
#   DEBIAN/{control,postinst,prerm,postrm}（postinst 做安装期底座优化校验，FR-OPS-013）
deb:
	@command -v dpkg-deb >/dev/null 2>&1 || { echo "跳过 deb：需要 dpkg-deb（请在 Linux/nfvis-vm 上执行）"; exit 1; }
	@[ "$(SOURCE_DATE_EPOCH)" != "0" ] || echo "提示：未取到 git 提交时间（非 git 工作树），本次 SOURCE_DATE_EPOCH=0——仍可复现，但包内时间戳为 1970；发布请显式传 SOURCE_DATE_EPOCH=<unix 秒>"
	rm -rf build/deb
	install -d build/deb/usr/bin build/deb/lib/systemd/system build/deb/usr/share/doc/nfvis build/deb/DEBIAN build/deb/usr/share/nfvis/installer
	GOOS=linux GOARCH=$(ARCH) $(GO) build -trimpath -ldflags "-s -w -X github.com/xzjt/nfvis/internal/api.VersionStr=$(VERSION)" -o build/deb/usr/bin/nfvisd ./cmd/nfvisd
	GOOS=linux GOARCH=$(ARCH) $(GO) build -trimpath -ldflags "-s -w -X github.com/xzjt/nfvis/internal/cli.Version=$(VERSION)" -o build/deb/usr/bin/nfvis-cli ./cmd/nfvis-cli
	install -m 0644 deploy/nfvis.service build/deb/lib/systemd/system/nfvis.service
	install -m 0644 docs/NFViS-openapi.yaml build/deb/usr/share/doc/nfvis/
	install -m 0644 docs/NFViS-CLI命令树完整设计.md build/deb/usr/share/doc/nfvis/
	install -m 0644 docs/NFViS-系统产品需求与目标架构规格书.md build/deb/usr/share/doc/nfvis/
	install -m 0644 docs/NFViS-用户手册.md build/deb/usr/share/doc/nfvis/
	install -m 0644 docs/NFViS-CLI命令全表.md build/deb/usr/share/doc/nfvis/
	install -m 0644 docs/M5-验收记录.md build/deb/usr/share/doc/nfvis/ 2>/dev/null || true
	install -m 0755 deploy/installer/nfvis-baseline.sh build/deb/usr/share/nfvis/installer/
	install -m 0755 deploy/installer/nfvis-ssh-harden.sh build/deb/usr/share/nfvis/installer/
	install -m 0755 deploy/debian/postinst build/deb/DEBIAN/postinst
	install -m 0755 deploy/debian/prerm build/deb/DEBIAN/prerm
	install -m 0755 deploy/debian/postrm build/deb/DEBIAN/postrm
	printf 'Package: nfvis\nVersion: %s\nSection: net\nPriority: optional\nArchitecture: %s\nMaintainer: NFViS <nfvis@example.invalid>\nDepends: libc6\nRecommends: vpp, libvirt-daemon-system, docker.io\nDescription: NFViS NFV infrastructure appliance (nfvisd + nfvis-cli)\n JunOS-style CLI + REST API for VPP/KVM/Docker NFV orchestration.\n' "$(VERSION)" "$(ARCH)" > build/deb/DEBIAN/control
	install -d build/deb/usr/share/doc/nfvis
	printf 'nfvis %s\n' "$(VERSION)" > build/deb/usr/share/doc/nfvis/version
	# 归一化暂存树 mtime：`-depth` 让子项先于父目录被 touch（touch 子项会改父目录 mtime，
	# 反过来做等于白做），`-h` 连同符号链接自身一起归一。
	find build/deb -depth -exec touch -h -d @$(SOURCE_DATE_EPOCH) {} +
	TZ=UTC SOURCE_DATE_EPOCH=$(SOURCE_DATE_EPOCH) dpkg-deb --root-owner-group --build build/deb build/nfvis_$(VERSION)_$(ARCH).deb
	@echo "已生成 build/nfvis_$(VERSION)_$(ARCH).deb（SOURCE_DATE_EPOCH=$(SOURCE_DATE_EPOCH)）"

# 端到端验收（M5-11；build tag e2e + 需 nfvis 已安装/VPP 可用；CI 不跑）
e2e:
	@if [ -z "$$NFVIS_API" ]; then echo "跳过 e2e：设置 NFVIS_API（如 http://127.0.0.1:8443）"; exit 0; fi; \
	$(GO) test -tags e2e -count=1 -v ./test/e2e/...
