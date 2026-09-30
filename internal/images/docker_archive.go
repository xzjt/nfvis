package images

// docker save 归档的内嵌 tag 读取（决策 #312）。
//
// 由来：容器镜像经 `request images upload` 导入时，产品会 `docker load` 归档并按**仓库目录项名**
// 重打标签 `<名>:latest`（决策 #160），故目录项名就是配置里唯一可用的名字。但重命名此前是
// **静默**的——操作者不知道 Docker 里被打了什么标签，排障时对不上，也说不清「tar 里的
// `alpine:3.20` 到底去哪了」。本文件把归档里内嵌的 tag 如实读出来（纯函数、可单测），
// 导入方据此记入 `Image.source_tags` 并在输出/详情里回显。
//
// **口径（决策 #312）**：本函数结论用于**说明**，不作放行判据——放行与否看 `docker load`
// 是否成功（失败即报错、不登记）；因此读不到 manifest.json 时只如实记「未解析到」，不阻断导入。

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
)

// dockerSaveManifest docker save 归档内 manifest.json 的条目结构（只取 RepoTags）。
type dockerSaveManifest struct {
	RepoTags []string `json:"RepoTags"`
}

// ReadDockerArchiveTags 读取 docker save 归档（tar）里 manifest.json 的 RepoTags。
//
// 返回内嵌 tag 列表（如 ["alpine:3.20"]；同一归档可含多条，保持声明序、去重）；
// 归档存在但没有 tag（`docker save` 未打标签的镜像，RepoTags 为 null）返回空列表；
// 读不到 manifest.json（不是 docker save 产物）返回 error——调用方如实说明「未解析到」。
func ReadDockerArchiveTags(archPath string) ([]string, error) {
	f, err := os.Open(archPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadDockerArchiveTagsFrom(f)
}

// ReadDockerArchiveTagsFrom 从归档流读取内嵌 tag（便于单测直接喂最小 tar 夹具）。
func ReadDockerArchiveTagsFrom(r io.Reader) ([]string, error) {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("读取归档条目: %w", err)
		}
		// manifest.json 位于归档根；用 path.Base 容忍带目录名的实现。
		if path.Base(path.Clean(h.Name)) != "manifest.json" {
			continue
		}
		var entries []dockerSaveManifest
		// manifest.json 很小，但仍限长以防异常归档。
		if err := json.NewDecoder(io.LimitReader(tr, 4<<20)).Decode(&entries); err != nil {
			return nil, fmt.Errorf("解析 manifest.json: %w", err)
		}
		seen := map[string]bool{}
		var tags []string
		for _, e := range entries {
			for _, t := range e.RepoTags {
				if t == "" || seen[t] {
					continue
				}
				seen[t] = true
				tags = append(tags, t)
			}
		}
		return tags, nil
	}
	return nil, fmt.Errorf("归档中没有 manifest.json（不是 docker save 产物？）")
}
