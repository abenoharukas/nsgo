package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// kitManifest is a library's nsgo-module.yaml with every field.
const kitManifest = `version: 1
import: [player, .]
targets: [macos, windows_x64, linux_x64, ios, ios_simulator]
notes: |
  FFmpeg's libraries are LGPL 2.1: offer their source beside your downloads, credit FFmpeg and allow replacing them.

  Background playback on iOS needs apple.info.UIBackgroundModes: [audio].
components:          # packages the module offers for copying
  chat: ui/chat
  file-picker: ui/filepicker
`

func TestParseModuleManifest(t *testing.T) {
	m, err := parseModuleManifest([]byte(kitManifest))
	if err != nil {
		t.Fatal(err)
	}
	want := &moduleManifest{
		Version: 1,
		Import:  []string{"player", "."},
		Targets: []string{"macos", "windows_x64", "linux_x64", "ios", "ios_simulator"},
		Notes:   "FFmpeg's libraries are LGPL 2.1: offer their source beside your downloads, credit FFmpeg and allow replacing them.\n\nBackground playback on iOS needs apple.info.UIBackgroundModes: [audio].\n",
		Components: map[string]string{
			"chat":        "ui/chat",
			"file-picker": "ui/filepicker",
		},
		expanded: []string{"macos_arm64", "macos_x64", "windows_x64", "linux_x64", "ios", "ios_simulator"},
	}
	if !reflect.DeepEqual(m, want) {
		t.Errorf("parsed %+v\nwant %+v", m, want)
	}

	for _, valid := range []string{
		"version: 1\n",
		"version: 1\nimport: [.]\ntargets: [all]\n",
		"version: 1\ncomponents:\n  " + strings.Repeat("a", 64) + ": ui/a\n  b-2-c: x/y_z.v2\n",
		"version: 1\nnotes: \"" + strings.Repeat("x", maxModuleNotes) + "\"\n",
		// Right-to-left text and emoji are letters and symbols, not controls.
		"version: 1\nnotes: \"مرحبا, 你好 🎬\\n第二行\"\n",
	} {
		if _, err := parseModuleManifest([]byte(valid)); err != nil {
			t.Errorf("parseModuleManifest(%q): %v", valid, err)
		}
	}

	for _, c := range []struct{ name, yaml, want string }{
		{"empty", "", "version: 1 is required"},
		{"comments only", "# nothing yet\n", "version: 1 is required"},
		{"no version", "import: [player]\n", "version: 1 is required"},
		{"a newer version", "version: 2\n", "version is 2; this nsgo reads version 1"},
		{"an unknown field", "version: 1\nskills: [chat]\n", "field skills not found"},
		{"a field's case", "version: 1\nImport: [player]\n", "field Import not found"},
		{"a key twice", "version: 1\nversion: 1\n", `mapping key "version" already defined`},
		{"a second document", "version: 1\n---\nversion: 1\n", "one YAML document is allowed"},
		{"an anchor", "version: 1\nimport: &dirs [player]\n", "line 2: anchors and aliases are not allowed"},
		{"an alias", "version: 1\nimport: &dirs [player]\ntargets: *dirs\n", "anchors and aliases are not allowed"},
		{"a merge key", "version: 1\n<<: {notes: hi}\n", "line 2: merge keys are not allowed"},
		{"a large file", "version: 1\nnotes: x\n# " + strings.Repeat("x", maxModuleManifest) + "\n", "more than 1 MiB"},
		{"a scalar for a list", "version: 1\nimport: player\n", "cannot unmarshal"},
		{"a list for the file", "- version: 1\n", "cannot unmarshal"},
		{"an empty import", "version: 1\nimport: []\n", "import lists no package"},
		{"an import twice", "version: 1\nimport: [player, player]\n", "import: player is listed twice"},
		{"an import outside", "version: 1\nimport: [../player]\n", `"../player" is not a clean directory`},
		{"an import with ./", "version: 1\nimport: [./player]\n", `"./player" is not a clean directory`},
		{"an absolute import", "version: 1\nimport: [/player]\n", `"/player" is not a clean directory`},
		{"a trailing slash", "version: 1\nimport: [player/]\n", `"player/" is not a clean directory`},
		{"a backslash", "version: 1\nimport: ['ui\\chat']\n", "is not a clean directory"},
		{"an empty import entry", "version: 1\nimport: ['']\n", `"" is not a clean directory`},
		{"a space", "version: 1\nimport: [my player]\n", `"my player" is not a directory an import path can name`},
		{"an internal package", "version: 1\nimport: [internal/player]\n", "internal/player is an internal package"},
		{"empty targets", "version: 1\ntargets: []\n", "targets lists no target"},
		{"an unknown target", "version: 1\ntargets: [macos, amiga]\n", `targets: unknown target "amiga"`},
		{"long notes", "version: 1\nnotes: \"" + strings.Repeat("x", maxModuleNotes+1) + "\"\n", "notes: 4097 bytes, more than 4 KiB"},
		{"an escape", "version: 1\nnotes: \"fine\\e[8m\"\n", "notes: line 1, column 5: U+001B"},
		{"a tab", "version: 1\nnotes: \"a\\tb\"\n", "U+0009"},
		{"a carriage return", "version: 1\nnotes: \"one\\ntwo\\rthree\"\n", "line 2, column 4: U+000D"},
		{"a bidi override", "version: 1\nnotes: \"left \\u202E right\"\n", "U+202E"},
		{"a bidi isolate", "version: 1\nnotes: \"left \\u2066 right\"\n", "U+2066"},
		{"a zero-width space", "version: 1\nnotes: \"zero\\u200Bwidth\"\n", "U+200B"},
		{"a byte order mark", "version: 1\nnotes: \"mark\\uFEFF\"\n", "U+FEFF"},
		{"a line separator", "version: 1\nnotes: \"line\\u2028separator\"\n", "U+2028"},
		{"a paragraph separator", "version: 1\nnotes: \"para\\u2029graph\"\n", "U+2029"},
		{"a next line", "version: 1\nnotes: \"next\\Nline\"\n", "U+0085"},
		{"bytes that are not UTF-8", "version: 1\nnotes: !!binary gA==\n", "notes: not UTF-8"},
		{"empty components", "version: 1\ncomponents: {}\n", "components lists no component"},
		{"an upper-case name", "version: 1\ncomponents: {Chat: ui/chat}\n", `components: "Chat" is not a name`},
		{"a leading hyphen", "version: 1\ncomponents: {-chat: ui/chat}\n", `"-chat" is not a name`},
		{"a trailing hyphen", "version: 1\ncomponents: {chat-: ui/chat}\n", `"chat-" is not a name`},
		{"a dot", "version: 1\ncomponents: {ui.chat: ui/chat}\n", `"ui.chat" is not a name`},
		{"an underscore", "version: 1\ncomponents: {file_picker: ui/filepicker}\n", `"file_picker" is not a name`},
		{"a long name", "version: 1\ncomponents: {" + strings.Repeat("a", 65) + ": ui/a}\n", "is not a name"},
		{"the root", "version: 1\ncomponents: {chat: .}\n", "components: chat: a component is a package below the module root"},
		{"a component outside", "version: 1\ncomponents: {chat: ../chat}\n", `components: chat: "../chat" is not a clean directory`},
		{"one directory twice", "version: 1\ncomponents: {chat: ui/chat, talk: ui/chat}\n", "components: chat and talk name the same directory, ui/chat"},
	} {
		if _, err := parseModuleManifest([]byte(c.yaml)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: parseModuleManifest(%q) = %v, want an error with %q", c.name, c.yaml, err, c.want)
		}
	}
}

