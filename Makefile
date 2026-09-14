# NFViS 产品工程（Go ≥ 1.26）。M3/M4 前全部检查可跑于任意平台（底座 mock）。
GO ?= go
COVER_MIN ?= 70

.PHONY: check build vet cover test archtest docscheck prototype-check integration deb e2e

VERSION ?= 1.0.0
ARCH ?= amd64

# make check：提交前/CI 的统一自检入口（AGENTS.md「每次改动后的自检清单」）
# 必须含 test：CI 只跑 make check，缺此项则 internal/api（含契约↔路由守护）、aaa、
# cli、state 的测试在 CI 完全不执行，守护形同虚设。
check: vet test cover archtest docscheck prototype-check

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

# 文档一致性守护：AGENTS.md 声明的决策条数须与附录 A 实际条数一致
# （该数字曾三次滞后：24→28→34，故自动校验）
docscheck:
	bash contrib/scripts/check_decisions_count.sh

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
# 产物 build/nfvis_<VERSION>_<ARCH>.deb，内含：
#   /usr/bin/{nfvisd,nfvis-cli}、/lib/systemd/system/nfvis.service、
#   /usr/share/doc/nfvis/（契约 OpenAPI + 命令树 + 规格书 + M5 验收记录）、
#   DEBIAN/{control,postinst,prerm,postrm}（postinst 做安装期底座优化校验，FR-OPS-013）
deb:
	@command -v dpkg-deb >/dev/null 2>&1 || { echo "跳过 deb：需要 dpkg-deb（请在 Linux/nfvis-vm 上执行）"; exit 1; }
	rm -rf build/deb
	install -d build/deb/usr/bin build/deb/lib/systemd/system build/deb/usr/share/doc/nfvis build/deb/DEBIAN
	GOOS=linux GOARCH=$(ARCH) $(GO) build -trimpath -ldflags "-s -w -X github.com/xzjt/nfvis/internal/api.VersionStr=$(VERSION)" -o build/deb/usr/bin/nfvisd ./cmd/nfvisd
	GOOS=linux GOARCH=$(ARCH) $(GO) build -trimpath -ldflags "-s -w" -o build/deb/usr/bin/nfvis-cli ./cmd/nfvis-cli
	install -m 0644 deploy/nfvis.service build/deb/lib/systemd/system/nfvis.service
	install -m 0644 docs/NFViS-openapi.yaml build/deb/usr/share/doc/nfvis/
	install -m 0644 docs/NFViS-CLI命令树完整设计.md build/deb/usr/share/doc/nfvis/
	install -m 0644 docs/NFViS-系统产品需求与目标架构规格书.md build/deb/usr/share/doc/nfvis/
	install -m 0644 docs/M5-验收记录.md build/deb/usr/share/doc/nfvis/ 2>/dev/null || true
	install -m 0755 deploy/debian/postinst build/deb/DEBIAN/postinst
	install -m 0755 deploy/debian/prerm build/deb/DEBIAN/prerm
	install -m 0755 deploy/debian/postrm build/deb/DEBIAN/postrm
	printf 'Package: nfvis\nVersion: %s\nSection: net\nPriority: optional\nArchitecture: %s\nMaintainer: NFViS <nfvis@example.invalid>\nDepends: libc6\nRecommends: vpp, libvirt-daemon-system, docker.io\nDescription: NFViS NFV infrastructure appliance (nfvisd + nfvis-cli)\n JunOS-style CLI + REST API for VPP/KVM/Docker NFV orchestration.\n' "$(VERSION)" "$(ARCH)" > build/deb/DEBIAN/control
	install -d build/deb/usr/share/doc/nfvis
	printf 'nfvis %s\n' "$(VERSION)" > build/deb/usr/share/doc/nfvis/version
	dpkg-deb --root-owner-group --build build/deb build/nfvis_$(VERSION)_$(ARCH).deb
	@echo "已生成 build/nfvis_$(VERSION)_$(ARCH).deb"

# 端到端验收（M5-11；build tag e2e + 需 nfvis 已安装/VPP 可用；CI 不跑）
e2e:
	@if [ -z "$$NFVIS_API" ]; then echo "跳过 e2e：设置 NFVIS_API（如 http://127.0.0.1:8443）"; exit 0; fi; \
	$(GO) test -tags e2e -count=1 -v ./test/e2e/...
