package images

// round49（镜像名越界收口）：镜像名是**用户输入**，而它被直接拼进仓库目录路径（导入/拉取/删除三处），
// 故必须有类型感知的名称校验 + 落盘路径的仓库前缀复核（纵深防御）。
// 本文件锁定校验函数的判据与三处落盘的越界拒绝。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 非法名一律拒绝：空/空白、控制字符、绝对路径、任何 `..` 段；VM 镜像另拒绝
// 路径/冒号类字符（文件名语义），容器镜像保留 Docker 引用语义。
func TestValidateImageNameRejects(t *testing.T) {
	cases := []struct {
		why  string
		name string
		typ  string
	}{
		{"空", "", TypeVM},
		{"全空白", "   ", TypeVM},
		{"上一级", "..", TypeVM},
		{"穿越（相对）", "../../etc/passwd", TypeVM},
		{"穿越（带扩展名）", "../evil.qcow2", TypeVM},
		{"先下潜再回退", "a/../../evil.qcow2", TypeVM},
		{"子目录（VM 不允许）", "a/b.qcow2", TypeVM},
		{"反斜杠（VM 不允许）", `a\b.qcow2`, TypeVM},
		{"冒号（VM 不允许）", "C:evil.qcow2", TypeVM},
		{"绝对路径（POSIX）", "/etc/passwd", TypeVM},
		{"绝对路径（Windows）", `C:\Windows\evil.qcow2`, TypeVM},
		{"制表/换行等控制字符", "evil\tqcow2.qcow2", TypeVM},
		{"NUL", "evil\x00.qcow2", TypeVM},
		{"ESC", "evil\x1b.qcow2", TypeVM},

		{"容器：空", "", TypeContainer},
		{"容器：上一级", "..", TypeContainer},
		{"容器：穿越段", "library/../../evil", TypeContainer},
		{"容器：绝对路径", "/library/alpine", TypeContainer},
		{"容器：尾随斜杠", "library/alpine/", TypeContainer},
		{"容器：空段", "library//alpine", TypeContainer},
		{"容器：反斜杠", `library\alpine`, TypeContainer},
		{"容器：空白", "alpine 3.20", TypeContainer},
		{"容器：控制字符", "alpine\x01:3.20", TypeContainer},
		{"容器：制表符", "alpine\t3.20", TypeContainer},

		{"未知类型按最严（VM）判", "../../etc/passwd", "bad-type"},
	}
	for _, c := range cases {
		t.Run(c.why, func(t *testing.T) {
			if err := ValidateImageName(c.name, c.typ); err == nil {
				t.Fatalf("应拒绝 %q（%s），实际通过了", c.name, c.typ)
			}
		})
	}
}

// 合法名必须照旧通过（否则会误伤既有用法）：VM 文件名、容器 Docker 引用。
func TestValidateImageNameAccepts(t *testing.T) {
	cases := []struct {
		name string
		typ  string
	}{
		{"alpine.qcow2", TypeVM},
		{"ubuntu-22.04-server.qcow2", TypeVM},
		{"debian12.img", TypeVM},
		{"img_a.iso", TypeVM},
		{"alpine:3.20", TypeContainer},
		{"busybox", TypeContainer},
		{"library/alpine:3.20", TypeContainer},
		{"registry.example.com/ns/img:1.0", TypeContainer},
		{"registry.example.com/ns/img@sha256:abcdef", TypeContainer},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := ValidateImageName(c.name, c.typ); err != nil {
				t.Fatalf("合法名 %q（%s）不应被拒: %v", c.name, c.typ, err)
			}
		})
	}
}

