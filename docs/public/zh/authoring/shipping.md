---
title: 文案、测试与发布
description: 翻译组件说的话，像用户一样测试它，并提供给 Go 和 TSX 应用。
group: 编写组件
order: 74
---

## 文案

组件自己要说的话，比如按钮给读屏器的名称，通过所在包的目录文件提供，而不是写成字符串字面量。在组件库中放一个 `locales/` 包，包含源语言的目录文件，以及一个生成代码的文件：

```yaml
# locales/en.yaml
clear:
  message: Clear
  description: What screen readers say of the button that empties a chip input.
```

```go
// Package locales says the chips' words in the language the app shows.
package locales

//go:generate go tool nsgo i18n
```

`go generate ./locales` 为每条消息生成一个函数，组件用自己的 scope 调用：`locales.Clear(cx)` 以应用显示的语言说出它。把生成的 `catalog_gen.go` 一并提交，应用只用 Go 就能构建。为每种已翻译的语言加一个文件（`zh-Hans.yaml`）；其他语言由应用在自己的目录文件中提供，键名是你的包的导入路径，与 [NSGo 自己的文案](../localization.md#nsgo-自己的文案)相同。

## 测试

`nsgotest` 以用户的方式无头运行组件。把它挂在一个小的根组件里，按角色和文字操作它：

```go
func TestMain(m *testing.M) { nsgotest.Main(m) }

func TestToggle(t *testing.T) {
	app := nsgotest.Start(t, func(cx *core.Scope) core.Node {
		on := core.NewSignal(cx, false)
		return chips.Toggle{On: on.Get, OnChange: on.Set, Label: "Bold"}
	})
	app.Press("role:switch Bold")
	if !app.Find("role:switch Bold").Props.Checked {
		t.Fatal("the toggle did not turn on")
	}
}
```

在组件库根目录运行 `nsgo test` 会测试所有包，加上 `-target macos` 或其他目标则在原生窗口中运行同样的测试。在两种外观和伪语言（`-lang en-XA`）下都测一遍，可以发现写死的颜色和文案。其余内容见[测试](../testing.md)。

## 发布

组件库就是一个 Go module：应用 `go get` 它并导入其中的包。同时有无样式和带样式两种形式时，把它们放在不同的包里，这样有自己外观的应用不会导入用不到的样式。

官网的[组件](https://nsgo.dev/components/)页列出可供他人使用的库；在 `docs/public/components/` 里加一个文档和一张截图，即可推荐你的库。

TSX 应用在 Go 组件注册后就能使用它们，注册写在 `init` 函数中：

```go
func init() {
	tsx.Register("example.com/chips", map[string]core.Node{"Toggle": Toggle{}})
}
```

之后 `import { Toggle } from "example.com/chips"` 即可把字段作为 props 使用（[TSX 应用](../tsx.md#在-tsx-中使用-go)）。

## nsgo-module.yaml

库通过模块根目录下、`go.mod` 旁边的可选文件 `nsgo-module.yaml`，告诉添加它的应用需要知道的事；[`nsgo add`](../building.md#添加库) 在写入任何东西之前，会在计划中展示它：

```yaml
version: 1
import: [player]            # 应用导入的包，相对于模块根目录；. 即根目录
targets: [macos, windows_x64, linux_x64, ios, ios_simulator]
notes: |
  FFmpeg 的库采用 LGPL 2.1：在你的下载旁边提供它们的源码，注明 FFmpeg，并允许替换它们。
components:                 # 模块提供给应用复制的包
  chat: ui/chat
```

- `import` 列出应用导入的包，每个都是这个模块自己的包，既不属于嵌套在其中的模块，也不是 `main` 包。计划为每个包打印一行 import；没有这个字段或没有这个文件时，打印根包的 import 行。
- `targets` 接受目标名和目标组，写法与 `build.targets` 相同。计划会列出应用的目标中这份列表没有的那些；应用链接的模块的列表里没有 `nsgo check -target` 所检查的目标时，它会发出警告。没有这个字段时，两者都不就这个模块说什么。
- `notes` 是至多 4 KiB 的纯文本，除换行外不含控制字符或格式字符。它位于计划的最后，在 `From <module>, the author's notes:` 之下，每行以 `|` 标记：应用要承担的义务，比如许可证的要求，就写在这里。
- `components` 把名字映射到模块根目录之下的包目录；名字由 1 到 64 个小写 ASCII 字母、数字和连字符组成，首尾不能是连字符。每个目录都是这个模块的包，不是 `main` 包，且目录的最后一段就是它的包名。计划为每个组件列出 `nsgo add <module>#<name>`；nsgo 还不能复制组件，目前会拒绝这条命令。

这个文件是严格的：只能有一个 YAML 文档，至多 1 MiB，版本为 1，不允许锚点、别名或 nsgo 不认识的字段；文件不能通过检查的模块，`nsgo add` 一律拒绝，无论是否给出 `-y`。新字段加在版本 1 之内，因此用到新字段的模块要依赖能读取它的 NSGo，较旧的 nsgo 会给出引入该 NSGo 的 `go get github.com/nsgo-dev/nsgo@<版本>`。`nsgo build` 从不读取这个文件。
