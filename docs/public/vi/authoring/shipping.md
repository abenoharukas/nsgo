---
title: Từ ngữ, kiểm thử và xuất bản
description: Dịch nội dung hiển thị của thành phần, kiểm thử như người dùng thực thụ, và cung cấp cho ứng dụng Go lẫn TSX.
group: Viết thành phần
order: 74
---

## Từ ngữ

Thành phần tự hiển thị các từ ngữ của riêng mình — chẳng hạn tên nút bấm cho trình đọc màn hình — thông qua các danh mục (catalog) của gói thay vì chuỗi ký tự viết cứng. Hãy đặt một gói `locales/` trong thư viện cùng danh mục của ngôn ngữ nguồn và một tệp sinh mã nguồn:

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

`go generate ./locales` sinh ra một hàm cho mỗi thông điệp, và thành phần chỉ cần gọi hàm đó kèm scope của mình: `locales.Clear(cx)` sẽ đọc lên nội dung theo đúng ngôn ngữ ứng dụng đang hiển thị. Hãy commit tệp `catalog_gen.go` đã sinh để các ứng dụng có thể build chỉ với Go. Thêm một tệp cho mỗi ngôn ngữ bạn dịch sang (`zh-Hans.yaml`); ứng dụng có thể bổ sung các ngôn ngữ khác trong danh mục riêng của nó, dưới một khóa mang đường dẫn import của gói bạn, giống như với [từ ngữ riêng của NSGo](../localization.md#văn-bản-riêng-của-nsgo).

## Kiểm thử

`nsgotest` chạy thành phần đúng theo cách người dùng thao tác, ở chế độ không giao diện (headless). Hãy gắn thành phần vào một gốc nhỏ và tương tác theo vai trò và văn bản:

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

Từ thư mục gốc của thư viện, `go tool nsgo test` chạy toàn bộ các gói, và `-target macos` hoặc một nền tảng đích khác chạy chính các bài kiểm thử đó trong cửa sổ gốc. Hãy kiểm thử cả hai giao diện sáng/tối và một ngôn ngữ giả (`-lang en-XA`) để phát hiện sớm màu sắc hoặc từ ngữ viết cứng. [Kiểm thử](../testing.md) trình bày phần còn lại.

## Xuất bản

Một thư viện thành phần là một Go module: các ứng dụng `go get` thư viện đó và import các gói của nó. Hãy tách riêng dạng không kiểu dáng và dạng có kiểu dáng thành hai gói khác nhau khi cả hai cùng tồn tại, để một ứng dụng có giao diện riêng không phải kéo theo những kiểu dáng mà nó không sử dụng.

Trang [Thành phần](https://nsgo.dev/components/) trên website liệt kê các thư viện để mọi người cùng dùng; hãy thêm một tài liệu và ảnh chụp màn hình vào `docs/public/components/` để giới thiệu thư viện của bạn.

Ứng dụng TSX có thể gọi tới các thành phần Go sau khi chúng được đăng ký trong hàm `init`:

```go
func init() {
	tsx.Register("example.com/chips", map[string]core.Node{"Toggle": Toggle{}})
}
```

Khi đó `import { Toggle } from "example.com/chips"` sẽ nhận các trường cấu trúc dưới dạng props ([Ứng dụng TSX](../tsx.md#go-từ-tsx)).

## nsgo-module.yaml

Một thư viện cho các ứng dụng thêm nó biết những điều cần biết qua `nsgo-module.yaml`, một tệp tùy chọn đặt cạnh `go.mod` ở gốc module, mà [`nsgo add`](../building.md#thêm-thư-viện) hiển thị trong kế hoạch trước khi ghi bất cứ thứ gì:

```yaml
version: 1
import: [player]            # các gói mà ứng dụng import, tương đối với gốc module; . là gốc
targets: [macos, windows_x64, linux_x64, ios, ios_simulator]
notes: |
  Các thư viện của FFmpeg dùng giấy phép LGPL 2.1: hãy cung cấp mã nguồn của chúng bên cạnh bản tải xuống của bạn, ghi công FFmpeg và cho phép thay thế chúng.
components:                 # các gói mà module cung cấp để sao chép
  chat: ui/chat
```

- `import` liệt kê các gói mà ứng dụng import, mỗi gói là một gói của chính module này, không thuộc module lồng bên trong nó, và không phải gói `main`. Kế hoạch in một dòng import cho mỗi gói; khi không có trường này, hoặc không có tệp, nó in dòng của gói gốc.
- `targets` nhận tên và nhóm target như `build.targets`. Kế hoạch nêu các target của ứng dụng mà danh sách không có, và `nsgo check -target` cảnh báo về mỗi module mà ứng dụng liên kết có danh sách thiếu target đang được kiểm tra. Khi không có trường này, cả hai đều không nói gì về module đó.
- `notes`, văn bản thuần tối đa 4 KiB, không có ký tự điều khiển hay ký tự định dạng nào ngoài dấu xuống dòng, kết thúc kế hoạch dưới dòng `From <module>, the author's notes:`, mỗi dòng được đánh dấu `|`: đây là nơi ghi những gì ứng dụng phải làm, chẳng hạn nghĩa vụ của một giấy phép.
- `components` ánh xạ các tên gồm 1 đến 64 chữ cái ASCII thường, chữ số và dấu gạch nối, không bắt đầu hay kết thúc bằng dấu gạch nối, tới các thư mục gói bên dưới gốc module. Mỗi thư mục chứa một gói của module này không phải gói `main`, và kết thúc bằng tên gói của nó. Kế hoạch liệt kê mỗi thành phần cùng `nsgo add <module>#<name>`; nsgo chưa sao chép thành phần và hiện từ chối lệnh đó.

Tệp này nghiêm ngặt: một tài liệu YAML duy nhất tối đa 1 MiB, phiên bản 1, không có anchor, alias hay trường mà nsgo không biết, và `nsgo add` từ chối module có tệp mà nó không chấp nhận được, dù có `-y` hay không. Các trường mới được thêm trong phiên bản 1, nên module dùng một trường mới phải yêu cầu NSGo đọc được nó, và nsgo cũ hơn sẽ nêu lệnh `go get github.com/nsgo-dev/nsgo@<phiên bản>` để có NSGo đó. `nsgo build` không bao giờ đọc tệp này.