func FuzzParseModuleManifest(f *testing.F) {
	for _, seed := range []string{
		kitManifest,
		"version: 1\n",
		"version: 1\nimport: [.]\ntargets: [all]\n",
		"version: 1\nimport: &dirs [player]\ntargets: *dirs\n",
		"version: 1\n<<: {notes: hi}\n",
		"version: 1\nnotes: !!binary gA==\n",
		"version: 1\nnotes: \"left \\u202E right\"\n",
		"version: 1\ncomponents: {chat: ui/chat, talk: ui/chat}\n",
		"version: 1\n---\nversion: 1\n",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := parseModuleManifest(data)
		if err != nil {
			return
		}
		// What the parser accepts keeps every rule of the format.
		if len(data) > maxModuleManifest || m.Version != 1 {
			t.Fatalf("accepted %d bytes of version %d", len(data), m.Version)
		}
		if err := checkManifestNotes(m.Notes); err != nil {
			t.Fatalf("accepted notes %q: %v", m.Notes, err)
		}
		if m.Import != nil && len(m.Import) == 0 || m.Targets != nil && len(m.Targets) == 0 || m.Components != nil && len(m.Components) == 0 {
			t.Fatalf("accepted an empty list: %+v", m)
		}
		for i, dir := range m.Import {
			if dir != "." && checkManifestDir(dir) != nil || slices.Contains(m.Import[:i], dir) {
				t.Fatalf("accepted the import %q", dir)
			}
		}
		if m.Targets != nil {
			if expanded, err := expandTargets(m.Targets); err != nil || !slices.Equal(expanded, m.expanded) {
				t.Fatalf("accepted the targets %q, expanded to %q: %v", m.Targets, m.expanded, err)
			}
		}
		dirs := map[string]bool{}
		for name, dir := range m.Components {
			if !validComponentName(name) || checkManifestDir(dir) != nil || dirs[dir] {
				t.Fatalf("accepted the component %q: %q", name, dir)
			}
			dirs[dir] = true
		}
		again, err := parseModuleManifest(data)
		if err != nil || !reflect.DeepEqual(m, again) {
			t.Fatalf("a second parse gave %+v, %v; the first %+v", again, err, m)
		}
	})
}

