---
title: Textos, pruebas y publicación
description: Traduce los textos de un componente, pruébalo como lo haría un usuario y ofrécelo a aplicaciones Go y TSX.
group: Escribir componentes
order: 74
---

## Textos

Un componente expresa sus propios textos, como el nombre de un botón para lectores de pantalla, a través de los catálogos de su paquete en lugar de usar cadenas literales. Incluye un paquete `locales/` en la biblioteca con el catálogo del idioma de origen y un archivo que genere el código:

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

`go generate ./locales` genera una función por mensaje, que el componente llama con su ámbito: `locales.Clear(cx)` lo devuelve en el idioma que muestra la aplicación. Haz commit del archivo `catalog_gen.go` generado para que las aplicaciones compilen solo con Go. Añade un archivo por cada idioma al que traduzcas (`zh-Hans.yaml`); una aplicación proporciona otros en sus propios catálogos, bajo una clave que indique la ruta de importación de tu paquete, tal como ocurre con [los textos propios de NSGo](../localization.md#las-palabras-propias-de-nsgo).

## Pruebas

`nsgotest` ejecuta un componente tal como lo usaría un usuario, sin interfaz gráfica. Móntalo en una pequeña raíz y actúa sobre él por rol y texto:

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

Desde la raíz de la biblioteca, `go tool nsgo test` ejecuta todos los paquetes, y `-target macos` u otro destino ejecuta las mismas pruebas en una ventana nativa. Prueba ambas apariencias y una configuración regional simulada (`-lang en-XA`) para detectar colores y textos fijos en código. [Pruebas](../testing.md) cubre el resto.

## Publicación

Una biblioteca de componentes es un módulo Go: las aplicaciones ejecutan `go get` e importan sus paquetes. Mantén sus versiones sin estilos y con estilos en paquetes separados cuando existan ambas, para que una aplicación con diseño propio no importe estilos que no usa.

La página [Componentes](https://nsgo.dev/components/) del sitio enumera bibliotecas que otros pueden utilizar; añade un documento y una captura de pantalla en `docs/public/components/` para publicar la tuya.

Las aplicaciones TSX acceden a los componentes de Go una vez registrados en una función `init`:

```go
func init() {
	tsx.Register("example.com/chips", map[string]core.Node{"Toggle": Toggle{}})
}
```

`import { Toggle } from "example.com/chips"` toma entonces los campos como propiedades ([Aplicaciones TSX](../tsx.md#go-desde-tsx)).

## nsgo-module.yaml

Una biblioteca indica a las aplicaciones que la añaden lo que deben saber en `nsgo-module.yaml`, un archivo opcional junto a su `go.mod` en la raíz del módulo, que [`nsgo add`](../building.md#añadir-bibliotecas) muestra en su plan antes de escribir nada:

```yaml
version: 1
import: [player]            # paquetes que importan las aplicaciones, relativos a la raíz del módulo; . es la raíz
targets: [macos, windows_x64, linux_x64, ios, ios_simulator]
notes: |
  Las bibliotecas de FFmpeg son LGPL 2.1: ofrece su código fuente junto a tus descargas, da crédito a FFmpeg y permite sustituirlas.
components:                 # paquetes que el módulo ofrece para copiar
  chat: ui/chat
```

- `import` nombra los paquetes que importan las aplicaciones, cada uno un paquete de este módulo, no de un módulo anidado en él, y no un paquete `main`. El plan imprime una línea de import para cada uno; sin el campo, o sin el archivo, imprime la del paquete raíz.
- `targets` admite nombres y grupos de destinos como `build.targets`. El plan nombra los destinos de la aplicación que faltan en la lista, y `nsgo check -target` avisa de cada módulo que enlaza la aplicación cuya lista no tiene el destino que comprueba. Sin el campo, ninguno de los dos dice nada del módulo.
- `notes`, texto plano de 4 KiB como máximo sin caracteres de control ni de formato salvo saltos de línea, cierra el plan bajo `From <module>, the author's notes:`, con cada línea marcada con `|`: es el lugar para lo que debe una aplicación, como las obligaciones de una licencia.
- `components` asocia nombres de 1 a 64 letras ASCII minúsculas, dígitos y guiones, sin guion al principio ni al final, con directorios de paquetes bajo la raíz del módulo. Cada directorio contiene un paquete de este módulo que no es un paquete `main` y termina en el nombre de su paquete. El plan lista cada componente con `nsgo add <module>#<name>`; nsgo todavía no copia componentes y por ahora rechaza esa orden.

El archivo es estricto: un solo documento YAML de 1 MiB como máximo, de la versión 1, sin anclas, alias ni campos que nsgo no conozca, y `nsgo add` rechaza un módulo cuyo archivo no puede aceptar, con `-y` o sin él. Los campos nuevos se añaden a la versión 1, así que un módulo que usa uno requiere el NSGo que lo lee, y un nsgo más antiguo indica el `go get github.com/nsgo-dev/nsgo@<versión>` que trae ese NSGo. `nsgo build` nunca lee el archivo.
