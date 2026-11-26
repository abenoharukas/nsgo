---
title: Sözcükler, testler ve yayınlama
description: Bir bileşenin söylediklerini çevirin, bir kullanıcı gibi test edin ve Go ile TSX uygulamalarına sunun.
group: Bileşen yazma
order: 74
---

## Sözcükler

Bir bileşen, ekran okuyucular için bir düğmenin adı gibi kendi sözcüklerini dize değişmezleri yerine paketinin katalogları aracılığıyla söyler. Kütüphaneye kaynak dilin kataloğunu içeren bir `locales/` paketi ve kodu üreten bir dosya koyun:

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

`go generate ./locales` ileti başına bir işlev yazar; bileşen bunu kendi kapsamıyla çağırır: `locales.Clear(cx)` sözcüğü uygulamanın gösterdiği dilde söyler. Uygulamaların yalnızca Go ile derlenebilmesi için üretilen `catalog_gen.go` dosyasını işleyin (commit). Çeviri yaptığınız her dil için bir dosya ekleyin (`zh-Hans.yaml`); bir uygulama ise tıpkı [NSGo'nun kendi sözcükleri](../localization.md#nsgonun-kendi-sözcükleri) için olduğu gibi, kendi kataloglarında paketinizin içe aktarma yolunu adlandıran bir anahtar altında diğer dilleri sağlar.

## Testler

`nsgotest` bir bileşeni tıpkı bir kullanıcının kullandığı şekilde, başsız (headless) olarak çalıştırır. Küçük bir köke bağlayın ve ona rol ile metne göre etki edin:

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

Kütüphanenin kökünden `go tool nsgo test` her paketi çalıştırır ve `-target macos` veya başka bir hedef aynı testleri yerel bir pencerede çalıştırır. Sabit kodlanmış renkleri ve sözcükleri yakalamak için her iki görünümü ve sözde yerel ayarı (`-lang en-XA`) test edin. Geri kalanını [Test etme](../testing.md) kapsar.

## Yayınlama

Bir bileşen kütüphanesi bir Go modülüdür: uygulamalar onu `go get` ile indirir ve paketlerini içe aktarır. Her ikisi de mevcut olduğunda stilsiz ve stilli biçimlerini ayrı paketlerde tutun; böylece kendi görünümüne sahip bir uygulama kullanmadığı stilleri içe aktarmaz.

Sitenin [Bileşenler](https://nsgo.dev/components/) sayfası başkalarının kullanabileceği kütüphaneleri listeler; sizinkini sunmak için `docs/public/components/` altına bir belge ve bir ekran görüntüsü ekleyin.

TSX uygulamaları, bir `init` işlevinde kaydedildikten sonra Go bileşenlerine ulaşır:

```go
func init() {
	tsx.Register("example.com/chips", map[string]core.Node{"Toggle": Toggle{}})
}
```

`import { Toggle } from "example.com/chips"` ardından alanları prop olarak alır ([TSX uygulamaları](../tsx.md#tsxten-go)).

## nsgo-module.yaml

Bir kütüphane, kendisini ekleyen uygulamalara bilmeleri gerekenleri `nsgo-module.yaml` ile söyler: modül kökünde `go.mod` dosyasının yanında duran isteğe bağlı bu dosyayı [`nsgo add`](../building.md#kütüphane-ekleme), hiçbir şey yazmadan önce planında gösterir:

```yaml
version: 1
import: [player]            # uygulamaların içe aktardığı paketler, modül köküne göre; . köktür
targets: [macos, windows_x64, linux_x64, ios, ios_simulator]
notes: |
  FFmpeg kütüphaneleri LGPL 2.1 lisanslıdır: kaynak kodlarını indirmelerinizin yanında sunun, FFmpeg'i belirtin ve değiştirilmelerine izin verin.
components:                 # modülün kopyalanmak üzere sunduğu paketler
  chat: ui/chat
```

- `import`, uygulamaların içe aktardığı paketleri sayar; her biri bu modülün bir paketidir, içine yerleştirilmiş bir modülün değildir ve `main` paketi değildir. Plan her biri için bir import satırı yazdırır; alan ya da dosya yoksa kök paketin satırını yazdırır.
- `targets`, `build.targets` gibi hedef adlarını ve gruplarını alır. Plan, uygulamanın hedeflerinden listede olmayanları belirtir; `nsgo check -target` ise uygulamanın bağladığı ve listesinde denetlediği hedef bulunmayan her modül için uyarır. Alan yoksa ikisi de modül hakkında bir şey söylemez.
- `notes`, satır sonları dışında denetim ya da biçim karakteri içermeyen en fazla 4 KiB düz metindir; planı `From <module>, the author's notes:` satırının altında, her satırı `|` ile işaretlenmiş olarak bitirir: bir lisansın yükümlülükleri gibi uygulamanın borçlu olduğu şeylerin yeridir.
- `components`, 1 ile 64 arası küçük ASCII harf, rakam ve kısa çizgiden oluşan, kısa çizgiyle başlamayan ve bitmeyen adları modül kökünün altındaki paket dizinlerine eşler. Her dizin bu modülün `main` olmayan bir paketini içerir ve paketinin adıyla biter. Plan her bileşeni `nsgo add <module>#<name>` ile listeler; nsgo henüz bileşen kopyalamaz ve şimdilik bu komutu reddeder.

Dosya katıdır: en fazla 1 MiB'lık tek bir YAML belgesi, sürüm 1, çapa, takma ad ya da nsgo'nun bilmediği alanlar olmadan; `nsgo add`, dosyasını kabul edemediği bir modülü `-y` olsa da olmasa da reddeder. Yeni alanlar sürüm 1'in içine eklenir; bu yüzden bunlardan birini kullanan bir modül onu okuyan NSGo'yu gerektirir ve daha eski bir nsgo, o NSGo'yu getiren `go get github.com/nsgo-dev/nsgo@<sürüm>` komutunu söyler. `nsgo build` bu dosyayı hiç okumaz.