// A module's targets lack a target its list does not name once expanded; macos_universal is among them when both macOS architectures are, and a module without a list lacks none.
func TestModuleManifestLacks(t *testing.T) {
	for _, c := range []struct {
		targets []string
		lacks   map[string]bool
	}{
		{[]string{"macos"}, map[string]bool{"macos_arm64": false, "macos_x64": false, "macos_universal": false, "tui_macos_arm64": true, "ios": true}},
		{[]string{"macos_arm64"}, map[string]bool{"macos_arm64": false, "macos_universal": true}},
		{[]string{"macos_universal"}, map[string]bool{"macos_universal": false, "macos_arm64": true}},
		{[]string{"android", "web"}, map[string]bool{"android_arm64": false, "android_x64": true, "web": false}},
		{nil, map[string]bool{"android_arm64": false, "web": false}},
	} {
		yaml := "version: 1\n"
		if c.targets != nil {
			yaml += "targets: [" + strings.Join(c.targets, ", ") + "]\n"
		}
		m, err := parseModuleManifest([]byte(yaml))
		if err != nil {
			t.Fatal(err)
		}
		for target, want := range c.lacks {
			if got := m.lacks(target); got != want {
				t.Errorf("targets %q: lacks(%s) = %v, want %v", c.targets, target, got, want)
			}
		}
	}
}

