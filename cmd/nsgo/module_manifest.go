package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	buildtargets "github.com/nsgo-dev/nsgo/targets"
)

// nsgo-module.yaml, at a module's root beside go.mod, is where a library's author says which of its packages apps import, the targets it works on, what its users should know and which packages it offers for copying (#2462). nsgo add shows all of it in its plan, and nsgo check -target warns of a linked module whose targets lack the one it checks; nsgo build never reads it. The file is optional: a module without it counts as its root package, with no targets list, no notes and no components.

const (
	moduleManifestName = "nsgo-module.yaml"
	// maxModuleManifest bounds the file and maxModuleNotes its notes, in bytes.
	maxModuleManifest = 1 << 20
	maxModuleNotes    = 4 << 10
	// maxComponentName is the longest name a component takes, as a registry's names do (#2461).
	maxComponentName = 64
)

// moduleManifest is nsgo-module.yaml, version 1. New fields join version 1, and a module that uses one requires an NSGo that reads it, so an nsgo that does not names that NSGo (readModuleManifest).
type moduleManifest struct {
	Version int `yaml:"version"`
	// Import are the package directories apps import, relative to the module root, with . for the root; without it, the root package.
	Import []string `yaml:"import,omitempty"`
	// Targets are the targets the module works on, names and groups as targets.Expand takes them; without it, the module says nothing of where it works.
	Targets []string `yaml:"targets,omitempty"`
	// Notes are what the module's users should know, plain text the plan of nsgo add prints last.
	Notes string `yaml:"notes,omitempty"`
	// Components map the names of the packages the module offers for copying to their directories below its root.
	Components map[string]string `yaml:"components,omitempty"`

	// expanded is Targets as targets.Expand expands them.
	expanded []string
}

