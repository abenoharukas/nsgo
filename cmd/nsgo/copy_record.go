package main

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
	"golang.org/x/mod/module"
)

// nsgo-copy.yaml sits in each copied component's directory (#2467, #2470, #2459).
// It records the source module, canonical version, component name, test choice,
// copied files and generated files deterministically with no timestamps or
// hashes. Every read driven by a record goes through an os.Root opened at the
// copy's directory, with paths validated on every read and symlinks refused.

const (
	copyRecordName     = "nsgo-copy.yaml"
	copyRecordVersion  = 1
	maxCopyRecordBytes = 64 << 10 // 64 KiB
)

// copyRecord is version 1 of nsgo-copy.yaml.
type copyRecord struct {
	Version       int      `yaml:"version"`
	Module        string   `yaml:"module"`
	ModuleVersion string   `yaml:"module_version"`
	Component     string   `yaml:"component"`
	Tests         bool     `yaml:"tests"`
	Files         []string `yaml:"files"`
	Generated     []string `yaml:"generated,omitempty"`
}

// moduleCopy is an existing copied component discovered inside the app's module.
type moduleCopy struct {
	dir        string // absolute path to the copy's directory
	relDir     string // slash-separated path relative to the module root
	importPath string // <appModule>/<relDir>
	record     *copyRecord
}

// parseCopyRecord strictly parses and validates nsgo-copy.yaml bytes.
func parseCopyRecord(data []byte) (*copyRecord, error) {
	if len(data) > maxCopyRecordBytes {
		return nil, fmt.Errorf("%s exceeds 64 KiB", copyRecordName)
	}
	if bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}) {
		return nil, fmt.Errorf("%s: UTF-8 byte order mark (BOM) is not allowed", copyRecordName)
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("%s: not valid UTF-8", copyRecordName)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("%s: empty YAML file", copyRecordName)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%s: %w", copyRecordName, err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("%s: multiple YAML documents are not allowed", copyRecordName)
		}
		return nil, fmt.Errorf("%s: %w", copyRecordName, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: must be a YAML mapping", copyRecordName)
	}
	if err := rejectCopyYAMLFeatures(&doc); err != nil {
		return nil, fmt.Errorf("%s: %w", copyRecordName, err)
	}

	// Capture line numbers for files/generated entries so path refusals can name the line.
	pathLines := map[string]int{}
	rootMap := doc.Content[0]
	for i := 0; i+1 < len(rootMap.Content); i += 2 {
		k, v := rootMap.Content[i], rootMap.Content[i+1]
		if (k.Value == "files" || k.Value == "generated") && v.Kind == yaml.SequenceNode {
			for _, item := range v.Content {
				if item.Kind == yaml.ScalarNode {
					if _, exists := pathLines[item.Value]; !exists {
						pathLines[item.Value] = item.Line
					}
				}
			}
		}
	}

	var r copyRecord
	if err := decodeStrictYAML(data, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", copyRecordName, err)
	}
	if err := validateCopyRecord(&r, pathLines); err != nil {
		return nil, fmt.Errorf("%s: %w", copyRecordName, err)
	}
	return &r, nil
}

func validateCopyRecord(r *copyRecord, pathLines map[string]int) error {
	switch {
	case r.Version == 0:
		return fmt.Errorf("version: %d is required", copyRecordVersion)
	case r.Version != copyRecordVersion:
		return fmt.Errorf("version is %d; this nsgo (%s) reads version %d", r.Version, displayVersion(toolVersion()), copyRecordVersion)
	}
	if err := module.CheckPath(r.Module); err != nil {
		return fmt.Errorf("invalid module %q: %w", r.Module, err)
	}
	if err := module.Check(r.Module, r.ModuleVersion); err != nil {
		return fmt.Errorf("invalid module_version %q: %w", r.ModuleVersion, err)
	}
	if !isValidCopyComponentName(r.Component) {
		return fmt.Errorf("invalid component name %q", r.Component)
	}

	seen := map[string]bool{}
	seenLower := map[string]string{}
	checkList := func(field string, paths []string) error {
		for _, p := range paths {
			linePrefix := ""
			if line := pathLines[p]; line > 0 {
				linePrefix = fmt.Sprintf("line %d: ", line)
			}
			if err := validateCopyRecordPath(p); err != nil {
				return fmt.Errorf("%s%s entry %q: %w", linePrefix, field, p, err)
			}
			if seen[p] {
				return fmt.Errorf("%sduplicate path %q", linePrefix, p)
			}
			low := strings.ToLower(p)
			if prev, ok := seenLower[low]; ok {
				return fmt.Errorf("%spath %q collides case-insensitively with %q", linePrefix, p, prev)
			}
			seen[p] = true
			seenLower[low] = p
		}
		return nil
	}
	if err := checkList("files", r.Files); err != nil {
		return err
	}
	if err := checkList("generated", r.Generated); err != nil {
		return err
	}
	return nil
}