// manifestProxy is a module proxy holding a stand-in NSGo at v0.4.0 and v0.5.0 and the libraries the tests of nsgo-module.yaml add.
func manifestProxy(t *testing.T) string {
	t.Helper()
	proxy := t.TempDir()
	addNSGoVersion(t, proxy, "v0.4.0", stubNSGoMod, stubNSGo)
	addNSGoVersion(t, proxy, "v0.5.0", stubNSGoMod, stubNSGo)
	addProxyModule(t, proxy, "github.com/acme/kit", "v1.0.0", "module github.com/acme/kit\n\ngo 1.26\n", map[string]string{
		moduleManifestName:            kitManifest,
		"kit.go":                      "package kit\n",
		"player/player.go":            "package player\n",
		"ui/chat/chat.go":             "package chat\n",
		"ui/chat/chat_test.go":        "package chat_test\n",
		"ui/chat/gen.go":              "//go:build ignore\n\npackage main\n",
		"ui/filepicker/filepicker.go": "//go:build ios\n\npackage filepicker\n",
	})
	// A module zip leaves out the modules nested in it, so example.com/nest's has no inner/.
	addProxyModule(t, proxy, "example.com/nest", "v1.0.0", "module example.com/nest\n\ngo 1.26\n\nrequire example.com/nest/inner v1.0.0\n", map[string]string{
		moduleManifestName: "version: 1\nimport: [inner/widget]\n",
		"nest.go":          "package nest\n",
	})
	addProxyModule(t, proxy, "example.com/nest/inner", "v1.0.0", "module example.com/nest/inner\n\ngo 1.26\n", map[string]string{"widget/widget.go": "package widget\n"})
	addProxyModule(t, proxy, "example.com/nestcomponent", "v1.0.0", "module example.com/nestcomponent\n\ngo 1.26\n", map[string]string{
		moduleManifestName: "version: 1\ncomponents: {widget: inner/widget}\n",
		"nest.go":          "package nest\n",
	})
	addProxyModule(t, proxy, "example.com/tools", "v1.0.0", "module example.com/tools\n\ngo 1.26\n", map[string]string{
		moduleManifestName: "version: 1\ncomponents: {tool: cmd/tool}\n",
		"cmd/tool/main.go": "package main\n\nfunc main() {}\n",
	})
	addProxyModule(t, proxy, "example.com/mainimport", "v1.0.0", "module example.com/mainimport\n\ngo 1.26\n", map[string]string{
		moduleManifestName: "version: 1\nimport: [cmd/tool]\n",
		"cmd/tool/main.go": "package main\n\nfunc main() {}\n",
	})
	addProxyModule(t, proxy, "example.com/misnamed", "v1.0.0", "module example.com/misnamed\n\ngo 1.26\n", map[string]string{
		moduleManifestName:     "version: 1\ncomponents: {talk: ui/talking}\n",
		"ui/talking/talker.go": "package talker\n",
	})
	addProxyModule(t, proxy, "example.com/assets", "v1.0.0", "module example.com/assets\n\ngo 1.26\n", map[string]string{
		moduleManifestName: "version: 1\ncomponents: {icons: icons}\n",
		"icons/flag.svg":   "<svg/>\n",
	})
	addProxyModule(t, proxy, "example.com/twopackages", "v1.0.0", "module example.com/twopackages\n\ngo 1.26\n", map[string]string{
		moduleManifestName: "version: 1\nimport: [ui]\n",
		"ui/a.go":          "package ui\n",
		"ui/b.go":          "package other\n",
	})
	addProxyModule(t, proxy, "example.com/missing", "v1.0.0", "module example.com/missing\n\ngo 1.26\n", map[string]string{
		moduleManifestName: "version: 1\nimport: [player]\n",
		"missing.go":       "package missing\n",
	})
	// A field a later NSGo adds, in a module that requires that NSGo and in one that does not.
	addProxyModule(t, proxy, "example.com/future", "v1.0.0", "module example.com/future\n\ngo 1.26\n\nrequire github.com/nsgo-dev/nsgo v0.5.0\n", map[string]string{
		moduleManifestName: "version: 1\nskills: [chat]\n",
		"future.go":        "package future\n",
	})
	addProxyModule(t, proxy, "example.com/present", "v1.0.0", "module example.com/present\n\ngo 1.26\n\nrequire github.com/nsgo-dev/nsgo v0.4.0\n", map[string]string{
		moduleManifestName: "version: 1\nskills: [chat]\n",
		"present.go":       "package present\n",
	})
	addProxyModule(t, proxy, "example.com/plain", "v1.0.0", "module example.com/plain\n\ngo 1.26\n", map[string]string{"plain.go": "package plain\n"})
	// The version a test's go.mod replaces with a directory of its own.
	addProxyModule(t, proxy, "example.com/local", "v1.0.0", "module example.com/local\n\ngo 1.26\n", map[string]string{"local.go": "package local\n"})
	return proxy
}

// The plan of a library with nsgo-module.yaml names the import lines it lists, the app's targets it lacks and the components it offers, each with the command that copies it, and ends with the author's notes, each line marked; applying it names the import lines next (#2462).
func TestAddPlanShowsModuleManifest(t *testing.T) {
	offlineAdd(t, manifestProxy(t))
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"go.mod":    "module example.com/app\n\ngo 1.27.3\n",
		"nsgo.yaml": "name: App\nbuild:\n  targets: [macos, android, web]\n",
	})
	plan, err := planAdd(t, dir, "github.com/acme/kit")
	if err != nil {
		t.Fatalf("plan: %v\n%s", err, plan)
	}
	const want = `github.com/acme/kit
  module     github.com/acme/kit v1.0.0, published 2026-01-01
  owner      github.com/acme, the first module from it in go.mod
  targets    android_arm64 web: not among the module's targets
  components chat         nsgo add github.com/acme/kit#chat
             file-picker  nsgo add github.com/acme/kit#file-picker
  next       import "github.com/acme/kit/player"
             import "github.com/acme/kit"

example.com/app
  go.mod     + require github.com/acme/kit v1.0.0 // indirect
  go.sum     + 2 lines
  checksums  GOSUMDB=off: go.sum takes the new hashes unchecked by the checksum database

  From github.com/acme/kit, the author's notes:
  | FFmpeg's libraries are LGPL 2.1: offer their source beside your downloads, credit FFmpeg and allow replacing them.
  |
  | Background playback on iOS needs apple.info.UIBackgroundModes: [audio].
`
	if plan != want {
		t.Errorf("plan:\n%s\nwant:\n%s", plan, want)
	}

	// From a directory of the app's, its nsgo.yaml is the closest; outside an app, no target is named.
	sub := filepath.Join(dir, "components")
	writeFiles(t, sub, map[string]string{"doc.go": "package components\n"})
	if plan, err := planAdd(t, sub, "github.com/acme/kit"); err != nil || !strings.Contains(plan, "  targets    android_arm64 web: not among the module's targets\n") {
		t.Errorf("from a subdirectory: %v\n%s", err, plan)
	}
	if err := os.Remove(filepath.Join(dir, "nsgo.yaml")); err != nil {
		t.Fatal(err)
	}
	if plan, err := planAdd(t, dir, "github.com/acme/kit"); err != nil || strings.Contains(plan, "targets") || !strings.Contains(plan, "components chat") {
		t.Errorf("outside an app: %v\n%s", err, plan)
	}

	t.Chdir(dir)
	var out bytes.Buffer
	stderr := captureStderr(t, func() {
		err = addModules(t.Context(), addOptions{yes: true}, []string{"github.com/acme/kit"}, &out, nil)
	})
	if err != nil || !strings.Contains(stderr, `next: import "github.com/acme/kit/player"`) || !strings.Contains(stderr, `next: import "github.com/acme/kit"`) {
		t.Errorf("applied: %v\n%s", err, stderr)
	}
}

