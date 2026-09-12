# NFViS 产品工程（Go ≥ 1.26）。M3/M4 前全部检查可跑于任意平台（底座 mock）。
GO ?= go
COVER_MIN ?= 70

.PHONY: check build vet cover test prototype-check

# make check：提交前/CI 的统一自检入口（AGENTS.md「每次改动后的自检清单」）
check: vet cover prototype-check

build:
	$(GO) build ./...

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

# 覆盖率门槛：internal/schema 与事务引擎（internal/config）≥ 70%（协作规则 3）
# internal/schema 建立后加入 GATED_PKGS。
GATED_PKGS := ./internal/config/ ./internal/model/ ./internal/schema/
cover:
	@fail=0; \
	for pkg in $(GATED_PKGS); do \
		prof=$$(echo $$pkg | tr '/' '_').cover; \
		$(GO) test -coverprofile=$$prof $$pkg >/dev/null || exit 1; \
		pct=$$($(GO) tool cover -func=$$prof | awk '$$1=="total:" {gsub("%","",$$3); print $$3}'); \
		echo "$$pkg 覆盖率: $$pct%"; \
		ok=$$(awk -v p="$$pct" -v m="$(COVER_MIN)" 'BEGIN { print (p >= m) ? 1 : 0 }'); \
		[ "$$ok" = "1" ] || { echo "✗ $$pkg 覆盖率 $$pct% 低于门槛 $(COVER_MIN)%"; fail=1; }; \
		rm -f $$prof; \
	done; \
	exit $$fail

# 原型演示代码仍须全绿（AGENTS.md 自检清单）
prototype-check:
	cd prototype && $(GO) build ./... && $(GO) vet ./... && $(GO) test ./...
