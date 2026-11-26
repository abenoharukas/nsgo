package main

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// loadYAML loads an app whose nsgo.yaml is src.
func loadYAML(t *testing.T, src string) (*project, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "nsgo.yaml"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return load(dir)
}

// A key nsgo.yaml does not take fails at its line, at any depth, with the path of its mapping and the nearest key when one is near, before anything checks the values (#2475).
func TestNSGoYAMLRefusesUnknownKeys(t *testing.T) {
	for _, tc := range []struct{ yaml, want string }{
		{"name: A\nandroid:\n  permisions: [RECORD_AUDIO]\n", "nsgo.yaml:3: android has no key permisions; did you mean permissions?"},
		{"name: A\napple:\n  infos:\n    NSMicrophoneUsageDescription: Records.\n", "nsgo.yaml:3: apple has no key infos; did you mean info?"},
		{"name: A\nlicences:\n  copyleft: error\n", "nsgo.yaml:2: nsgo.yaml has no key licences; did you mean licenses?"},
		{"nmae: A\n", "nsgo.yaml:1: nsgo.yaml has no key nmae; did you mean name?"},
		{"name: A\nBuild:\n  targets: [web]\n", "nsgo.yaml:2: nsgo.yaml has no key Build; did you mean build?"},
		{"name: A\nhot_reload: false\n", "nsgo.yaml:2: nsgo.yaml has no key hot_reload"},
		{"name: A\ndev:\n  hotreload: false\n", "nsgo.yaml:3: dev has no key hotreload; did you mean hot_reload?"},
		{"name: A\napple:\n  privacy:\n    traking: false\n", "nsgo.yaml:4: apple.privacy has no key traking; did you mean tracking?"},
		{"name: A\nfile_types:\n  - name: Doc\n    extensions: [doc]\n  - name: Sheet\n    extentions: [sheet]\n", "nsgo.yaml:6: file_types[1] has no key extentions; did you mean extensions?"},
		{"name: A\nlaunch_screen:\n  backgound: \"#000000\"\n", "nsgo.yaml:3: launch_screen has no key backgound; did you mean background?"},
		{"name: A\nlaunch_screen:\n  background: \"#000000\"\n  dark:\n    backgrond: \"#FFFFFF\"\n", "nsgo.yaml:5: launch_screen.dark has no key backgrond; did you mean background?"},
		{"name: A\nicon:\n  android:\n    forground: assets/Foreground.png\n", "nsgo.yaml:4: icon.android has no key forground; did you mean foreground?"},
		{"name: A\nharmonyos:\n  signing:\n    key_alais: release\n", "nsgo.yaml:4: harmonyos.signing has no key key_alais; did you mean key_alias?"},
		{"name: A\nstatus_bar:\n  hiden: true\n", "nsgo.yaml:3: status_bar has no key hiden; did you mean hidden?"},
		{"name: A\nandroid:\n  \"permissions\\u200b\": [CAMERA]\n", "nsgo.yaml:3: android has no key \"permissions\\u200b\"; did you mean permissions?"},
		// Every unknown key is named, in the file's order.
		{"name: A\nandroid:\n  permisions: [RECORD_AUDIO]\napple:\n  infos: {}\n", "nsgo.yaml:3: android has no key permisions; did you mean permissions?\nnsgo.yaml:5: apple has no key infos; did you mean info?"},
		// An alias and a merge key bring their mapping's keys where they are used, which checks them there, at the lines they are written on.
		{"name: A\nwindows: &windows\n  path: true\nlinux: *windows\n", "nsgo.yaml:3: linux has no key path"},
		{"name: A\nwindows: &windows\n  path: true\nlinux:\n  <<: *windows\n", "nsgo.yaml:3: linux has no key path"},
		// A key written as an alias is the key its anchor names, refused at the alias's line, though the anchor is in a map that takes any key.
		{"name: A\napple:\n  info:\n    &k permisions: x\nandroid:\n  *k : [CAMERA]\n", "nsgo.yaml:6: android has no key permisions; did you mean permissions?"},
	} {
		_, err := loadYAML(t, tc.yaml)
		if err == nil || err.Error() != tc.want {
			t.Errorf("%q: load error = %v, want %q", tc.yaml, err, tc.want)
		}
	}
}

