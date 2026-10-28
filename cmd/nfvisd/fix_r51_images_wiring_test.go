package main

// round51 R51-M3：镜像仓库「未接入 ⇒ 503」的降级路径被 **typed-nil** 击穿。
//
// 由来：`images.Open` 失败时返回的是 **typed-nil** `*images.Store`，而装配层把它直接赋给了
// 两个**接口**字段（`system.ImageStore` / `api.ImagesRuntime`）。接口值 = 类型 + 数据，
// typed-nil 的接口**不为 nil** ⇒ `s.images == nil`（api 的 requireImages）与
// `m.images != nil`（system.Manager 的备份/清数据分支）恒判「已接入」，随后在 nil 接收者上
// `s.mu.Lock()` panic：**不是降级，是崩**。`api.New` 里「给仓库注入配置读口」那一句
// （`opts.Images.(imageConfigSource)` → `SetConfigSource`）就会先在启动装配期炸掉。
//
// 本文件钉住两件事：
//   ① 装配层的转换（wireImageStore）对 nil 具体值交出**真 nil 接口**，可用仓库原样送达；
//   ② 端到端：拿一个「未接通的仓库」装配出真 api.Server，GET /images 必须**不崩**并回
//      503 + 「镜像仓库未接入」（这正是仓库不可用时的产品语义，不是 500/panic）。

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xzjt/nfvis/internal/aaa"
	"github.com/xzjt/nfvis/internal/api"
	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/images"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

// TestFixR51ImageStoreWiringIsNilInterface 装配层：nil 具体值 ⇒ nil 接口；非 nil ⇒ 原样送达。
func TestFixR51ImageStoreWiringIsNilInterface(t *testing.T) {
	var failed *images.Store // images.Open 失败时就是这个形状（typed-nil）

	backup, apiRT := wireImageStore(failed)
	if backup != nil {
		t.Fatalf("仓库未打开时 system.ImageStore 必须是 nil 接口，实际 %#v", backup)
	}
	if apiRT != nil {
		t.Fatalf("仓库未打开时 api.ImagesRuntime 必须是 nil 接口，实际 %#v", apiRT)
	}

	// 正控：可用仓库不得被抹成 nil（那会把正常实例的镜像功能整个关掉）。
	ok := &images.Store{}
	backup2, apiRT2 := wireImageStore(ok)
	if backup2 == nil || apiRT2 == nil {
		t.Fatalf("可用仓库不得被抹成 nil 接口: backup=%#v api=%#v", backup2, apiRT2)
	}
	if backup2.(*images.Store) != ok || apiRT2.(*images.Store) != ok {
		t.Fatal("可用仓库应原样送达（同一个实例）")
	}
}

// TestFixR51UnavailableImageRepoAnswers503 端到端（生产同形的 api.Server）：仓库未接通 ⇒ 503。
//
// 修复前该用例在 `api.New` 内部 panic（typed-nil 的 SetConfigSource → nil 接收者上取锁），
// 即「镜像仓库初始化失败」会让整个 nfvisd 起不来；修复后是既有降级语义：服务起得来、
// 镜像端点如实报 503、其余端点不受影响。
func TestFixR51UnavailableImageRepoAnswers503(t *testing.T) {
	const pw = "FixR51-Passw0rd!"
	store, err := config.OpenStore(filepath.Join(t.TempDir(), "nfvis.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	engine, err := config.NewEngine(store, orchestrator.NewNoopApplier(), config.Options{})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(engine.Close)
	authz := aaa.NewService(engine, nil)
	if _, _, err := aaa.EnsureBootstrapAdmin(engine, authz, pw); err != nil {
		t.Fatalf("引导 admin: %v", err)
	}

	// 生产路径同一个转换：仓库未打开（typed-nil）交给装配层。
	_, imageRT := wireImageStore(nil)
	srv := api.New(engine, authz, api.Options{
		Addr: ":0", Log: slog.New(slog.DiscardHandler), Images: imageRT,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// 登录（读端点仍需凭据）。
	body, _ := json.Marshal(map[string]string{"username": "admin", "password": pw})
	resp, err := http.Post(ts.URL+api.APIPrefix+"/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	var login struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&login)
	if login.Token == "" {
		t.Fatalf("登录未取到 token（status=%d）", resp.StatusCode)
	}

	req, err := http.NewRequest(http.MethodGet, ts.URL+api.APIPrefix+"/images", nil)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+login.Token)
	got, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /images: %v", err)
	}
	defer got.Body.Close()
	raw, _ := io.ReadAll(got.Body)
	if got.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("仓库未接入应回 503，实际 %d：%s", got.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "镜像仓库未接入") {
		t.Fatalf("503 正文应说明镜像仓库未接入，实际：%s", raw)
	}

	// 其余端点不受影响（仓库不可用不该把整个管理面拖下水）。
	req2, _ := http.NewRequest(http.MethodGet, ts.URL+api.APIPrefix+"/system/version", nil)
	req2.Header.Set("Authorization", "Bearer "+login.Token)
	got2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("GET /system/version: %v", err)
	}
	defer got2.Body.Close()
	if got2.StatusCode != http.StatusOK {
		t.Fatalf("仓库不可用不应影响其它端点：/system/version 回 %d", got2.StatusCode)
	}
}
