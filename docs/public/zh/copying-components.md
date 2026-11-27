---
title: 复制组件
description: 使用 nsgo diff 查看已复制 Go 组件的本地修改以及上游版本变更。
group: 组件
order: 35
---

复制到应用模块中的组件会在其目录内保留一份 `nsgo-copy.yaml` 记录，记载来源模块、版本、组件名、测试选择、已复制文件和生成文件。复制完成后，组件即成为应用自己的代码。

## 使用 `nsgo diff` 对比副本

使用 `nsgo diff` 查看应用对副本的修改、另一个上游版本会带来的变更，或列出模块中所有副本的修改状态：

```sh
nsgo diff                              # 列出每个副本、版本以及是否被修改
nsgo diff chat                         # 输出本地修改相对记录版本的统一 diff
nsgo diff components/chat              # 按目录路径指定副本
nsgo diff chat@v0.5.0                  # 输出从记录版本到 v0.5.0 的统一 diff
nsgo diff chat@upgrade                 # 输出从记录版本到最新正式版的统一 diff
nsgo diff -stat chat                   # 按文件输出增删行数（也用于 Apache-2.0 第 4(b) 条标明修改文件）
nsgo diff -exit-code chat              # 存在差异时以状态码 1 退出
```

### 目标解析规则

`nsgo diff` 接受副本目录或组件名：

1. 若目录中包含 `nsgo-copy.yaml`，直接使用该目录。
2. 短名称优先与模块内的 `nsgo-copy.yaml` 记录匹配，因此离线也可用；若来自不同模块的两个副本同名，`nsgo diff` 会提示传入目录（或 `module#component`）。
3. 否则按 `nsgo add` 的方式解析名称，并按模块与组件匹配记录。

### 对比方式

- **干净基准副本。** `nsgo diff` 直接从经校验的模块 zip（通过 `zip.CheckZip` 与 `dirhash.HashZip` 核验并比对 `go.sum`）重建记录版本（或目标版本）的副本，应用相同的测试选择、祖先许可证复制、Go 导入与注释重写（其他已复制组件按其当前记录所在目录定位）以及 `go/format` 格式化；`locales/catalog_gen.go` 等生成文件不参与对比。
- **兼容 Git 的输出。** 统一 diff 使用 `diff --git` 头和相对模块根目录的路径，附带三行上下文，可直接交给 `git apply` 和 `git apply -R` 使用。二进制文件输出 `Binary files ... differ`。
- **行尾与格式。** 对比时将 CRLF 视作 LF，且经 `go/format` 格式化后与记录版本一致的 Go 文件视为未修改。
- **离线可用。** 记录版本优先从本地 Go 模块缓存（`GOMODCACHE`）读取，因此应用已获取过的版本可在 `GOPROXY=off` 下离线对比；未命中缓存时在 `GOTOOLCHAIN=local` 的仅代理环境中下载。
- **版本不可用。** 若记录的版本无法再下载，副本中的所有非生成文件均视为已修改，并输出不可用原因。
- **修改文件声明。** Apache-2.0 许可证第 4(b) 条要求标明修改过的文件，`nsgo diff -stat` 会列出所有改动文件。
- **安全约束。** 每次读取都会重新校验 `nsgo-copy.yaml` 中的路径（包含 `../form/form.go` 等越界路径会被拒绝并指出行号），所有文件读取均限制在副本目录的 `os.Root` 内，并拒绝符号链接。

## `nsgo-copy.yaml` 记录

每个复制组件目录中都有一份确定性格式、不含哈希的 `nsgo-copy.yaml`：

```yaml
# Written by nsgo add. nsgo diff compares this directory with the copy of this version.
version: 1
module: github.com/acme/kit
module_version: v1.0.0
component: field
tests: true
files:
  - LICENSE
  - field.go
  - locales/en.yaml
generated:
  - locales/catalog_gen.go
```

移动副本目录无需额外命令，`nsgo-copy.yaml` 会随之移动；`nsgo diff` 通过遍历模块查找所有副本（跳过 `vendor`、`testdata`、嵌套模块以及以 `.` 或 `_` 开头的目录）。