// validateCopyRecordPath verifies that p is a clean, local, slash-separated
// relative path allowed by the copied-component file rules.
func validateCopyRecordPath(p string) error {
	if p == "" || strings.Contains(p, "\\") || strings.HasPrefix(p, "/") {
		return errors.New("must be a local slash-separated path")
	}
	if !filepath.IsLocal(filepath.FromSlash(p)) {
		return errors.New("path escapes the copy directory")
	}
	if err := module.CheckFilePath(p); err != nil {
		return err
	}
	return copyComponentFileAllowed(p)
}

// readCopyRecord reads and strictly validates nsgo-copy.yaml inside copyDir
// through os.Root, refusing symlinks and non-regular files. Returns (nil, nil)
// when nsgo-copy.yaml does not exist in copyDir.
func readCopyRecord(copyDir string) (*copyRecord, error) {
	info, err := os.Lstat(copyDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s: symlinks are not allowed", copyDir)
	}
	if !info.IsDir() {
		return nil, nil
	}
	root, err := os.OpenRoot(copyDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	st, err := root.Lstat(copyRecordName)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s: symlinks are not allowed", copyRecordName)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: must be a regular file", copyRecordName)
	}
	if st.Size() > maxCopyRecordBytes {
		return nil, fmt.Errorf("%s exceeds 64 KiB", copyRecordName)
	}
	f, err := root.Open(copyRecordName)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxCopyRecordBytes+1))
	if err != nil {
		return nil, err
	}
	return parseCopyRecord(data)
}

// findModuleCopies walks modRoot to locate every directory holding an
// nsgo-copy.yaml record, skipping vendor, testdata, nested modules, and
// directories starting with '.' or '_'.
func findModuleCopies(modRoot, appModPath string) ([]moduleCopy, error) {
	var copies []moduleCopy
	err := filepath.WalkDir(modRoot, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == modRoot {
			return nil
		}
		name := d.Name()
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			return fs.SkipDir
		}
		if st, err := os.Lstat(filepath.Join(p, "go.mod")); err == nil && !st.IsDir() {
			return fs.SkipDir
		}
		rec, err := readCopyRecord(p)
		if err != nil {
			relPath, _ := filepath.Rel(modRoot, p)
			return fmt.Errorf("%s/%s: %w", filepath.ToSlash(relPath), copyRecordName, err)
		}
		if rec != nil {
			relPath, err := filepath.Rel(modRoot, p)
			if err != nil {
				return err
			}
			slash := filepath.ToSlash(relPath)
			impPath := slash
			if appModPath != "" {
				impPath = appModPath + "/" + slash
			}
			copies = append(copies, moduleCopy{
				dir:        p,
				relDir:     slash,
				importPath: impPath,
				record:     rec,
			})
			// A copied component cannot hold another copied component.
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(copies, func(a, b moduleCopy) int {
		return cmp.Compare(a.relDir, b.relDir)
	})
	return copies, nil
}

// rejectCopyYAMLFeatures refuses YAML anchors, aliases, merge keys and
// duplicate mapping keys before struct decoding.
func rejectCopyYAMLFeatures(n *yaml.Node) error {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.AliasNode || n.Alias != nil {
		return fmt.Errorf("line %d: YAML aliases are not allowed", n.Line)
	}
	if n.Anchor != "" {
		return fmt.Errorf("line %d: YAML anchors are not allowed", n.Line)
	}
	if n.Tag == "!!merge" {
		return fmt.Errorf("line %d: YAML merge keys are not allowed", n.Line)
	}
	if n.Kind == yaml.MappingNode {
		seen := make(map[string]bool, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Tag == "!!merge" {
				return fmt.Errorf("line %d: YAML merge keys are not allowed", k.Line)
			}
			if k.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: YAML mapping keys must be scalars", k.Line)
			}
			if seen[k.Value] {
				return fmt.Errorf("line %d: duplicate key %q", k.Line, k.Value)
			}
			seen[k.Value] = true
		}
	}
	for _, child := range n.Content {
		if err := rejectCopyYAMLFeatures(child); err != nil {
			return err
		}
	}
	return nil
}

