---
title: "Kata, pengujian, dan publikasi"
description: "Menerjemahkan apa yang diucapkan komponen, mengujinya seperti pengguna, dan menawarkannya ke aplikasi Go serta TSX."
group: Menulis komponen
order: 74
---

## Kata-kata

Komponen mengucapkan kata-katanya sendiri—seperti nama tombol untuk pembaca layar—melalui katalog paketnya alih-alih string literal. Taruh paket `locales/` di dalam pustaka beserta katalog bahasa sumbernya, dan sebuah file yang menghasilkan kodenya:

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

`go generate ./locales` menulis satu fungsi per pesan, yang dipanggil oleh komponen bersama cakupannya: `locales.Clear(cx)` mengucapkannya dalam bahasa yang sedang ditampilkan aplikasi. Komit file `catalog_gen.go` yang dihasilkan, sehingga aplikasi dapat dibangun dengan Go saja. Tambahkan satu file untuk setiap bahasa tujuan terjemahan (`zh-Hans.yaml`); aplikasi dapat menyediakan bahasa lainnya dalam katalognya sendiri, di bawah kunci yang menamai jalur impor paket Anda, seperti untuk [kata-kata milik NSGo sendiri](../localization.md#kata-kata-milik-nsgo).

## Pengujian

`nsgotest` menjalankan komponen seperti cara pengguna menggunakannya, tanpa antarmuka grafis (*headless*). Pasang di dalam root kecil dan operasikan berdasarkan peran serta teksnya:

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

Dari root pustaka, `go tool nsgo test` menguji setiap paket, dan `-target macos` atau target lainnya menjalankan pengujian yang sama di jendela native. Uji kedua tampilan (terang dan gelap) serta pseudo-lokal (`-lang en-XA`) untuk menemukan warna dan kata yang di-hardcode. [Pengujian](../testing.md) membahas sisanya.

## Publikasi

Pustaka komponen adalah sebuah modul Go: aplikasi melakukan `go get` dan mengimpor paket-paketnya. Pisahkan bentuk tanpa gaya dan bentuk bergaya ke dalam paket terpisah jika keduanya tersedia, sehingga aplikasi dengan tampilannya sendiri tidak mengimpor gaya yang tidak digunakannya.

Halaman [Komponen](https://nsgo.dev/components/) di situs ini mencantumkan pustaka yang dapat digunakan orang lain; tambahkan dokumen dan tangkapan layar ke `docs/public/components/` untuk menawarkan pustaka Anda.

Aplikasi TSX dapat mengakses komponen Go setelah didaftarkan, di dalam fungsi `init`:

```go
func init() {
	tsx.Register("example.com/chips", map[string]core.Node{"Toggle": Toggle{}})
}
```

`import { Toggle } from "example.com/chips"` kemudian menerima field-fieldnya sebagai props ([Aplikasi TSX](../tsx.md#go-dari-tsx)).

## nsgo-module.yaml

Sebuah pustaka memberi tahu aplikasi yang menambahkannya apa yang perlu mereka ketahui lewat `nsgo-module.yaml`, file opsional di samping `go.mod` pada akar modul, yang ditampilkan [`nsgo add`](../building.md#menambahkan-pustaka) dalam rencananya sebelum menulis apa pun:

```yaml
version: 1
import: [player]            # paket yang diimpor aplikasi, relatif terhadap akar modul; . adalah akar
targets: [macos, windows_x64, linux_x64, ios, ios_simulator]
notes: |
  Pustaka FFmpeg berlisensi LGPL 2.1: sediakan kode sumbernya di samping unduhan Anda, cantumkan FFmpeg, dan izinkan penggantiannya.
components:                 # paket yang ditawarkan modul untuk disalin
  chat: ui/chat
```

- `import` menyebutkan paket yang diimpor aplikasi, masing-masing paket milik modul ini, bukan milik modul yang bersarang di dalamnya, dan bukan paket `main`. Rencana mencetak satu baris import untuk masing-masing; tanpa kolom ini, atau tanpa file-nya, rencana mencetak baris paket akar.
- `targets` menerima nama dan grup target seperti `build.targets`. Rencana menyebutkan target aplikasi yang tidak ada dalam daftar, dan `nsgo check -target` memperingatkan setiap modul yang ditautkan aplikasi yang daftarnya tidak memuat target yang diperiksanya. Tanpa kolom ini, keduanya tidak mengatakan apa pun tentang modul itu.
- `notes`, teks biasa paling banyak 4 KiB tanpa karakter kontrol atau format selain pemisah baris, menutup rencana di bawah `From <module>, the author's notes:`, dengan setiap baris ditandai `|`: tempat untuk apa yang menjadi kewajiban aplikasi, seperti kewajiban sebuah lisensi.
- `components` memetakan nama berupa 1 sampai 64 huruf kecil ASCII, angka, dan tanda hubung, yang tidak diawali atau diakhiri tanda hubung, ke direktori paket di bawah akar modul. Setiap direktori berisi paket milik modul ini yang bukan paket `main`, dan berakhir dengan nama paketnya. Rencana mencantumkan setiap komponen dengan `nsgo add <module>#<name>`; nsgo belum menyalin komponen dan untuk sementara menolak perintah itu.

File ini ketat: satu dokumen YAML paling besar 1 MiB, versi 1, tanpa anchor, alias, atau kolom yang tidak dikenal nsgo, dan `nsgo add` menolak modul yang file-nya tidak dapat diterimanya, dengan atau tanpa `-y`. Kolom baru ditambahkan di dalam versi 1, sehingga modul yang memakainya membutuhkan NSGo yang dapat membacanya, dan nsgo yang lebih lama menyebutkan `go get github.com/nsgo-dev/nsgo@<versi>` yang membawa NSGo tersebut. `nsgo build` tidak pernah membaca file ini.