// parseModuleManifest reads nsgo-module.yaml strictly, one YAML document of at most 1 MiB, of version 1, without anchors, aliases, merge keys or fields this nsgo does not know, and checks each field; whether its directories hold the packages it says needs the module's files (checkPackages).
func parseModuleManifest(data []byte) (*moduleManifest, error) {
	if len(data) > maxModuleManifest {
		return nil, fmt.Errorf("%d bytes, more than 1 MiB", len(data))
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
		return nil, errors.New("version: 1 is required")
	} else if err != nil {
		return nil, yamlError(err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, errors.New("one YAML document is allowed")
	} else if !errors.Is(err, io.EOF) {
		return nil, yamlError(err)
	}
	if err := plainYAML(&doc); err != nil {
		return nil, err
	}
	strict := yaml.NewDecoder(bytes.NewReader(data))
	strict.KnownFields(true)
	var m moduleManifest
	if err := strict.Decode(&m); err != nil {
		return nil, yamlError(err)
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// yamlError is err with the errors of a yaml.TypeError on one line, which a warning prints whole.
func yamlError(err error) error {
	var typeErr *yaml.TypeError
	if errors.As(err, &typeErr) {
		return errors.New(strings.Join(typeErr.Errors, "; "))
	}
	return err
}

// plainYAML refuses anchors and merge keys under n, and so aliases, each of which follows the anchor it names, so that each value of the file is written where it applies.
func plainYAML(n *yaml.Node) error {
	if n.Anchor != "" {
		return fmt.Errorf("line %d: anchors and aliases are not allowed", n.Line)
	}
	for i, child := range n.Content {
		if n.Kind == yaml.MappingNode && i%2 == 0 && child.ShortTag() == "!!merge" {
			return fmt.Errorf("line %d: merge keys are not allowed", child.Line)
		}
		if err := plainYAML(child); err != nil {
			return err
		}
	}
	return nil
}

// validate checks each field of m on its own and expands its targets.
func (m *moduleManifest) validate() error {
	switch {
	case m.Version == 0:
		return errors.New("version: 1 is required")
	case m.Version != 1:
		return fmt.Errorf("version is %d; this nsgo reads version 1", m.Version)
	}
	if m.Import != nil && len(m.Import) == 0 {
		return errors.New("import lists no package; leave it out for the root package")
	}
	for i, dir := range m.Import {
		if dir != "." {
			if err := checkManifestDir(dir); err != nil {
				return fmt.Errorf("import: %w", err)
			}
			if slices.Contains(strings.Split(dir, "/"), "internal") {
				return fmt.Errorf("import: %s is an internal package, which other modules cannot import", dir)
			}
		}
		if slices.Contains(m.Import[:i], dir) {
			return fmt.Errorf("import: %s is listed twice", dir)
		}
	}
	if m.Targets != nil {
		if len(m.Targets) == 0 {
			return errors.New("targets lists no target; leave it out to say nothing of where the module works")
		}
		expanded, err := buildtargets.Expand(m.Targets)
		if err != nil {
			return fmt.Errorf("targets: %w", err)
		}
		m.expanded = expanded
	}
	if err := checkManifestNotes(m.Notes); err != nil {
		return fmt.Errorf("notes: %w", err)
	}
	if m.Components != nil && len(m.Components) == 0 {
		return errors.New("components lists no component; leave it out")
	}
	named := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(m.Components)) {
		dir := m.Components[name]
		if !validComponentName(name) {
			return fmt.Errorf("components: %q is not a name: 1 to %d lower-case letters, digits and hyphens, neither first nor last a hyphen", name, maxComponentName)
		}
		if dir == "." {
			return fmt.Errorf("components: %s: a component is a package below the module root, not the root", name)
		}
		if err := checkManifestDir(dir); err != nil {
			return fmt.Errorf("components: %s: %w", name, err)
		}
		if other, ok := named[dir]; ok {
			return fmt.Errorf("components: %s and %s name the same directory, %s", other, name, dir)
		}
		named[dir] = name
	}
	return nil
}

// checkManifestDir refuses a package directory that is not a clean, relative, slash-separated path whose elements an import path takes, as ui/chat is.
func checkManifestDir(dir string) error {
	if dir == "" || dir == "." || path.Clean(dir) != dir || path.IsAbs(dir) || dir == ".." || strings.HasPrefix(dir, "../") || strings.Contains(dir, `\`) {
		return fmt.Errorf("%q is not a clean directory relative to the module root, such as ui/chat", dir)
	}
	if err := module.CheckImportPath(dir); err != nil {
		return fmt.Errorf("%q is not a directory an import path can name: %w", dir, err)
	}
	return nil
}

// validComponentName reports whether name follows the rule for names in a registry's index (#2461): 1 to 64 lower-case ASCII letters, digits and hyphens, neither first nor last a hyphen, so that a name never reads as a module path.
func validComponentName(name string) bool {
	if name == "" || len(name) > maxComponentName || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	for i := range len(name) {
		if c := name[i]; !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// checkManifestNotes refuses notes of more than 4 KiB, or holding a character that could hide or rewrite what the plan prints around them: a control character other than a line feed, a format character such as a bidi control, or a line or paragraph separator, which a terminal may break a line at without the mark the plan puts before each line.
func checkManifestNotes(notes string) error {
	if len(notes) > maxModuleNotes {
		return fmt.Errorf("%d bytes, more than 4 KiB", len(notes))
	}
	if !utf8.ValidString(notes) {
		return errors.New("not UTF-8")
	}
	line, column := 1, 1
	for _, r := range notes {
		if r == '\n' {
			line, column = line+1, 1
			continue
		}
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			return fmt.Errorf("line %d, column %d: %U is a control or format character", line, column, r)
		}
		column++
	}
	return nil
}

// manifestNSGoVersion is the NSGo whose module files this nsgo reads, its own version, which tests replace.
var manifestNSGoVersion = toolVersion

// readModuleManifest reads the nsgo-module.yaml of the module path at version, whose files are in dir, through an os.Root opened at dir: nil, without an error, when the module has none. A file this nsgo cannot read, in a module that requires a newer NSGo than this nsgo's, fails naming the go get that brings that NSGo.
func readModuleManifest(path, version, dir string) (*moduleManifest, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	defer root.Close()
	f, err := root.Open(moduleManifestName)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	} else if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: %s is not a regular file", path, moduleManifestName)
	}
	// One byte past the limit is enough for parseModuleManifest to refuse the file, however large it is.
	data, err := io.ReadAll(io.LimitReader(f, maxModuleManifest+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	m, err := parseModuleManifest(data)
	if err == nil {
		return m, nil
	}
	if needed := newerNSGo(root, path, version); needed != "" {
		return nil, fmt.Errorf("%s needs NSGo %s: go get %s@%s (its %s: %v)", path, needed, nsgoModule, needed, moduleManifestName, err)
	}
	return nil, fmt.Errorf("%s: %s: %w", path, moduleManifestName, err)
}

// newerNSGo is the NSGo that the module path at version requires, by the go.mod in root, when it is newer than this nsgo's, else empty; NSGo's own module stands for itself at its version. A build that records no version, as one from a checkout, takes itself for the newest.
func newerNSGo(root *os.Root, path, version string) string {
	required := version
	if path != nsgoModule {
		data, err := root.ReadFile("go.mod")
		if err != nil {
			return ""
		}
		f, err := modfile.ParseLax("go.mod", data, nil)
		if err != nil {
			return ""
		}
		required = requiredVersion(f, nsgoModule)
	}
	own := manifestNSGoVersion()
	if own == "" || !semver.IsValid(required) || semver.Compare(required, own) <= 0 {
		return ""
	}
	return required
}

// manifestImportPath is the import path of the package in dir, relative to the root of the module path.
func manifestImportPath(path, dir string) string {
	if dir == "." {
		return path
	}
	return path + "/" + dir
}

// packageDirs are the directories m names, import's then the components', in order.
func (m *moduleManifest) packageDirs() []string {
	dirs := slices.Clone(m.Import)
	for _, name := range slices.Sorted(maps.Keys(m.Components)) {
		dirs = append(dirs, m.Components[name])
	}
	return dirs
}

// manifestPackage is what go list -find says of a package that an nsgo-module.yaml names: the module it is in, or why it is in none.
type manifestPackage struct {
	ImportPath string
	Module     *struct{ Path string }
	Error      *struct{ Err string }
}

// manifestPackages runs go list -find -e on the packages at paths with flags, which loads none of their imports, and returns what it says of each.
func (m *addModule) manifestPackages(ctx context.Context, flags, paths []string) (map[string]manifestPackage, error) {
	out, err := m.goOutput(ctx, append(append([]string{"list", "-find", "-e", "-json=ImportPath,Module,Error", "-mod=readonly"}, flags...), paths...)...)
	if err != nil {
		return nil, err
	}
	pkgs := map[string]manifestPackage{}
	for dec := json.NewDecoder(bytes.NewReader(out)); dec.More(); {
		var pkg manifestPackage
		if err := dec.Decode(&pkg); err != nil {
			return nil, fmt.Errorf("go list: %w", err)
		}
		pkgs[pkg.ImportPath] = pkg
	}
	return pkgs, nil
}

// checkPackages checks the directories that the manifest of the module modPath at version, whose files are in dir, names: each holds a package that go list, which said pkgs of them, finds in this module, not in a module nested in it, and a component's is not a main package and ends in its package's name.
func (m *moduleManifest) checkPackages(modPath, version, dir string, pkgs map[string]manifestPackage) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("%s: %w", modPath, err)
	}
	defer root.Close()
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s: %s: %s", modPath, moduleManifestName, fmt.Sprintf(format, args...))
	}
	// name is the package in entry, which field names, once it is one of this module's.
	name := func(field, entry string) (string, error) {
		// A module zip leaves out the modules nested in it, so a directory of one is missing from the module cache; in a directory that replaces the module, its go.mod says so.
		if _, err := root.Stat(filepath.FromSlash(entry)); err != nil {
			return "", fail("%s: %s: no such directory in %s %s (a module's zip leaves out the modules nested in it)", field, entry, modPath, version)
		}
		for at := entry; at != "."; at = path.Dir(at) {
			if _, err := root.Stat(filepath.FromSlash(at + "/go.mod")); err == nil {
				return "", fail("%s: %s is in the module nested at %s, not in %s", field, entry, at, modPath)
			}
		}
		pkgName, err := packageNameIn(root, entry)
		if err != nil {
			return "", fail("%s: %s: %v", field, entry, err)
		}
		switch pkg := pkgs[manifestImportPath(modPath, entry)]; {
		case pkg.Module == nil:
			reason := "the go command finds it in no module"
			if pkg.Error != nil {
				reason, _, _ = strings.Cut(pkg.Error.Err, "\n")
			}
			return "", fail("%s: %s is not a package of %s: %s", field, entry, modPath, reason)
		case pkg.Module.Path != modPath:
			return "", fail("%s: %s is a package of %s, not of %s", field, entry, pkg.Module.Path, modPath)
		}
		return pkgName, nil
	}
	for _, entry := range m.Import {
		pkgName, err := name("import", entry)
		if err != nil {
			return err
		}
		if pkgName == "main" {
			return fail("import: %s is a main package, which apps cannot import", entry)
		}
	}
	for _, component := range slices.Sorted(maps.Keys(m.Components)) {
		entry := m.Components[component]
		pkgName, err := name("components: "+component, entry)
		if err != nil {
			return err
		}
		if pkgName == "main" {
			return fail("components: %s: %s is a main package, which no app can import", component, entry)
		}
		if last := path.Base(entry); pkgName != last {
			return fail("components: %s: %s holds package %s; a component's directory ends in its package's name, %s", component, entry, pkgName, last)
		}
	}
	return nil
}

// packageNameIn is the name that the Go files in dir under root, other than tests, declare, whatever their build constraints, since an app may import or copy a package for any target. Files the go command ignores, whose names start with _ or ., and files that //go:build ignore leaves out of every build, such as a generator's, do not count.
func packageNameIn(root *os.Root, dir string) (string, error) {
	files := root.FS()
	entries, err := fs.ReadDir(files, dir)
	if err != nil {
		return "", err
	}
	var name, first string
	for _, entry := range entries {
		file := entry.Name()
		if !entry.Type().IsRegular() || !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") || strings.HasPrefix(file, "_") || strings.HasPrefix(file, ".") {
			continue
		}
		src, err := fs.ReadFile(files, path.Join(dir, file))
		if err != nil {
			return "", err
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, src, parser.PackageClauseOnly|parser.ParseComments)
		if err != nil {
			return "", err
		}
		if ignoredGoFile(parsed) {
			continue
		}
		switch {
		case name == "":
			name, first = parsed.Name.Name, file
		case parsed.Name.Name != name:
			return "", fmt.Errorf("holds the packages %s (%s) and %s (%s)", name, first, parsed.Name.Name, file)
		}
	}
	if name == "" {
		return "", errors.New("holds no Go package")
	}
	return name, nil
}

// ignoredGoFile reports whether file's //go:build line is ignore, which keeps a file such as a generator out of every build.
func ignoredGoFile(file *ast.File) bool {
	for _, group := range file.Comments {
		if group.Pos() > file.Package {
			break
		}
		for _, c := range group.List {
			if expr, err := constraint.Parse(c.Text); err == nil {
				if tag, ok := expr.(*constraint.TagExpr); ok && tag.Tag == "ignore" {
					return true
				}
			}
		}
	}
	return false
}

// lacks reports whether m lists targets and target is not among them once expanded; macos_universal, which joins the two macOS builds, is among them when both are.
func (m *moduleManifest) lacks(target string) bool {
	if m.Targets == nil || slices.Contains(m.expanded, target) {
		return false
	}
	if target == "macos_universal" {
		return !slices.Contains(m.expanded, "macos_arm64") || !slices.Contains(m.expanded, "macos_x64")
	}
	return true
}

// readManifests reads the nsgo-module.yaml of each module planned from the module cache, checks the packages each names with go list and flags, the planned go.mod's, and, when one lists targets, reads the app's. A manifest that fails is the item's manifestErr, which refuses the plan.
func (p *addPlan) readManifests(ctx context.Context, flags []string) error {
	stage(ctx, "reading the modules' %s", moduleManifestName)
	var paths []string
	for i := range p.items {
		item := &p.items[i]
		if item.listed.Error != nil || item.listed.Dir == "" {
			continue
		}
		if item.manifest, item.manifestErr = readModuleManifest(item.path, item.selected, item.listed.Dir); item.manifest == nil {
			continue
		}
		for _, dir := range item.manifest.packageDirs() {
			paths = append(paths, manifestImportPath(item.path, dir))
		}
	}
	if len(paths) > 0 {
		pkgs, err := p.m.manifestPackages(ctx, flags, paths)
		if err != nil {
			return err
		}
		for i := range p.items {
			item := &p.items[i]
			if item.manifest == nil {
				continue
			}
			if err := item.manifest.checkPackages(item.path, item.selected, item.listed.Dir, pkgs); err != nil {
				item.manifest, item.manifestErr = nil, err
			}
		}
	}
	if slices.ContainsFunc(p.items, func(item addItem) bool { return item.manifest != nil && item.manifest.Targets != nil }) {
		targets, err := addAppTargets(p.m.dir)
		if err != nil {
			return err
		}
		p.appTargets = targets
	}
	return nil
}

// addAppTargets are the targets of the app whose nsgo.yaml is the closest to the working directory within the module at moduleDir, build.targets expanded as nsgo build expands them, or none outside an app.
func addAppTargets(moduleDir string) ([]string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "nsgo.yaml")); err == nil {
			p, err := load(dir)
			if err != nil {
				return nil, err
			}
			return p.Build.Targets, nil
		}
		if dir == moduleDir || filepath.Dir(dir) == dir {
			return nil, nil
		}
	}
}

// importLines are the import declarations the plan names for item: one for each package its nsgo-module.yaml lists under import, else its root package's, when it has one an app imports.
func (item addItem) importLines() []string {
	if item.manifest == nil || item.manifest.Import == nil {
		if item.importable() {
			return []string{fmt.Sprintf("import %q", item.path)}
		}
		return nil
	}
	lines := make([]string, len(item.manifest.Import))
	for i, dir := range item.manifest.Import {
		lines[i] = fmt.Sprintf("import %q", manifestImportPath(item.path, dir))
	}
	return lines
}

// lackedTargets is the plan's line naming the targets of the app's build.targets that item's module does not list among its own, or none.
func (p *addPlan) lackedTargets(item addItem) []string {
	if item.manifest == nil {
		return nil
	}
	var lacked []string
	for _, target := range p.appTargets {
		if item.manifest.lacks(target) {
			lacked = append(lacked, target)
		}
	}
	if len(lacked) == 0 {
		return nil
	}
	return []string{strings.Join(lacked, " ") + ": not among the module's targets"}
}

// componentCommands are the plan's lines for the components item's module offers: each name, with the command that copies it.
func (item addItem) componentCommands() []string {
	if item.manifest == nil {
		return nil
	}
	names := slices.Sorted(maps.Keys(item.manifest.Components))
	width := 0
	for _, name := range names {
		width = max(width, len(name))
	}
	lines := make([]string, len(names))
	for i, name := range names {
		lines[i] = fmt.Sprintf("%-*s  %s add %s#%s", width, name, nsgoCommand(), item.path, name)
	}
	return lines
}

// writeNotes ends the plan with the notes of each module planned, under a line that names the module, each of their lines marked, so that none reads as the plan's own.
func (p *addPlan) writeNotes(b *bytes.Buffer) {
	for _, item := range p.items {
		if item.manifest == nil {
			continue
		}
		notes := strings.TrimSpace(item.manifest.Notes)
		if notes == "" {
			continue
		}
		fmt.Fprintf(b, "\n  From %s, the author's notes:\n", item.path)
		for _, line := range strings.Split(notes, "\n") {
			fmt.Fprintln(b, strings.TrimRight("  | "+line, " "))
		}
	}
}

// linkedManifests are the nsgo-module.yaml files of the modules that the builds nsgo check -target vets link, each read once, and, by the index of each target checked, the modules whose targets lack it.
type linkedManifests struct {
	mu       sync.Mutex
	read     map[string]*moduleManifest // by module directory; nil without the file or when it failed
	problems map[string]error           // by module path
	lacking  map[int][]string
}

func newLinkedManifests() *linkedManifests {
	return &linkedManifests{read: map[string]*moduleManifest{}, problems: map[string]error{}, lacking: map[int][]string{}}
}

// linkedModuleFormat is go list's line for a package of a module other than the main ones: the module's path, version and directory.
const linkedModuleFormat = `{{with .Module}}{{if not .Main}}{{.Path}}{{"\t"}}{{.Version}}{{"\t"}}{{.Dir}}{{end}}{{end}}`

// scan records the modules whose nsgo-module.yaml lacks target, the index-th checked, among those that the build for target links of the packages in p's directory, as go list -deps lists them with the target's environment env and its tags. A go list that fails leaves the target unscanned: vetting it reports what keeps the go command from loading the packages.
func (l *linkedManifests) scan(ctx context.Context, p *project, env []string, index int, target string) {
	tags, err := goTags(ctx, p, env, targetTags(target)...)
	if err != nil {
		return
	}
	args := append([]string{"list", "-e", "-deps", "-f", linkedModuleFormat}, tagsArgs(tags)...)
	out, err := execute(ctx, processCommand{name: "go", args: append(args, "./..."), dir: p.Dir, env: append(os.Environ(), env...)})
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for line := range strings.Lines(string(out)) {
		path, rest, _ := strings.Cut(strings.TrimRight(line, "\r\n"), "\t")
		version, dir, _ := strings.Cut(rest, "\t")
		if path == "" || dir == "" || seen[path] {
			continue
		}
		seen[path] = true
		if m := l.manifest(path, version, dir); m != nil && m.lacks(target) {
			l.mu.Lock()
			l.lacking[index] = append(l.lacking[index], strings.TrimSpace(path+" "+version))
			l.mu.Unlock()
		}
	}
}

// manifest is the nsgo-module.yaml of the module path at version in dir, read the first time it is asked for; a file that fails is recorded for warn and counts as none.
func (l *linkedManifests) manifest(path, version, dir string) *moduleManifest {
	l.mu.Lock()
	defer l.mu.Unlock()
	if m, ok := l.read[dir]; ok {
		return m
	}
	m, err := readModuleManifest(path, version, dir)
	if err != nil {
		l.problems[path] = err
	}
	l.read[dir] = m
	return m
}

// warn prints a warning for each module whose nsgo-module.yaml nsgo check could not read, then one for each module whose targets lack a target checked, naming those targets in the order of names.
func (l *linkedManifests) warn(names []string) {
	for _, path := range slices.Sorted(maps.Keys(l.problems)) {
		warn("%v", l.problems[path])
	}
	var order []string
	lacked := map[string][]string{}
	for i, name := range names {
		for _, mod := range l.lacking[i] {
			if lacked[mod] == nil {
				order = append(order, mod)
			}
			lacked[mod] = append(lacked[mod], name)
		}
	}
	for _, mod := range order {
		warn("%s: %s: not among the module's targets in its %s", mod, strings.Join(lacked[mod], " "), moduleManifestName)
	}
}
