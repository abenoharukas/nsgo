---
title: Mots, tests et publication
description: Traduire ce qu'un composant énonce, le tester comme un utilisateur et le proposer aux applications Go et TSX.
group: Écrire des composants
order: 74
---

## Mots

Un composant énonce ses propres textes, comme le nom d'un bouton pour les lecteurs d'écran, via les catalogues de son paquet plutôt que par des chaînes littérales. Placez un paquet `locales/` dans la bibliothèque avec le catalogue de la langue source et un fichier qui génère le code :

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

`go generate ./locales` écrit une fonction par message, que le composant appelle avec sa portée : `locales.Clear(cx)` l'énonce dans la langue affichée par l'application. Validez le fichier généré `catalog_gen.go` dans votre dépôt, afin que les applications compilent avec Go seul. Ajoutez un fichier par langue traduite (`zh-Hans.yaml`) ; une application fournit d'autres langues dans ses propres catalogues, sous une clé nommant le chemin d'import de votre paquet, comme pour les [textes propres à NSGo](../localization.md#les-textes-propres-à-nsgo).

## Tests

`nsgotest` exécute un composant comme l'utiliserait un utilisateur, en mode sans interface graphique (*headless*). Montez-le dans une petite racine et agissez dessus par rôle et par texte :

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

Depuis la racine de la bibliothèque, `go tool nsgo test` teste chaque paquet, et `-target macos` ou une autre cible exécute les mêmes tests dans une fenêtre native. Testez les deux apparences ainsi qu'une pseudo-locale (`-lang en-XA`) pour repérer les couleurs et les textes codés en dur. [Tests](../testing.md) couvre le reste.

## Publication

Une bibliothèque de composants est un module Go : les applications l'installent avec `go get` et importent ses paquets. Conservez ses formes sans style et stylisée dans des paquets séparés lorsque les deux existent, afin qu'une application dotée de sa propre apparence n'importe aucun style dont elle n'a pas besoin.

La page [Composants](https://nsgo.dev/components/) du site répertorie les bibliothèques réutilisables ; ajoutez un document et une capture d'écran dans `docs/public/components/` pour proposer la vôtre.

Les applications TSX accèdent aux composants Go une fois ceux-ci enregistrés dans une fonction `init` :

```go
func init() {
	tsx.Register("example.com/chips", map[string]core.Node{"Toggle": Toggle{}})
}
```

`import { Toggle } from "example.com/chips"` accepte ensuite les champs comme props ([Applications TSX](../tsx.md#go-depuis-tsx)).

## nsgo-module.yaml

Une bibliothèque indique aux applications qui l'ajoutent ce qu'elles doivent savoir dans `nsgo-module.yaml`, un fichier facultatif placé à côté de son `go.mod` à la racine du module, que [`nsgo add`](../building.md#ajouter-des-bibliothèques) montre dans son plan avant d'écrire quoi que ce soit :

```yaml
version: 1
import: [player]            # paquets que les applications importent, relatifs à la racine du module ; . est la racine
targets: [macos, windows_x64, linux_x64, ios, ios_simulator]
notes: |
  Les bibliothèques de FFmpeg sont sous LGPL 2.1 : proposez leur source à côté de vos téléchargements, créditez FFmpeg et permettez de les remplacer.
components:                 # paquets que le module propose de copier
  chat: ui/chat
```

- `import` nomme les paquets que les applications importent, chacun un paquet de ce module, et non d'un module imbriqué dans celui-ci, et pas un paquet `main`. Le plan affiche une ligne d'import pour chacun ; sans ce champ, ou sans le fichier, il affiche celle du paquet racine.
- `targets` prend des noms et des groupes de cibles comme `build.targets`. Le plan nomme les cibles de l'application absentes de la liste, et `nsgo check -target` avertit de chaque module lié par l'application dont la liste n'a pas la cible qu'il vérifie. Sans ce champ, ni l'un ni l'autre ne dit rien du module.
- `notes`, du texte brut d'au plus 4 Kio sans caractères de contrôle ni de mise en forme autres que les sauts de ligne, termine le plan sous `From <module>, the author's notes:`, chaque ligne marquée `|` : c'est là que se place ce que doit une application, comme les obligations d'une licence.
- `components` associe des noms de 1 à 64 lettres ASCII minuscules, chiffres et tirets, sans tiret au début ni à la fin, à des répertoires de paquets sous la racine du module. Chaque répertoire contient un paquet de ce module qui n'est pas un paquet `main`, et se termine par le nom de son paquet. Le plan liste chaque composant avec `nsgo add <module>#<name>` ; nsgo ne copie pas encore de composants et refuse cette commande pour l'instant.

Le fichier est strict : un seul document YAML d'au plus 1 Mio, en version 1, sans ancres, alias ni champs que nsgo ne connaît pas, et `nsgo add` refuse un module dont il ne peut accepter le fichier, avec ou sans `-y`. Les nouveaux champs s'ajoutent à la version 1 : un module qui en utilise un requiert le NSGo qui le lit, et un nsgo plus ancien indique le `go get github.com/nsgo-dev/nsgo@<version>` qui apporte ce NSGo. `nsgo build` ne lit jamais ce fichier.
