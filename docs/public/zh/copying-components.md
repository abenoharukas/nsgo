---
title: 复制组件
description: 将声明的 Go 组件复制到应用中、通过 nsgo-copy.yaml 记录来源，并在 nsgo add、check、build 与 dev 中诊断因升级破坏的副本。
group: 组件
order: 35
---

当应用需要直接修改某个组件而不是仅配置其参数时，复制组件会直接存放在应用模块中，并在目录内保留一份 `nsgo-copy.yaml`，记录来源模块、版本、组件名、测试选择、已复制文件与生成文件：

```yaml
# Written by nsgo add. nsgo diff compares this directory with the copy of this version.
version: 1
module: github.com/nsgo-dev/nsgo
module_version: v0.4.2
component: chat
tests: true
files:
  - LICENSE
  - NOTICE
  - chat.go
  - locales/en.yaml
generated:
  - locales/catalog_gen.go
```

移动副本目录无需额外命令，`nsgo-copy.yaml` 会随之移动。命令在模块内查找副本时会跳过 `vendor`、`testdata`、嵌套模块、符号链接以及以 `.` 或 `_` 开头的目录。每次读取 `nsgo-copy.yaml` 都会严格校验其路径规则并将路径限制在副本目录内，拒绝任何符号链接。

## 在包含副本的应用中升级 NSGo

NSGo 或组件模块在新版本中可能引入不兼容变更。副本在与复制时匹配的模块版本下能够正常编译，升级后则可能出现编译失败。`nsgo` 会在升级前后指出受影响的副本，同时避免在没有破坏编译的版本差异上令 CI 失败：

1. **升级前（`nsgo add`）。** 当 `nsgo add` 的计划移动了已有副本所导入的模块（尤其是 NSGo）时，`nsgo add` 会在不改写模块文件的前提下，用规划后的 `go.mod` 与 `go.sum` 对这些副本执行宿主机 `go vet`。凡会停止编译的副本都会在计划的 `copies` 栏中列出，附带第一条编译或 vet 错误以及 `nsgo diff <名称>@<版本>`、`nsgo add <名称>@<版本>` 命令。在 `-y` 下，会破坏副本的计划会被拒绝，防止自动化流程静默破坏副本；在终端中交互审阅计划后仍可手动确认。
2. **外部升级后（`nsgo check`）。** 当 `go.mod` 通过 `go get` 或 Dependabot 等方式在 `nsgo` 之外更新后，`nsgo check` 会在执行 vet 之前读取所有 `nsgo-copy.yaml`、校验路径与边界，并对来源模块在 `go.mod` 中已处于不同版本的副本输出警告：

   ```text
   warn components/chat is chat from github.com/nsgo-dev/nsgo v0.4.2; this module requires v0.5.0: nsgo diff chat@v0.5.0, nsgo add chat@v0.5.0
   ```

   已是最新版本的副本不会输出任何信息。如果版本变动未导致副本编译失败，`nsgo check` 仅输出该警告并正常退出（退出码 0）。
3. **编译或 vet 失败时（`nsgo check`、`nsgo build`、`nsgo dev`）。** 每当 `go vet` 或 `go build` 在某个副本包内失败（依据 Go 输出的 `# <导入路径>` 行匹配），`nsgo` 会在 `go` 命令输出之后追加同一行诊断信息，直接标明失败包对应的副本目录、来源模块与版本以及查看和更新命令。如果模块中存在两个同名副本，命令提示会自动使用副本的相对目录路径以消除歧义。
