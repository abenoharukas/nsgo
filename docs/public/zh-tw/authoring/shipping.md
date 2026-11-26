---
title: 文字、測試與發佈
description: 翻譯元件顯示的文字，以使用者的操作方式測試元件，並提供給 Go 與 TSX 應用程式使用。
group: 撰寫元件
order: 74
---

## 文字

元件所顯示的文字（例如螢幕閱讀器為按鈕朗讀的名稱）應透過套件的語系目錄提供，而非寫死字串常值。在函式庫中建立一個 `locales/` 套件，放入來源語言的語系目錄，以及用於產生程式碼的檔案：

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

執行 `go generate ./locales` 會為每則訊息產生對應函式，元件可傳入作用域呼叫：`locales.Clear(cx)` 便會以應用程式目前顯示的語言輸出該文字。請將產生的 `catalog_gen.go` 提交至版本庫，讓應用程式僅需 Go 工具鏈即可建置。為每個需要翻譯的語言新增對應檔案（例如 `zh-Hans.yaml`）；應用程式也可在自己的語系目錄中，以命名貴套件匯入路徑的鍵值提供其他語言翻譯，方式與 [NSGo 內建文字](../localization.md#nsgo-內建文字)相同。

## 測試

`nsgotest` 能以無頭（headless）模式模擬真實使用者的操作方式來測試元件。將元件掛載於一個小型根元件中，並透過角色與文字進行操作：

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

在函式庫根目錄執行 `go tool nsgo test` 會測試所有套件，加上 `-target macos` 或其他目標平台則會在原生視窗中執行相同的測試。請務必測試淺色與深色兩種外觀以及偽語系（`-lang en-XA`），以抓出寫死的顏色與文字。其餘細節請參閱[測試](../testing.md)。

## 發佈

元件函式庫是一個 Go 模組：應用程式透過 `go get` 下載並匯入其套件。當同時提供無樣式與有樣式版本時，請將兩者置於不同的套件中，讓自訂外觀的應用程式不會匯入多餘的樣式。

網站的[元件](https://nsgo.dev/components/)頁面收錄了可供他人使用的函式庫；只要在 `docs/public/components/` 中新增說明文件與螢幕截圖即可登錄你的函式庫。

只要在 `init` 函式中註冊元件，TSX 應用程式也能使用 Go 元件：

```go
func init() {
	tsx.Register("example.com/chips", map[string]core.Node{"Toggle": Toggle{}})
}
```

之後透過 `import { Toggle } from "example.com/chips"` 即可將元件欄位作為 props 使用（[TSX 應用程式](../tsx.md#在-tsx-中使用-go)）。

## nsgo-module.yaml

函式庫透過模組根目錄下、`go.mod` 旁邊的選用檔案 `nsgo-module.yaml`，告訴新增它的應用程式需要知道的事；[`nsgo add`](../building.md#新增函式庫) 在寫入任何東西之前，會在計畫中顯示它：

```yaml
version: 1
import: [player]            # 應用程式匯入的套件，相對於模組根目錄；. 即根目錄
targets: [macos, windows_x64, linux_x64, ios, ios_simulator]
notes: |
  FFmpeg 的函式庫採用 LGPL 2.1：請在你的下載旁提供它們的原始碼，註明 FFmpeg，並允許替換它們。
components:                 # 模組提供給應用程式複製的套件
  chat: ui/chat
```

- `import` 列出應用程式匯入的套件，每個都是這個模組自己的套件，既不屬於巢狀於其中的模組，也不是 `main` 套件。計畫為每個套件印出一行 import；沒有這個欄位或沒有這個檔案時，印出根套件的 import 行。
- `targets` 接受目標名稱與目標群組，寫法與 `build.targets` 相同。計畫會列出應用程式的目標中這份清單沒有的那些；應用程式連結的模組的清單裡沒有 `nsgo check -target` 所檢查的目標時，它會發出警告。沒有這個欄位時，兩者都不就這個模組說什麼。
- `notes` 是至多 4 KiB 的純文字，除換行外不含控制字元或格式字元。它位於計畫的最後，在 `From <module>, the author's notes:` 之下，每行以 `|` 標示：應用程式要承擔的義務，例如授權條款的要求，就寫在這裡。
- `components` 把名稱對應到模組根目錄之下的套件目錄；名稱由 1 到 64 個小寫 ASCII 字母、數字和連字號組成，首尾不能是連字號。每個目錄都是這個模組的套件，不是 `main` 套件，且目錄的最後一段就是它的套件名稱。計畫為每個元件列出 `nsgo add <module>#<name>`；nsgo 還不能複製元件，目前會拒絕這個指令。

這個檔案是嚴格的：只能有一個 YAML 文件，至多 1 MiB，版本為 1，不允許錨點、別名或 nsgo 不認識的欄位；檔案無法通過檢查的模組，`nsgo add` 一律拒絕，無論是否給出 `-y`。新欄位加在版本 1 之內，因此用到新欄位的模組要依賴能讀取它的 NSGo，較舊的 nsgo 會給出引入該 NSGo 的 `go get github.com/nsgo-dev/nsgo@<版本>`。`nsgo build` 從不讀取這個檔案。