// load still reads an empty file as one without a name, and reports YAML's own errors, those of a value of the wrong type among them, as yaml.v3 does.
func TestNSGoYAMLDecodeErrors(t *testing.T) {
	for _, tc := range []struct{ yaml, want string }{
		{"", "nsgo.yaml: name is required"},
		{"# Nothing yet.\n", "nsgo.yaml: name is required"},
		{"name: A\nbuild_number: many\n", "nsgo.yaml: yaml: unmarshal errors:\n  line 2: cannot unmarshal !!str `many` into int"},
		{"name: A\n  id: x\n", "nsgo.yaml: yaml: line 2: mapping values are not allowed in this context"},
	} {
		_, err := loadYAML(t, tc.yaml)
		if err == nil || err.Error() != tc.want {
			t.Errorf("%q: load error = %v, want %q", tc.yaml, err, tc.want)
		}
	}
}

// The keys nsgo.yaml no longer takes keep the errors that say what replaced them (#2475).
func TestNSGoYAMLRemovedKeys(t *testing.T) {
	const fps = "nsgo.yaml: fps is gone: start the app with --nsgo-fps, or run nsgo dev -fps"
	for _, tc := range []struct{ yaml, want string }{
		{"name: A\nwindow:\n  width: 800\n  height: 600\n", "nsgo.yaml: window is set in Go now: nsgo.Run(Root, nsgo.WindowSize(width, height)), or desktop.WindowOptions"},
		{"name: A\ndev:\n  fps: true\n", fps},
		{"name: A\nbuild:\n  fps: 60\n", fps},
	} {
		_, err := loadYAML(t, tc.yaml)
		if err == nil || err.Error() != tc.want {
			t.Errorf("%q: load error = %v, want %q", tc.yaml, err, tc.want)
		}
	}
}

// What nsgo.yaml passes on to other formats takes any key, as do the parts whose own readers check them; aliases and merge keys of keys it takes load (#2475).
func TestNSGoYAMLTakesFreeFormKeys(t *testing.T) {
	p, err := loadYAML(t, `name: A
apple:
  info:
    NSMicrophoneUsageDescription: Records voice notes.
    UIBackgroundModes: [audio]
  privacy:
    accessed_apis:
      NSPrivacyAccessedAPICategoryDiskSpace: [E174.1]
    collected_data:
      - NSPrivacyCollectedDataType: NSPrivacyCollectedDataTypeEmailAddress
        NSPrivacyCollectedDataTypeLinked: true
macos:
  entitlements:
    com.apple.security.device.audio-input: true
miniprogram:
  appid:
    wechat: wx0123456789abcdef
itch:
  channels:
    web: html5
tailwind:
  colors:
    brand: "#3366FF"
capabilities:
  clipboard: true
status_bar:
  foreground: light
  hidden: false
  immersive: true
linux:
  depends: &deps
    deb: [libfoo1]
    arch: [foo]
  recommends:
    <<: *deps
    deb: [libbar1]
`)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Apple.Info["NSMicrophoneUsageDescription"]; got != "Records voice notes." {
		t.Errorf("apple.info.NSMicrophoneUsageDescription = %v", got)
	}
	if got := p.MacOS.Entitlements["com.apple.security.device.audio-input"]; got != true {
		t.Errorf("macos.entitlements = %v", p.MacOS.Entitlements)
	}
	if s := p.StatusBar; s.Foreground != "light" || s.Hidden == nil || *s.Hidden || s.Immersive == nil || !*s.Immersive {
		t.Errorf("status_bar = %+v", s)
	}
	if got := p.Linux.Recommends; !slices.Equal(got.Deb, []string{"libbar1"}) || !slices.Equal(got.Arch, []string{"foo"}) {
		t.Errorf("linux.recommends = %+v, want deb [libbar1] and arch [foo] from the merge", got)
	}
}

// checkKeys walks a node as a type once, so a document whose aliases double at each level takes time in its nodes, not in the 2^60 paths to its first.
func TestCheckKeysWalksAliasesOnce(t *testing.T) {
	type nest []nest
	var b strings.Builder
	b.WriteString("l0: &l0 [{name: A}]\n")
	for i := 1; i <= 60; i++ {
		fmt.Fprintf(&b, "l%d: &l%d [*l%d, *l%d]\n", i, i, i-1, i-1)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(b.String()), &doc); err != nil {
		t.Fatal(err)
	}
	if err := checkKeys("nest.yaml", &doc, reflect.TypeFor[map[string]nest]()); err != nil {
		t.Fatal(err)
	}
}

