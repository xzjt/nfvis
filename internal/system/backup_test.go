package system

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/xzjt/nfvis/internal/config"
	"github.com/xzjt/nfvis/internal/images"
	"github.com/xzjt/nfvis/internal/model"
	"github.com/xzjt/nfvis/internal/orchestrator"
)

type fakeImages struct {
	metas   []images.Meta
	deleted []string
}

func (f *fakeImages) List() []images.Meta { return append([]images.Meta(nil), f.metas...) }
func (f *fakeImages) Delete(name string, _ int) error {
	f.deleted = append(f.deleted, name)
	return nil
}

func newTestManager(t *testing.T, imgs ImageStore) (*Manager, *config.Engine) {
	t.Helper()
	store, err := config.OpenStore(filepath.Join(t.TempDir(), "nfvis.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	eng, err := config.NewEngine(store, orchestrator.NewNoopApplier(), config.Options{})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(eng.Close)
	m := NewManager(Config{Dir: filepath.Join(t.TempDir(), "backup")}, eng, imgs, "test-1.0")
	return m, eng
}

func seedConfig(t *testing.T, eng *config.Engine, cfg model.Config) {
	t.Helper()
	sess := config.Session{User: "admin", Source: "test"}
	if err := eng.Edit(sess); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if err := eng.UpdateCandidate(sess, cfg); err != nil {
		t.Fatalf("UpdateCandidate: %v", err)
	}
	if verrs, err := eng.CommitCheck(sess); err != nil {
		t.Fatalf("CommitCheck: %v", err)
	} else if len(verrs) > 0 {
		t.Fatalf("seed 配置不合法: %+v", verrs)
	}
	if _, err := eng.Commit(context.Background(), sess, config.CommitOpts{Message: "seed"}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := eng.Release(sess); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestBackupAndList(t *testing.T) {
	imgs := &fakeImages{metas: []images.Meta{{Name: "alpine.qcow2", Type: images.TypeVM, SizeBytes: 10}}}
	m, eng := newTestManager(t, imgs)
	seedConfig(t, eng, model.Config{
		System:     &model.SystemConfig{Hostname: "node1"},
		Interfaces: []model.InterfaceConfig{{Name: "ens2f0", Description: "to-tor"}},
	})

	f, err := m.Backup()
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if f.Kind != "config-backup" || f.SizeBytes <= 0 {
		t.Fatalf("备份元数据: %+v", f)
	}
	path, err := m.Path(f.File)
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var arch Archive
	if err := json.Unmarshal(data, &arch); err != nil {
		t.Fatalf("解析归档: %v", err)
	}
	if arch.Format != Format || arch.ArchiveVer != ArchiveVersion || arch.Version != "test-1.0" {
		t.Fatalf("归档头: %+v", arch)
	}
	if arch.Config.System == nil || arch.Config.System.Hostname != "node1" {
		t.Fatalf("归档应含 committed 配置: %+v", arch.Config)
	}
	// 镜像仅清单（元数据），不含文件本体（FR-OPS-006）
	if len(arch.Images) != 1 || arch.Images[0].Name != "alpine.qcow2" {
		t.Fatalf("归档应含镜像清单: %+v", arch.Images)
	}
	if list := m.List(); len(list) != 1 || list[0].File != f.File {
		t.Fatalf("列表: %+v", list)
	}
}

func TestPathTraversalRejected(t *testing.T) {
	m, _ := newTestManager(t, nil)
	for _, bad := range []string{"../etc/passwd", "sub/dir.json", "", "a\\b.json", "..\\x"} {
		if _, err := m.Path(bad); err == nil {
			t.Fatalf("非法路径应拒绝: %q", bad)
		}
	}
}

func TestRestoreAppliesConfigAndReturnsImageManifest(t *testing.T) {
	m, eng := newTestManager(t, nil)
	seedConfig(t, eng, model.Config{System: &model.SystemConfig{Hostname: "before"}})
	arch := Archive{
		Format: Format, ArchiveVer: ArchiveVersion, Version: "test-1.0",
		Config: model.Config{System: &model.SystemConfig{Hostname: "restored"}},
		Images: []images.Meta{{Name: "img.qcow2", Type: images.TypeVM}},
	}
	data, _ := json.Marshal(arch)

	res, manifest, err := m.Restore(context.Background(), data, "admin")
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.Revision <= 1 {
		t.Fatalf("恢复应产生新修订: %+v", res)
	}
	if len(manifest) != 1 || manifest[0].Name != "img.qcow2" {
		t.Fatalf("应返回镜像清单: %+v", manifest)
	}
	got, _ := eng.Committed()
	if got.System == nil || got.System.Hostname != "restored" {
		t.Fatalf("恢复后配置不符: %+v", got.System)
	}
}

func TestRestoreRejectsBadArchives(t *testing.T) {
	m, _ := newTestManager(t, nil)
	if _, _, err := m.Restore(context.Background(), []byte("not json"), "admin"); err == nil {
		t.Fatal("非法 JSON 应拒绝")
	}
	bad, _ := json.Marshal(Archive{Format: "other", ArchiveVer: 1})
	if _, _, err := m.Restore(context.Background(), bad, "admin"); err == nil {
		t.Fatal("非 nfvis 归档应拒绝")
	}
	future, _ := json.Marshal(Archive{Format: Format, ArchiveVer: ArchiveVersion + 1})
	if _, _, err := m.Restore(context.Background(), future, "admin"); err == nil {
		t.Fatal("高版本归档应拒绝")
	}
}

func TestZeroizeClearsConfigAndImages(t *testing.T) {
	imgs := &fakeImages{metas: []images.Meta{
		{Name: "a.qcow2", Type: images.TypeVM},
		{Name: "alpine:3.20", Type: images.TypeContainer},
	}}
	m, eng := newTestManager(t, imgs)
	seedConfig(t, eng, model.Config{
		System:     &model.SystemConfig{Hostname: "node1"},
		Interfaces: []model.InterfaceConfig{{Name: "ens2f0"}},
	})

	res, err := m.Zeroize(context.Background(), "admin")
	if err != nil {
		t.Fatalf("Zeroize: %v", err)
	}
	if res.RemovedImages != 2 || len(imgs.deleted) != 2 {
		t.Fatalf("应删除全部镜像: %+v %v", res, imgs.deleted)
	}
	got, _ := eng.Committed()
	if len(got.Interfaces) != 0 || got.System != nil {
		t.Fatalf("恢复出厂后配置应为空: %+v", got)
	}
}