// A module without nsgo-module.yaml has its root package's import line and nothing else of the file's (#2462).
func TestAddPlanWithoutModuleManifest(t *testing.T) {
	offlineAdd(t, manifestProxy(t))
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"go.mod":    "module example.com/app\n\ngo 1.27.3\n",
		"nsgo.yaml": "name: App\nbuild:\n  targets: [android]\n",
	})
	plan, err := planAdd(t, dir, "example.com/plain")
	if err != nil {
		t.Fatalf("plan: %v\n%s", err, plan)
	}
	if !strings.Contains(plan, "  next       import \"example.com/plain\"\n") || strings.Contains(plan, "targets") || strings.Contains(plan, "components") || strings.Contains(plan, "notes") {
		t.Errorf("plan:\n%s", plan)
	}
}

// An import or components entry that is not a package of the module itself, as one of a module nested in it, is refused, -y or not, as are a component that is a main package and one whose directory does not end in its package's name, and nothing is written (#2462).
func TestAddRefusesModuleManifestPackages(t *testing.T) {
	offlineAdd(t, manifestProxy(t))
	local := t.TempDir()
	writeFiles(t, local, map[string]string{
		"go.mod":           "module example.com/local\n\ngo 1.26\n",
		moduleManifestName: "version: 1\ncomponents: {inner: sub/inner}\n",
		"local.go":         "package local\n",
		"sub/go.mod":       "module example.com/local/sub\n\ngo 1.26\n",
		"sub/inner/i.go":   "package inner\n",
	})
	for _, c := range []struct{ module, want string }{
		{"example.com/nest", "example.com/nest: nsgo-module.yaml: import: inner/widget: no such directory in example.com/nest v1.0.0"},
		{"example.com/nestcomponent", "components: widget: inner/widget: no such directory in example.com/nestcomponent v1.0.0"},
		{"example.com/local@v1.0.0", "example.com/local: nsgo-module.yaml: components: inner: sub/inner is in the module nested at sub, not in example.com/local"},
		{"example.com/tools", "example.com/tools: nsgo-module.yaml: components: tool: cmd/tool is a main package"},
		{"example.com/mainimport", "example.com/mainimport: nsgo-module.yaml: import: cmd/tool is a main package, which apps cannot import"},
		{"example.com/misnamed", "components: talk: ui/talking holds package talker; a component's directory ends in its package's name, talking"},
		{"example.com/missing", "import: player: no such directory in example.com/missing v1.0.0"},
		{"example.com/assets", "example.com/assets: nsgo-module.yaml: components: icons: icons: holds no Go package"},
		{"example.com/twopackages", "import: ui: holds the packages ui (a.go) and other (b.go)"},
	} {
		dir := t.TempDir()
		gomod := "module example.com/app\n\ngo 1.27.3\n\nreplace example.com/local => " + filepath.ToSlash(local) + "\n"
		writeFiles(t, dir, map[string]string{"go.mod": gomod})
		for _, o := range []addOptions{{dryRun: true}, {yes: true}} {
			plan, err := runAdd(t, dir, o, nil, c.module)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s %+v: %v, want a refusal with %q\n%s", c.module, o, err, c.want, plan)
			}
			if got, _ := moduleFiles(t, dir); string(got) != gomod {
				t.Errorf("%s %+v: go.mod changed:\n%s", c.module, o, got)
			}
		}
	}
}