// isValidCopyComponentName reports whether s is 1 to 64 characters of [a-z0-9-],
// not starting or ending with '-'.
func isValidCopyComponentName(s string) bool {
	if len(s) < 1 || len(s) > 64 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// validCopyComponentRelPath checks that relPath is a clean slash-separated
// relative path whose elements consist only of ASCII letters, digits, '-', '_'
// and '.', with no element starting with '.' or '_'.
func validCopyComponentRelPath(relPath string) error {
	if relPath == "" || strings.HasPrefix(relPath, "/") || strings.Contains(relPath, "\\") {
		return fmt.Errorf("invalid component path %q", relPath)
	}
	for elem := range strings.SplitSeq(relPath, "/") {
		if elem == "" {
			return fmt.Errorf("path %q has an empty element", relPath)
		}
		if elem[0] == '.' || elem[0] == '_' {
			return fmt.Errorf("path element %q must not start with '.' or '_'", elem)
		}
		for i := range len(elem) {
			c := elem[i]
			ok := 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.'
			if !ok {
				return fmt.Errorf("path element %q contains disallowed character %q; only ASCII letters, digits, '-', '_' and '.' are allowed", elem, c)
			}
		}
	}
	return nil
}

var copyRefusedExactNames = map[string]string{
	"go.mod":            "nested module file is not allowed in a component",
	"go.sum":            "go.sum is not allowed in a component",
	"go.work":           "go.work is not allowed in a component",
	"go.work.sum":       "go.work.sum is not allowed in a component",
	"nsgo.yaml":         "nsgo.yaml is not allowed in a component",
	"nsgo-module.yaml":  "nsgo-module.yaml is not allowed in a component",
	"nsgo-natives.yaml": "nsgo-natives.yaml is not allowed in a component",
	"nsgo-copy.yaml":    "nsgo-copy.yaml is reserved for nsgo add and not allowed in a component",
	"package.json":      "package.json is not allowed in a component",
	"tsconfig.json":     "tsconfig.json is not allowed in a component",
	"jsconfig.json":     "jsconfig.json is not allowed in a component",
	"deno.json":         "deno.json is not allowed in a component",
	"composer.json":     "composer.json is not allowed in a component",
}

var copyRefusedSourceExtensions = map[string]bool{
	".s":       true,
	".sx":      true,
	".syso":    true,
	".c":       true,
	".h":       true,
	".m":       true,
	".mm":      true,
	".cc":      true,
	".cpp":     true,
	".cxx":     true,
	".hh":      true,
	".hpp":     true,
	".f":       true,
	".f90":     true,
	".swig":    true,
	".swigcxx": true,
	".wasm":    true,
}

var copyAllowedAssetExtensions = map[string]bool{
	".svg":   true,
	".png":   true,
	".jpg":   true,
	".jpeg":  true,
	".webp":  true,
	".gif":   true,
	".avif":  true,
	".ico":   true,
	".ttf":   true,
	".otf":   true,
	".woff2": true,
	".wav":   true,
	".mp3":   true,
	".ogg":   true,
	".txt":   true,
}

// copyComponentFileAllowed reports whether relPath (slash-separated, relative
// to the component root) is permitted by the copied-component path and file rules.
func copyComponentFileAllowed(relPath string) error {
	if err := validCopyComponentRelPath(relPath); err != nil {
		return err
	}
	low := strings.ToLower(relPath)
	dir := path.Dir(low)
	base := path.Base(low)
	ext := path.Ext(base)
	topDir := ""
	if dir != "." {
		topDir, _, _ = strings.Cut(low, "/")
	}
	if reason, bad := copyRefusedExactNames[base]; bad {
		return errors.New(reason)
	}
	if copyRefusedSourceExtensions[ext] {
		return fmt.Errorf("%s files are not allowed in a component", ext)
	}
	if ext == ".go" {
		if dir == "." || dir == "locales" {
			return nil
		}
		return fmt.Errorf("Go files are only allowed at the component root and in locales/; %q would be a second package", path.Dir(relPath))
	}
	if _, ok := isLicenseDocument(path.Base(relPath)); ok {
		return nil
	}
	if ext == ".yaml" || ext == ".yml" {
		if dir == "locales" || topDir == "testdata" {
			return nil
		}
		return errors.New("YAML files are only allowed in locales/ and testdata/")
	}
	if low == "readme.md" {
		return nil
	}
	if ext == ".json" {
		if dir == "." || topDir == "locales" {
			return errors.New("JSON files are only allowed in testdata/ and asset subdirectories")
		}
		return nil
	}
	if copyAllowedAssetExtensions[ext] {
		return nil
	}
	return errors.New("file type is not allowed in a component")
}