// 越界名不得动到仓库**外**的文件：导入（os.Rename 目的地）与删除（os.Remove）都曾
// 直接用 filepath.Join(Dir, name)，`../` 一穿就是仓库外的任意文件。
func TestOutOfRepoNameCannotTouchOutsideFile(t *testing.T) {
	s := newStore(t)
	repo := s.Config().Dir
	outside := filepath.Join(filepath.Dir(repo), "outside.qcow2") // 仓库的**同级**文件
	if err := os.WriteFile(outside, []byte("MUST-STAY"), 0o644); err != nil {
		t.Fatal(err)
	}
	escape := "../outside.qcow2"

	// ① 导入：源文件在 incoming 内合法，但**目标名**越界
	src := filepath.Join(s.Config().IncomingDir, "upload.qcow2")
	if err := os.WriteFile(src, []byte("EVIL"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportIncoming(escape, TypeVM, src, ""); err == nil {
		t.Fatal("越界镜像名导入应被拒绝")
	}
	assertUntouched(t, outside, "导入")
	if _, err := os.Stat(src); err != nil {
		t.Errorf("被拒绝的导入不应动到 incoming 源文件: %v", err)
	}

	// ② 删除：模拟历史遗留的越界索引条目（修复前 Delete 会照删仓库外文件）
	if err := s.setMeta(Meta{Name: escape, Type: TypeVM, ImportState: StateReady}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(escape, 0); err == nil {
		t.Fatal("越界索引条目的删除应被拒绝")
	}
	assertUntouched(t, outside, "删除")

	// ③ 路径解析：越界条目不给出可用路径（Path 是克隆源，指到仓库外就是读任意文件）
	if got := s.Path(escape); got != "" {
		t.Fatalf("越界条目不应给出仓库外路径: %q", got)
	}
}

// 越界名不得在仓库**外**写盘：URL 拉取曾 dest=Join(Dir,name) 并 MkdirAll。
func TestOutOfRepoNameDownloadRejected(t *testing.T) {
	s := newStore(t)
	repo := s.Config().Dir
	outside := filepath.Join(filepath.Dir(repo), "pulled.qcow2")
	if err := os.WriteFile(outside, []byte("MUST-STAY"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := s.Download(context.Background(), DownloadOptions{
		Name: "../pulled.qcow2", Type: TypeVM,
		URL: "http://127.0.0.1:1/never.qcow2", SHA256: strings.Repeat("a", 64),
	})
	if err == nil {
		t.Fatal("越界镜像名的 URL 拉取应被拒绝")
	}
	assertUntouched(t, outside, "URL 拉取")
	if _, ok := s.Get("../pulled.qcow2"); ok {
		t.Error("被拒绝的拉取不应登记镜像条目")
	}
	if err := ValidateDownloadOptions(DownloadOptions{
		Name: "../pulled.qcow2", Type: TypeVM,
		URL: "http://127.0.0.1:1/never.qcow2", SHA256: strings.Repeat("a", 64),
	}); err == nil {
		t.Fatal("ValidateDownloadOptions 应拒绝越界名（调用方受理前同步校验）")
	}
}

// 容器镜像的归档**不能**按名字落盘：`library/alpine:3.20` 这样的合法 Docker 引用含 `/`，
// 直接用名字当文件名会在仓库里建子目录、并让 .part/清理路径跟着名字走。
// 落盘必须用仓库内的安全临时名，且导入后清掉。
func TestContainerDownloadUsesSafeTempName(t *testing.T) {
	const payload = "docker-archive-bytes"
	s := newStore(t)
	repo := s.Config().Dir
	srv := serveOnce(t, []byte(payload))

	var loadedPath string
	s.SetDockerLoader(func(path, name string) error { loadedPath = path; return nil })

	m, err := s.Download(context.Background(), DownloadOptions{
		Name: "library/alpine:3.20", Type: TypeContainer, URL: srv, SHA256: sha256Hex([]byte(payload)),
	})
	if err != nil {
		t.Fatalf("容器镜像拉取: %v", err)
	}
	if m.ImportState != StateReady {
		t.Fatalf("容器镜像应 ready: %+v", m)
	}
	if loadedPath == "" {
		t.Fatal("应经 docker load 载入归档")
	}
	repoAbs, err := filepath.Abs(repo)
	if err != nil {
		t.Fatal(err)
	}
	gotAbs, err := filepath.Abs(loadedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(gotAbs, repoAbs+string(filepath.Separator)) {
		t.Fatalf("归档临时文件应在仓库内: %s", gotAbs)
	}
	if filepath.Base(gotAbs) == "library/alpine:3.20" || strings.Contains(filepath.Base(gotAbs), "/") {
		t.Fatalf("归档不应按原始名字落盘: %s", gotAbs)
	}
	if _, err := os.Stat(filepath.Join(repo, "library")); !os.IsNotExist(err) {
		t.Errorf("不应按名字在仓库里建子目录: %v", err)
	}
	if _, err := os.Stat(gotAbs); !os.IsNotExist(err) {
		t.Errorf("load 成功后应清掉临时归档: %v", err)
	}
	if _, ok := s.Get("library/alpine:3.20"); !ok {
		t.Error("应登记容器镜像条目（名字即 Docker 引用）")
	}
}

func assertUntouched(t *testing.T, path, stage string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s 动了仓库外文件（读不到即被删/改名）: %v", stage, err)
	}
	if string(b) != "MUST-STAY" {
		t.Fatalf("%s 改了仓库外文件内容: %q", stage, b)
	}
}

// serveOnce 起一个只服务 payload 的测试服务器，返回其 URL。
func serveOnce(t *testing.T, payload []byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "archive.tar", srvTime(), strings.NewReader(string(payload)))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}