// A field this nsgo does not know, in a module that requires a newer NSGo, fails naming that NSGo and the go get that brings it; in a module that requires this nsgo's NSGo or an older one, and from a build that records no version, it fails as the strict file it is (#2462).
func TestAddModuleManifestNeedsANewerNSGo(t *testing.T) {
	offlineAdd(t, manifestProxy(t))
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"go.mod": "module example.com/app\n\ngo 1.27.3\n\nrequire github.com/nsgo-dev/nsgo v0.4.0\n\ntool github.com/nsgo-dev/nsgo/cmd/nsgo\n"})
	goIn(t, dir, "mod", "tidy")
	t.Cleanup(func() { manifestNSGoVersion = toolVersion })

	manifestNSGoVersion = func() string { return "v0.4.0" }
	plan, err := planAdd(t, dir, "example.com/future")
	if want := "example.com/future needs NSGo v0.5.0: go get github.com/nsgo-dev/nsgo@v0.5.0"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("a newer NSGo's field: %v, want %q", err, want)
	}
	if !strings.Contains(plan, "~ require github.com/nsgo-dev/nsgo v0.4.0 => v0.5.0, a release") {
		t.Errorf("the plan does not show NSGo's move:\n%s", plan)
	}
	_, err = planAdd(t, dir, "example.com/present")
	if err == nil || !strings.Contains(err.Error(), "example.com/present: nsgo-module.yaml: line 2: field skills not found") || strings.Contains(err.Error(), "needs NSGo") {
		t.Errorf("an unknown field: %v", err)
	}

	manifestNSGoVersion = func() string { return "" }
	_, err = planAdd(t, dir, "example.com/future")
	if err == nil || !strings.Contains(err.Error(), "example.com/future: nsgo-module.yaml: line 2: field skills not found") || strings.Contains(err.Error(), "needs NSGo") {
		t.Errorf("a build without a version: %v", err)
	}
}

// nsgo add of a component names it and says it cannot copy it yet, rather than calling it a malformed module path (#2462).
func TestParseAddRequestsRefusesAComponent(t *testing.T) {
	_, err := parseAddRequests([]string{"github.com/acme/kit#chat"})
	if want := "github.com/acme/kit#chat names a component, which this nsgo cannot copy yet"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("%v, want %q", err, want)
	}
}

// nsgo check -target warns of each linked module whose nsgo-module.yaml lacks the target, naming those it lacks, and of one whose file it cannot read, and says nothing of a module without the file or of one that lists the target (#2462).
func TestCheckWarnsOfModulesLackingTheTarget(t *testing.T) {
	if testing.Short() {
		t.Skip("cross-vets a real package")
	}
	offlineAdd(t, manifestProxy(t))
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.27.3\n",
		"app.go": "package app\n\nimport (\n\t_ \"example.com/plain\"\n\t_ \"example.com/present\"\n\t_ \"github.com/acme/kit/player\"\n)\n",
	})
	goIn(t, dir, "mod", "tidy")
	p, err := loadOrDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out string
	for _, target := range []string{"android_arm64,web", "macos_arm64"} {
		out = captureStderr(t, func() { err = check(context.Background(), p, target, 0) })
		if err != nil {
			t.Fatalf("check -target %s: %v\n%s", target, err, out)
		}
		if want := "example.com/present: nsgo-module.yaml: line 2: field skills not found"; !strings.Contains(out, want) {
			t.Errorf("check -target %s does not warn %q:\n%s", target, want, out)
		}
		if strings.Contains(out, "example.com/plain") {
			t.Errorf("check -target %s warns of a module without the file:\n%s", target, out)
		}
		lacks := strings.Contains(out, "github.com/acme/kit v1.0.0: android_arm64 web: not among the module's targets in its nsgo-module.yaml")
		if lacks != (target != "macos_arm64") || strings.Count(out, "github.com/acme/kit") > 1 {
			t.Errorf("check -target %s warned of github.com/acme/kit %v, once at most:\n%s", target, lacks, out)
		}
	}
}
