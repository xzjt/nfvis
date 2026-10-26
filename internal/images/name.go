package images

// round49（镜像名越界收口）：镜像名的校验（类型感知）。
//
// 为什么必须有：镜像名是**用户输入**（CLI/REST 的 name 字段），而它被直接拼进仓库目录
// 路径——导入是 `os.Rename(abs, filepath.Join(Dir, name))`、URL 拉取是
// `dest = filepath.Join(Dir, name)` + `MkdirAll`、删除是 `os.Remove(filepath.Join(Dir, name))`。
// 于是 `../../etc/xxx` 这类名字能把这三件事做到仓库**外面**去（覆盖或删除任意文件）。
//
// 校验分两层，两层都要有：**入口**（导入/拉取/删除三处各调 ValidateImageName）与
// **落盘路径**（containedPath 复核前缀，见 images.go / download.go）。任何一层漏了，
// 另一层仍然挡得住——这是纵深防御，不是重复。

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrInvalidName 镜像名不合法（入口校验与落盘路径复核共用）。
// 独立错误值而非裸 fmt.Errorf：api 层据此把「名字不合法」映射成 400（而不是 500）。
var ErrInvalidName = errors.New("镜像名不合法")

// ValidateImageName 校验镜像名（类型感知）。
//
// 两种类型都拒绝：空/全空白、含控制字符或非法 UTF-8、绝对路径、任何 `..` 路径段。
//
// VM 镜像（TypeVM）的名字就是仓库里的**文件名**，故另拒绝 `/`、`\`、`:`（路径分隔符与
// Windows 盘符在跨平台下都是逃逸媒介），并要求 name == filepath.Base(name)。
//
// 容器镜像（TypeContainer）的名字是 **Docker 引用**，`:`、`/`、`@` 是它的语法
// （`alpine:3.20`、`library/alpine:3.20`、`registry.example.com/ns/img@sha256:…`），
// 故只拒绝首尾 `/`、空段（`a//b`）、反斜杠、空白与控制字符。
//
// 未知类型按**最严**（VM）判——宁可误拒，不可误放。
func ValidateImageName(name, typ string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w：不能为空", ErrInvalidName)
	}
	for _, r := range name {
		if r == utf8.RuneError || unicode.IsControl(r) {
			return fmt.Errorf("%w：%q 含控制字符或非法字符", ErrInvalidName, name)
		}
	}
	if filepath.IsAbs(name) || strings.HasPrefix(name, "/") || strings.HasPrefix(name, `\`) {
		return fmt.Errorf("%w：%q 不能是绝对路径（只写镜像名，不带目录）", ErrInvalidName, name)
	}
	if hasDotDotSegment(name) {
		return fmt.Errorf("%w：%q 含 `..` 路径段（不允许引出上级目录）", ErrInvalidName, name)
	}
	if typ == TypeContainer {
		if strings.ContainsRune(name, '\\') {
			return fmt.Errorf("%w：容器镜像名 %q 不能含 `\\`（Docker 引用用 `/` 分层）", ErrInvalidName, name)
		}
		if strings.ContainsRune(name, ' ') {
			return fmt.Errorf("%w：容器镜像名 %q 不能含空白", ErrInvalidName, name)
		}
		for _, seg := range strings.Split(name, "/") {
			if seg == "" {
				return fmt.Errorf("%w：容器镜像名 %q 含空的路径段（不能以 `/` 开头/结尾，也不能有 `//`）", ErrInvalidName, name)
			}
		}
		return nil
	}
	// VM 镜像（含未知类型：按最严判）。
	if strings.ContainsAny(name, `/\:`) {
		return fmt.Errorf("%w：VM 镜像名 %q 不能含 `/`、`\\`、`:`"+
			"（它同时是仓库里的文件名；请只写文件名，如 alpine.qcow2）", ErrInvalidName, name)
	}
	if name != filepath.Base(name) {
		return fmt.Errorf("%w：VM 镜像名 %q 不是纯文件名", ErrInvalidName, name)
	}
	return nil
}

// hasDotDotSegment 判定名字是否含 `..` 路径段（`/` 与 `\` 都算分隔符）。
// 只判**整段**等于 `..`：`a..b.qcow2` 这类普通文件名不受影响。
func hasDotDotSegment(name string) bool {
	segs := strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == '\\' })
	for _, s := range segs {
		if s == ".." {
			return true
		}
	}
	return false
}