// checkKeys takes the keys of an inlined struct in its place, and gives every other key to an inlined map, as yaml.v3 decodes them.
func TestCheckKeysInline(t *testing.T) {
	type inner struct {
		Inner string `yaml:"inner"`
	}
	type withStruct struct {
		Outer string `yaml:"outer"`
		inner `yaml:",inline"`
	}
	type withMap struct {
		Outer string                `yaml:"outer"`
		Rest  map[string]withStruct `yaml:",inline"`
	}
	for _, tc := range []struct {
		typ       reflect.Type
		yaml, err string
	}{
		{reflect.TypeFor[withStruct](), "outer: a\ninner: b\n", ""},
		{reflect.TypeFor[withStruct](), "outer: a\ninnr: b\n", "x.yaml:2: x.yaml has no key innr; did you mean inner?"},
		{reflect.TypeFor[withMap](), "outer: a\nany: {outer: b, inner: c}\n", ""},
		{reflect.TypeFor[withMap](), "outer: a\nany: {outer: b, iner: c}\n", "x.yaml:2: any has no key iner; did you mean inner?"},
	} {
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(tc.yaml), &doc); err != nil {
			t.Fatal(err)
		}
		got := ""
		if err := checkKeys("x.yaml", &doc, tc.typ); err != nil {
			got = err.Error()
		}
		if got != tc.err {
			t.Errorf("%v %q: checkKeys = %q, want %q", tc.typ, tc.yaml, got, tc.err)
		}
	}
}

// yamlFields reads a struct's keys as yaml.v3 does: every struct nsgo.yaml decodes into, at any depth, takes the keys yaml.v3 writes for it, in its order, and no others.
func TestYAMLFieldsAsYAMLv3(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var visit func(reflect.Type)
	visit = func(typ reflect.Type) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array || typ.Kind() == reflect.Map {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || typ == yamlNodeType || seen[typ] {
			return
		}
		seen[typ] = true
		data, err := yaml.Marshal(reflect.New(typ).Interface())
		if err != nil {
			t.Fatalf("%v: %v", typ, err)
		}
		var written yaml.Node
		if err := yaml.Unmarshal(data, &written); err != nil {
			t.Fatalf("%v: %v", typ, err)
		}
		var want []string
		for i, n := range written.Content[0].Content {
			if i%2 == 0 {
				want = append(want, n.Value)
			}
		}
		var got []string
		fields, _ := yamlFields(typ)
		for _, f := range fields {
			got = append(got, f.key)
			visit(f.typ)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%v takes %v, yaml.v3 writes %v", typ, got, want)
		}
	}
	visit(reflect.TypeFor[project]())
	if len(seen) < 20 {
		t.Fatalf("visited %d structs, too few for nsgo.yaml's", len(seen))
	}
}

// Every nsgo.yaml in the repository, the examples' and testdata's among them, and the one nsgo init writes, loads: nothing NSGo writes uses a key it refuses (#2475).
func TestRepositoryNSGoYAMLsLoad(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dirs := repositoryNSGoYAMLs(t, root)
	if len(dirs) < 40 {
		t.Fatalf("found %d nsgo.yaml files under %s, too few for the repository's", len(dirs), root)
	}
	for _, dir := range dirs {
		if _, err := load(dir); err != nil {
			t.Errorf("%s: %v", dir, err)
		}
	}

	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "nsgo.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	err = initYAML.Execute(f, initData{Name: "My App", ID: "dev.nsgo.myapp"})
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := load(dir); err != nil {
		t.Errorf("nsgo init's nsgo.yaml: %v", err)
	}
}

// repositoryNSGoYAMLs are the directories under root of the nsgo.yaml files Git tracks, so that an untracked one of a local checkout, such as a scratch app's or a nested worktree's, does not fail the test; without Git, those of every nsgo.yaml under root outside hidden directories and node_modules.
func repositoryNSGoYAMLs(t *testing.T, root string) []string {
	t.Helper()
	var dirs []string
	cmd := exec.Command("git", "ls-files", "-z", "--", "*nsgo.yaml")
	cmd.Dir = root
	if out, err := cmd.Output(); err == nil {
		for _, name := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
			if path.Base(name) == "nsgo.yaml" {
				dirs = append(dirs, filepath.Join(root, filepath.FromSlash(path.Dir(name))))
			}
		}
		return dirs
	}
	err := filepath.WalkDir(root, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && name != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if d.Name() == "nsgo.yaml" && d.Type().IsRegular() {
			dirs = append(dirs, filepath.Dir(name))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return dirs
}
