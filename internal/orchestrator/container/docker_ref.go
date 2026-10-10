package container

// 容器镜像的 Docker 引用推导（决策 #447）。
//
// 由来：镜像仓库的**目录项名**既是配置里的引用（容器 `image`），也是 Docker 侧要落成的引用。
// 目录项名**允许含冒号**（`alpine:3.20` 这类用法在手册/套件里是一等公民），此时它本身就是一个
// 合法 Docker 引用——不能再无条件给它补 `:latest`：那既与「按目录项名引用」的口径不符，也会
// 抢占用户 Docker 里已有的同名 tag。本文件把「目录项名 → Docker 引用」的推导收敛为纯函数，
// load 与 remove 两侧共用同一推导，引用严格对称。

import "strings"

// DockerRefFor 由镜像仓库目录项名推导 Docker 侧引用：
//   - 含冒号（`alpine:3.20`、`reg:5000/img`、`a.b/c:1`）⇒ 原样返回（本身即合法 Docker 引用）；
//   - 不含冒号（`alpine`、`a/b`）⇒ `<名>:latest`（沿用 #160/#312 语义）。
func DockerRefFor(name string) string {
	if strings.Contains(name, ":") {
		return name
	}
	return name + ":latest"
}

// splitDockerRef 按 Docker 引用规则拆分 repo/tag：取**最后一个 `/` 之后**的冒号做 tag 分隔。
//
//	alpine:3.20    → (alpine, 3.20)
//	reg:5000/img   → (reg:5000/img, "")   // 冒号属于 registry 端口，最后一个 / 之前 ⇒ 不是 tag
//	a/b            → (a/b, "")            // 无冒号
//
// tag 为空表示引用未带 tag，调用方按 `latest` 补（即 c.tag(..., repo, "latest")）。
func splitDockerRef(ref string) (repo, tag string) {
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i], ref[i+1:]
	}
	return ref, ""
}
