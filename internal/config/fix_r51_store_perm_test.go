package config

// round51 G4：配置库文件（含 WAL 边车）落盘必须 0600 —— 库里存着全部登录用户的口令哈希与
// 整份 committed 配置。
//
// 同时钉住一条**有意为之的边界**：所在目录不动（生产是 /var/lib/nfvis，它同时是 VM 盘与镜像的
// 落点，QEMU 以 libvirt-qemu 运行、必须能穿越；见 hardenStoreFiles 的 WHY）。谁若日后把目录
// 收紧成 0700，这条断言会拦下他。

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := st.Mode().Perm(); got != want {
		t.Fatalf("%s 权限应为 %o，实际 %o", path, want, got)
	}
}

func TestFixR51StoreFilesAreNotWorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 无 POSIX 权限语义（Go 的 Chmod 只切只读位，Perm() 恒 0666/0444）")
	}
	for _, tc := range []struct {
		name    string
		premake bool // 预先造一个 0644 的库文件（模拟既有安装升级上来的现场）
	}{
		{"全新路径", false},
		{"既有 0644 库文件", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "nfvis")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatalf("建目录: %v", err)
			}
			db := filepath.Join(dir, "nfvis.db")
			if tc.premake {
				if err := os.WriteFile(db, nil, 0o644); err != nil {
					t.Fatalf("预置宽松库文件: %v", err)
				}
				if err := os.Chmod(db, 0o644); err != nil {
					t.Fatalf("预置权限: %v", err)
				}
			}

			s, err := OpenStore(db)
			if err != nil {
				t.Fatalf("OpenStore: %v", err)
			}
			// migrate 已写过库；再写一条以确保 WAL 边车确已落地。
			if _, err := s.AppendRevision([]byte(`{"system":{"hostname":"x"}}`), time.Now(), "t", "admin"); err != nil {
				t.Fatalf("AppendRevision: %v", err)
			}

			assertMode(t, db, 0o600)
			if _, err := os.Stat(db + "-wal"); err == nil {
				assertMode(t, db+"-wal", 0o600)
			} else {
				t.Logf("-wal 不存在（%v），跳过该断言", err)
			}
			// 目录**保持 0755**（不得收紧：QEMU 需穿越 /var/lib/nfvis 取 VM 盘/镜像）。
			assertMode(t, dir, 0o755)
			if err := s.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			// 幂等/可重复收紧：手工放宽权限后重新打开，仍应收紧到 0600。
			if err := os.Chmod(db, 0o644); err != nil {
				t.Fatalf("放宽权限: %v", err)
			}
			s2, err := OpenStore(db)
			if err != nil {
				t.Fatalf("OpenStore(重开): %v", err)
			}
			defer s2.Close()
			assertMode(t, db, 0o600)
		})
	}
}
