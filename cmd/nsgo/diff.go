package main

import (
	"archive/zip"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	"golang.org/x/mod/sumdb/dirhash"
	modzip "golang.org/x/mod/zip"
)

// nsgo diff shows what an app changed in a copied component, what another
// version of the component would change, or a summary of all copies in the
// module (#2468, #2459).

const (
	diffManifestName       = "nsgo-module.yaml"
	maxDiffManifestBytes   = 1 << 20 // 1 MiB
	maxDiffComponentFileSz = 1 << 20 // 1 MiB
	maxDiffComponentTotal  = 4 << 20 // 4 MiB
)

// diffOptions are nsgo diff's flags.
type diffOptions struct {
	stat     bool // print changed lines per file
	exitCode bool // exit with status 1 when there are differences
}

// diffIndexItem is a registry component item resolved by bare name when no
// local record matches directly.
type diffIndexItem struct {
	modulePath string
	component  string
}

// lookupDiffIndexItem resolves a bare registry name to a component when the
// module's records do not match directly. Replaced by the built-in registry
// when available.
var lookupDiffIndexItem = func(name string) (diffIndexItem, bool) {
	return diffIndexItem{}, false
}

// diffModuleContext holds the host module environment for nsgo diff.
type diffModuleContext struct {
	dir        string
	modPath    string
	goCmd      string
	env        []string
	modCache   string
	goSum      []byte
	compBase   string // default components base directory (relative to mod root, slash-separated)
	zipCache   map[string]*diffZipModule
	unavailErr map[string]error
}

func (m *diffModuleContext) close() {
	for _, dm := range m.zipCache {
		dm.close()
	}
}

// diffUnavailableError signals that a module version could not be fetched from
// the module cache or proxy (as distinct from a corrupt or tampered zip).
type diffUnavailableError struct {
	modPath string
	version string
	reason  string
}

func (e *diffUnavailableError) Error() string {
	if e.reason != "" {
		return fmt.Sprintf("%s@%s: %s", e.modPath, e.version, e.reason)
	}
	return fmt.Sprintf("%s@%s is unavailable", e.modPath, e.version)
}

// diffZipModule is a verified module zip ready to read component files from.
type diffZipModule struct {
	path        string
	version     string
	zipPath     string
	sum         string
	manifest    *diffModuleManifest
	zr          *zip.ReadCloser
	entries     map[string]*zip.File
	sortedPaths []string
}

func (dm *diffZipModule) close() {
	if dm != nil && dm.zr != nil {
		dm.zr.Close()
		dm.zr = nil
	}
}

func (dm *diffZipModule) readFile(relPath string, limit int64) ([]byte, error) {
	zf, ok := dm.entries[relPath]
	if !ok {
		return nil, fs.ErrNotExist
	}
	if limit > 0 && int64(zf.UncompressedSize64) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", relPath, limit)
	}
	rc, err := zf.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	if limit > 0 {
		data, err := io.ReadAll(io.LimitReader(rc, limit+1))
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > limit {
			return nil, fmt.Errorf("%s exceeds %d bytes", relPath, limit)
		}
		return data, nil
	}
	return io.ReadAll(rc)
}

type diffModuleManifest struct {
	Version    int               `yaml:"version"`
	Import     []string          `yaml:"import,omitempty"`
	Targets    []string          `yaml:"targets,omitempty"`
	Notes      string            `yaml:"notes,omitempty"`
	Components map[string]string `yaml:"components,omitempty"`
}

// diffCommand executes nsgo diff and returns whether any differences were found.
func diffCommand(ctx context.Context, o diffOptions, arg string, out, errOut io.Writer) (bool, error) {
	m, err := findDiffModule(ctx)
	if err != nil {
		return false, err
	}
	defer m.close()

	// No argument: list every copy, its version, and whether it is edited
	// (or with -stat, print changed lines per file across all copies).
	if arg == "" {
		copies, err := findModuleCopies(m.dir, m.modPath)
		if err != nil {
			return false, fmt.Errorf("diff: %w", err)
		}
		return diffListCopies(ctx, m, o, copies, out, errOut)
	}

	parsed, err := parseDiffArg(arg)
	if err != nil {
		return false, err
	}

	// If the target directly names an existing directory, validate its record
	// immediately so a broken nsgo-copy.yaml in that directory reports clearly.
	directCopy, isDir, err := tryDirectCopyDir(m.dir, m.modPath, parsed.target)
	if err != nil {
		return false, fmt.Errorf("diff: %w", err)
	}

	copies, err := findModuleCopies(m.dir, m.modPath)
	if err != nil {
		return false, fmt.Errorf("diff: %w", err)
	}

	targetCopy, err := resolveDiffTarget(parsed, directCopy, isDir, copies)
	if err != nil {
		return false, err
	}

	if parsed.hasVersion {
		return diffAgainstVersion(ctx, m, o, targetCopy, copies, parsed.version, out, errOut)
	}
	return diffWorkingCopy(ctx, m, o, targetCopy, copies, out, errOut)
}

// diffArg is the parsed [name | dir][@version] (or module[@version]#component)
// positional argument of nsgo diff.
type diffArg struct {
	raw        string
	target     string // directory path, bare name, or module#component
	modulePath string // non-empty when arg used module[#component] syntax
	component  string // non-empty when arg used module#component syntax
	version    string
	hasVersion bool
}

func parseDiffArg(arg string) (diffArg, error) {
	if arg == "" {
		return diffArg{}, nil
	}
	if strings.HasPrefix(arg, "-") {
		return diffArg{}, fmt.Errorf("diff: flags go before the component: %s diff %s", nsgoCommand(), arg)
	}

	// If arg literally exists as a directory holding nsgo-copy.yaml, do not
	// split on '@' inside the directory path.
	if info, err := os.Lstat(arg); err == nil && info.IsDir() {
		if _, recErr := os.Lstat(filepath.Join(arg, copyRecordName)); recErr == nil {
			return diffArg{raw: arg, target: arg}, nil
		}
	}

	// Support module[@version]#component and module#component@version forms.
	if strings.Contains(arg, "#") {
		left, right, _ := strings.Cut(arg, "#")
		modPart, ver1, hasVer1 := strings.Cut(left, "@")
		compPart, ver2, hasVer2 := strings.Cut(right, "@")
		if hasVer1 && hasVer2 {
			return diffArg{}, fmt.Errorf("diff: %q specifies two versions", arg)
		}
		version := ver1
		hasVer := hasVer1
		if hasVer2 {
			version = ver2
			hasVer = true
		}
		if err := module.CheckPath(modPart); err != nil {
			return diffArg{}, fmt.Errorf("diff: %q: %w", arg, err)
		}
		if !isValidCopyComponentName(compPart) {
			return diffArg{}, fmt.Errorf("diff: %q: invalid component name %q", arg, compPart)
		}
		if hasVer {
			if err := validateDiffVersionQuery(arg, version); err != nil {
				return diffArg{}, err
			}
		}
		return diffArg{
			raw:        arg,
			target:     modPart + "#" + compPart,
			modulePath: modPart,
			component:  compPart,
			version:    version,
			hasVersion: hasVer,
		}, nil
	}

	// Standard [name | dir][@version]: split on the last '@' after index 0
	// so @registry/name[@version] stays intact.
	target := arg
	var version string
	var hasVersion bool
	if idx := strings.LastIndex(arg, "@"); idx > 0 {
		target = arg[:idx]
		version = arg[idx+1:]
		hasVersion = true
		if err := validateDiffVersionQuery(arg, version); err != nil {
			return diffArg{}, err
		}
	}
	if target == "" {
		return diffArg{}, fmt.Errorf("diff: %q is not a component name or directory", arg)
	}
	return diffArg{
		raw:        arg,
		target:     target,
		version:    version,
		hasVersion: hasVersion,
	}, nil
}

func validateDiffVersionQuery(arg, version string) error {
	if version == "" {
		return fmt.Errorf("diff: %q has an empty version after @", arg)
	}
	if version == "none" {
		return fmt.Errorf("diff: %q: @none is not a valid version to diff against", arg)
	}
	if strings.ContainsAny(version, " \t\r\n/\\") {
		return fmt.Errorf("diff: %q: invalid version %q", arg, version)
	}
	return nil
}

// tryDirectCopyDir checks whether target names an existing directory (relative
// to cwd or the module root). If it is a directory and holds nsgo-copy.yaml,
// it parses and returns the moduleCopy.
func tryDirectCopyDir(modRoot, appModPath, target string) (*moduleCopy, bool, error) {
	if target == "" || strings.Contains(target, "#") {
		return nil, false, nil
	}
	candidates := []string{target}
	if !filepath.IsAbs(target) {
		if absCwd, err := filepath.Abs(target); err == nil {
			candidates = []string{absCwd}
		}
		fromMod := filepath.Join(modRoot, filepath.FromSlash(target))
		if len(candidates) == 0 || candidates[0] != fromMod {
			candidates = append(candidates, fromMod)
		}
	}
	for _, cand := range candidates {
		info, err := os.Lstat(cand)
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, true, fmt.Errorf("%s: symlinks are not allowed", target)
		}
		if !info.IsDir() {
			continue
		}
		absDir, err := filepath.Abs(cand)
		if err != nil {
			return nil, true, err
		}
		rec, err := readCopyRecord(absDir)
		if err != nil {
			relPath, relErr := filepath.Rel(modRoot, absDir)
			if relErr == nil && filepath.IsLocal(relPath) {
				return nil, true, fmt.Errorf("%s/%s: %w", filepath.ToSlash(relPath), copyRecordName, err)
			}
			return nil, true, fmt.Errorf("%s/%s: %w", target, copyRecordName, err)
		}
		if rec == nil {
			return nil, true, nil
		}
		relPath, err := filepath.Rel(modRoot, absDir)
		if err != nil || relPath == "." || !filepath.IsLocal(relPath) {
			return nil, true, fmt.Errorf("%s is outside this module", target)
		}
		slash := filepath.ToSlash(relPath)
		impPath := slash
		if appModPath != "" {
			impPath = appModPath + "/" + slash
		}
		return &moduleCopy{
			dir:        absDir,
			relDir:     slash,
			importPath: impPath,
			record:     rec,
		}, true, nil
	}
	return nil, false, nil
}

// resolveDiffTarget matches parsed against the module's copied components:
//  1. a directory holding nsgo-copy.yaml;
//  2. offline match against module records (unique component name or relDir);
//  3. add-style resolution (module#component or registry lookup) matched by
//     module and component.
func resolveDiffTarget(parsed diffArg, directCopy *moduleCopy, isDir bool, copies []moduleCopy) (moduleCopy, error) {
	if directCopy != nil {
		return *directCopy, nil
	}
	if isDir {
		return moduleCopy{}, fmt.Errorf("diff: %s is not a copied component (no %s)", parsed.target, copyRecordName)
	}

	// Explicit module#component match.
	if parsed.modulePath != "" && parsed.component != "" {
		var matches []moduleCopy
		for _, c := range copies {
			if c.record.Module == parsed.modulePath && c.record.Component == parsed.component {
				matches = append(matches, c)
			}
		}
		return selectUniqueCopyMatch(parsed.target, matches)
	}

	// Exact relative directory path match (e.g. "components/chat").
	cleanSlash := filepath.ToSlash(filepath.Clean(parsed.target))
	for _, c := range copies {
		if c.relDir == cleanSlash {
			return c, nil
		}
	}

	// Offline match by component name among the module's records.
	var byName []moduleCopy
	for _, c := range copies {
		if c.record.Component == parsed.target {
			byName = append(byName, c)
		}
	}
	if len(byName) == 1 {
		return byName[0], nil
	}
	if len(byName) > 1 {
		return selectUniqueCopyMatch(parsed.target, byName)
	}

	// Fallback: resolve as add resolves a bare/registry name and match by
	// module and component.
	if item, ok := lookupDiffIndexItem(parsed.target); ok && item.component != "" {
		var byItem []moduleCopy
		for _, c := range copies {
			if c.record.Module == item.modulePath && c.record.Component == item.component {
				byItem = append(byItem, c)
			}
		}
		if len(byItem) > 0 {
			return selectUniqueCopyMatch(parsed.target, byItem)
		}
	}

	if strings.ContainsAny(parsed.target, "/\\") || parsed.target == "." || parsed.target == ".." {
		return moduleCopy{}, fmt.Errorf("diff: %s is not a copied component (no %s)", parsed.target, copyRecordName)
	}
	return moduleCopy{}, fmt.Errorf("diff: no copied component named %q", parsed.target)
}

func selectUniqueCopyMatch(name string, matches []moduleCopy) (moduleCopy, error) {
	switch len(matches) {
	case 0:
		return moduleCopy{}, fmt.Errorf("diff: no copied component matches %q", name)
	case 1:
		return matches[0], nil
	default:
		dirs := make([]string, len(matches))
		for i, c := range matches {
			dirs[i] = c.relDir
		}
		return moduleCopy{}, fmt.Errorf("diff: %q matches %d copied components (%s); pass the copy's directory instead", name, len(matches), strings.Join(dirs, ", "))
	}
}

// findDiffModule locates the current module, its toolchain's go command, proxy-only
// environment, GOMODCACHE, go.mod module path and go.sum.
func findDiffModule(ctx context.Context) (*diffModuleContext, error) {
	found, err := goEnvValues(ctx, "go", ".", append(os.Environ(), "GOTOOLCHAIN=local"), "GOMOD")
	if err != nil {
		return nil, fmt.Errorf("diff: %w", err)
	}
	if found["GOMOD"] == "" || found["GOMOD"] == os.DevNull {
		return nil, fmt.Errorf("diff: not in a module: run %s diff inside a Go module", nsgoCommand())
	}
	modDir := filepath.Dir(found["GOMOD"])

	alone := append(os.Environ(), "GOWORK=off")
	vars, err := goEnvValues(ctx, "go", modDir, alone, "GOROOT", "GOPROXY", "GOMODCACHE")
	if err != nil {
		return nil, fmt.Errorf("diff: %w", err)
	}
	goCmd := filepath.Join(vars["GOROOT"], "bin", "go")
	if runtime.GOOS == "windows" {
		goCmd += ".exe"
	}
	if _, err := os.Stat(goCmd); err != nil {
		return nil, fmt.Errorf("diff: the module's toolchain has no go command: %w", err)
	}

	rawMod, err := os.ReadFile(filepath.Join(modDir, "go.mod"))
	if err != nil {
		return nil, fmt.Errorf("diff: %w", err)
	}
	mf, err := modfile.Parse(filepath.Join(modDir, "go.mod"), rawMod, nil)
	if err != nil {
		return nil, fmt.Errorf("diff: %w", err)
	}
	if mf.Module == nil {
		return nil, fmt.Errorf("diff: %s has no module line", filepath.Join(modDir, "go.mod"))
	}
	goSum, err := os.ReadFile(filepath.Join(modDir, "go.sum"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("diff: %w", err)
	}

	cwd, _ := os.Getwd()
	compBase := diffComponentsBaseDir(cwd, modDir)

	return &diffModuleContext{
		dir:        modDir,
		modPath:    mf.Module.Mod.Path,
		goCmd:      goCmd,
		env:        append(alone, "GOTOOLCHAIN=local", "GOROOT="+vars["GOROOT"], "GOPROXY="+initModuleProxy(vars["GOPROXY"])),
		modCache:   vars["GOMODCACHE"],
		goSum:      goSum,
		compBase:   compBase,
		zipCache:   map[string]*diffZipModule{},
		unavailErr: map[string]error{},
	}, nil
}

// diffComponentsBaseDir resolves the default base directory for components
// (nsgo.yaml's components.dir relative to nsgo.yaml, else "components") as a
// slash-separated path relative to modDir.
func diffComponentsBaseDir(cwd, modDir string) string {
	for _, dir := range []string{cwd, modDir} {
		if dir == "" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, "nsgo.yaml"))
		if err != nil {
			continue
		}
		var cfg struct {
			Components struct {
				Dir string `yaml:"dir"`
			} `yaml:"components"`
		}
		if yaml.Unmarshal(raw, &cfg) == nil && cfg.Components.Dir != "" {
			abs := cfg.Components.Dir
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(dir, abs)
			}
			if rel, err := filepath.Rel(modDir, abs); err == nil && rel != "." && filepath.IsLocal(rel) {
				return filepath.ToSlash(rel)
			}
		}
		if rel, err := filepath.Rel(modDir, filepath.Join(dir, "components")); err == nil && filepath.IsLocal(rel) {
			return filepath.ToSlash(rel)
		}
		break
	}
	return "components"
}

// diffListCopies implements `nsgo diff` with no component argument.
func diffListCopies(ctx context.Context, m *diffModuleContext, o diffOptions, copies []moduleCopy, out, errOut io.Writer) (bool, error) {
	if len(copies) == 0 {
		return false, nil
	}

	type copyStatus struct {
		copy        moduleCopy
		diffs       []fileDiff
		unavailable string
	}
	statuses := make([]copyStatus, 0, len(copies))
	anyChanged := false
	var allDiffs []fileDiff

	for _, c := range copies {
		localFiles, err := readLocalCopyFiles(c.dir, c.record)
		if err != nil {
			return false, fmt.Errorf("diff: %s: %w", c.relDir, err)
		}
		baseFiles, unavail, err := reconstructRecordedBase(ctx, m, c, copies)
		if err != nil {
			return false, fmt.Errorf("diff: %s: %w", c.relDir, err)
		}
		diffs := compareCopyTrees(c.relDir, baseFiles, localFiles, true)
		if len(diffs) > 0 || unavail != "" {
			anyChanged = true
		}
		allDiffs = append(allDiffs, diffs...)
		statuses = append(statuses, copyStatus{
			copy:        c,
			diffs:       diffs,
			unavailable: unavail,
		})
	}

	if o.stat {
		for _, st := range statuses {
			if st.unavailable != "" {
				writeDiffUnavailableWarning(errOut, st.copy, st.unavailable)
			}
		}
		if len(allDiffs) > 0 {
			if _, err := io.WriteString(out, formatDiffStat(allDiffs)); err != nil {
				return anyChanged, err
			}
		}
		return anyChanged, nil
	}

	maxDir := 0
	maxVer := 0
	for _, st := range statuses {
		if len(st.copy.relDir) > maxDir {
			maxDir = len(st.copy.relDir)
		}
		if len(st.copy.record.ModuleVersion) > maxVer {
			maxVer = len(st.copy.record.ModuleVersion)
		}
	}
	for _, st := range statuses {
		state := "unedited"
		if st.unavailable != "" {
			state = fmt.Sprintf("edited (unavailable: %s)", st.unavailable)
		} else if len(st.diffs) > 0 {
			state = "edited"
		}
		if _, err := fmt.Fprintf(out, "%-*s  %-*s  %s\n", maxDir, st.copy.relDir, maxVer, st.copy.record.ModuleVersion, state); err != nil {
			return anyChanged, err
		}
	}
	return anyChanged, nil
}

// diffWorkingCopy compares the recorded base version of c against c.dir on disk.
func diffWorkingCopy(ctx context.Context, m *diffModuleContext, o diffOptions, c moduleCopy, copies []moduleCopy, out, errOut io.Writer) (bool, error) {
	localFiles, err := readLocalCopyFiles(c.dir, c.record)
	if err != nil {
		return false, fmt.Errorf("diff: %s: %w", c.relDir, err)
	}
	baseFiles, unavail, err := reconstructRecordedBase(ctx, m, c, copies)
	if err != nil {
		return false, fmt.Errorf("diff: %s: %w", c.relDir, err)
	}
	if unavail != "" {
		writeDiffUnavailableWarning(errOut, c, unavail)
	}
	diffs := compareCopyTrees(c.relDir, baseFiles, localFiles, true)
	if len(diffs) == 0 {
		return unavail != "", nil
	}
	if o.stat {
		_, err = io.WriteString(out, formatDiffStat(diffs))
	} else {
		_, err = io.WriteString(out, formatUnifiedDiff(diffs))
	}
	return true, err
}

// diffAgainstVersion compares the copy of the recorded version against the copy
// of targetQuery (such as v0.5.0 or upgrade).
func diffAgainstVersion(ctx context.Context, m *diffModuleContext, o diffOptions, c moduleCopy, copies []moduleCopy, targetQuery string, out, errOut io.Writer) (bool, error) {
	targetMod, err := m.loadModule(ctx, c.record.Module, targetQuery, c.record.ModuleVersion)
	if err != nil {
		return false, fmt.Errorf("diff: %w", err)
	}
	newFiles, err := reconstructCopyFiles(m, targetMod, c, copies)
	if err != nil {
		return false, fmt.Errorf("diff: %w", err)
	}

	oldFiles, unavail, err := reconstructRecordedBase(ctx, m, c, copies)
	if err != nil {
		return false, fmt.Errorf("diff: %w", err)
	}
	if unavail != "" {
		writeDiffUnavailableWarning(errOut, c, unavail)
	}

	diffs := compareCopyTrees(c.relDir, oldFiles, newFiles, false)
	if len(diffs) == 0 {
		return unavail != "", nil
	}
	if o.stat {
		_, err = io.WriteString(out, formatDiffStat(diffs))
	} else {
		_, err = io.WriteString(out, formatUnifiedDiff(diffs))
	}
	return true, err
}

func writeDiffUnavailableWarning(errOut io.Writer, c moduleCopy, reason string) {
	if errOut == nil {
		return
	}
	fmt.Fprintf(errOut, "  warn  %s: recorded version %s@%s is unavailable (%s); every file counts as edited\n",
		c.relDir, c.record.Module, c.record.ModuleVersion, reason)
}

// reconstructRecordedBase loads c.record.Module@c.record.ModuleVersion and
// reconstructs its clean copied files. When the recorded version cannot be
// downloaded (cache miss + proxy failure), it returns an empty map and a
// non-empty reason string so every file counts as edited.
func reconstructRecordedBase(ctx context.Context, m *diffModuleContext, c moduleCopy, copies []moduleCopy) (map[string][]byte, string, error) {
	dm, err := m.loadModule(ctx, c.record.Module, c.record.ModuleVersion, "")
	if err != nil {
		var unavail *diffUnavailableError
		if errors.As(err, &unavail) {
			return map[string][]byte{}, unavail.reason, nil
		}
		return nil, "", err
	}
	files, err := reconstructCopyFiles(m, dm, c, copies)
	if err != nil {
		return nil, "", err
	}
	return files, "", nil
}

// loadModule loads modPath@query, checking GOMODCACHE first for canonical
// versions (so offline use works with GOPROXY=off) and falling back to
// go mod download -json under GOTOOLCHAIN=local and proxy-only GOPROXY.
// Every zip is verified with x/mod/zip.CheckZip and dirhash.HashZip.
func (m *diffModuleContext) loadModule(ctx context.Context, modPath, query, minUpgradeVer string) (*diffZipModule, error) {
	dlQuery := query
	if query == "upgrade" {
		dlQuery = "latest"
	}
	cacheKey := modPath + "@" + dlQuery
	if dm, ok := m.zipCache[cacheKey]; ok {
		if query == "upgrade" && minUpgradeVer != "" && semver.Compare(minUpgradeVer, dm.version) > 0 {
			return m.loadModule(ctx, modPath, minUpgradeVer, "")
		}
		return dm, nil
	}
	if prevErr, ok := m.unavailErr[cacheKey]; ok {
		return nil, prevErr
	}

	// Cache-first lookup for exact canonical versions so offline diff works
	// without spawning go mod download when the zip is already cached.
	if dlQuery != "latest" && dlQuery != "upgrade" && m.modCache != "" {
		if dm, found, err := m.openCachedZip(modPath, dlQuery); err != nil {
			return nil, err
		} else if found {
			m.zipCache[cacheKey] = dm
			m.zipCache[modPath+"@"+dm.version] = dm
			return dm, nil
		}
	}

	dm, err := m.downloadVerifiedZip(ctx, modPath, dlQuery)
	if err != nil {
		var unavail *diffUnavailableError
		if errors.As(err, &unavail) {
			m.unavailErr[cacheKey] = err
		}
		return nil, err
	}
	if query == "upgrade" && minUpgradeVer != "" && semver.Compare(minUpgradeVer, dm.version) > 0 {
		dm.close()
		return m.loadModule(ctx, modPath, minUpgradeVer, "")
	}
	m.zipCache[cacheKey] = dm
	m.zipCache[modPath+"@"+dm.version] = dm
	return dm, nil
}

// openCachedZip opens and verifies a module zip from GOMODCACHE/cache/download.
func (m *diffModuleContext) openCachedZip(modPath, version string) (*diffZipModule, bool, error) {
	escPath, err := module.EscapePath(modPath)
	if err != nil {
		return nil, false, nil
	}
	escVer, err := module.EscapeVersion(version)
	if err != nil {
		return nil, false, nil
	}
	zipPath := filepath.Join(m.modCache, "cache", "download", filepath.FromSlash(escPath), "@v", escVer+".zip")
	st, err := os.Lstat(zipPath)
	if errors.Is(err, fs.ErrNotExist) || err != nil || !st.Mode().IsRegular() {
		return nil, false, nil
	}
	var expectedSum string
	if hashBytes, err := os.ReadFile(zipPath + "hash"); err == nil {
		expectedSum = strings.TrimSpace(string(hashBytes))
	}
	dm, err := openAndVerifyModuleZip(modPath, version, zipPath, expectedSum, m.goSum)
	if err != nil {
		return nil, true, err
	}
	return dm, true, nil
}

// downloadVerifiedZip runs go mod download -json in an isolated temporary
// directory under proxy-only, GOTOOLCHAIN=local settings and verifies the zip.
func (m *diffModuleContext) downloadVerifiedZip(ctx context.Context, modPath, query string) (*diffZipModule, error) {
	tmp, err := os.MkdirTemp("", "nsgo-diff-dl-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	cmd := exec.CommandContext(ctx, m.goCmd, "mod", "download", "-json", modPath+"@"+query)
	cmd.Dir = tmp
	cmd.Env = m.env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, runErr := cmd.Output()

	var dl struct {
		Path    string
		Version string
		Zip     string
		Sum     string
		Error   string
	}
	decErr := json.Unmarshal(out, &dl)
	if runErr != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		reason := strings.TrimSpace(dl.Error)
		if reason == "" {
			reason = strings.TrimSpace(goMessages(stderr.String()))
		}
		if reason == "" {
			reason = runErr.Error()
		}
		return nil, &diffUnavailableError{modPath: modPath, version: query, reason: reason}
	}
	if decErr != nil {
		return nil, fmt.Errorf("download %s@%s: %w", modPath, query, decErr)
	}
	if dl.Error != "" {
		return nil, &diffUnavailableError{modPath: modPath, version: query, reason: strings.TrimSpace(dl.Error)}
	}
	if dl.Zip == "" {
		return nil, &diffUnavailableError{modPath: modPath, version: query, reason: "download returned no zip archive"}
	}
	return openAndVerifyModuleZip(dl.Path, dl.Version, dl.Zip, dl.Sum, m.goSum)
}

// openAndVerifyModuleZip verifies a module zip with x/mod/zip.CheckZip and
// dirhash.HashZip (against expectedSum and appGoSum) and parses nsgo-module.yaml.
func openAndVerifyModuleZip(modPath, version, zipPath, expectedSum string, appGoSum []byte) (*diffZipModule, error) {
	mv := module.Version{Path: modPath, Version: version}
	if _, err := modzip.CheckZip(mv, zipPath); err != nil {
		return nil, fmt.Errorf("%s@%s: invalid module zip: %w", modPath, version, err)
	}
	actualHash, err := dirhash.HashZip(zipPath, dirhash.Hash1)
	if err != nil {
		return nil, fmt.Errorf("%s@%s: hash zip: %w", modPath, version, err)
	}
	if expectedSum != "" && actualHash != expectedSum {
		return nil, fmt.Errorf("%s@%s: zip hash mismatch: archive hashes to %s, expected %s", modPath, version, actualHash, expectedSum)
	}
	if sumHash := diffGoSumEntry(appGoSum, modPath, version); sumHash != "" && actualHash != sumHash {
		return nil, fmt.Errorf("%s@%s: go.sum mismatch: archive hashes to %s, go.sum has %s", modPath, version, actualHash, sumHash)
	}

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("%s@%s: open zip: %w", modPath, version, err)
	}
	dm := &diffZipModule{
		path:    modPath,
		version: version,
		zipPath: zipPath,
		sum:     actualHash,
		zr:      zr,
		entries: make(map[string]*zip.File, len(zr.File)),
	}
	prefix := modPath + "@" + version + "/"
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		relPath, ok := strings.CutPrefix(f.Name, prefix)
		if !ok || relPath == "" {
			continue
		}
		dm.entries[relPath] = f
		dm.sortedPaths = append(dm.sortedPaths, relPath)
	}
	slices.Sort(dm.sortedPaths)

	goModRaw, err := dm.readFile("go.mod", maxDiffManifestBytes)
	if err != nil {
		dm.close()
		return nil, fmt.Errorf("%s@%s: read go.mod from zip: %w", modPath, version, err)
	}
	if modSum := diffGoSumEntry(appGoSum, modPath, version+"/go.mod"); modSum != "" {
		actualModSum, err := dirhash.Hash1([]string{"go.mod"}, func(string) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(goModRaw)), nil
		})
		if err == nil && actualModSum != modSum {
			dm.close()
			return nil, fmt.Errorf("%s@%s: go.sum mismatch for go.mod: archive hashes to %s, go.sum has %s", modPath, version, actualModSum, modSum)
		}
	}

	if manifestRaw, err := dm.readFile(diffManifestName, maxDiffManifestBytes); err == nil {
		manifest, err := parseDiffModuleManifest(manifestRaw)
		if err != nil {
			dm.close()
			return nil, fmt.Errorf("%s@%s: %w", modPath, version, err)
		}
		dm.manifest = manifest
	} else if !errors.Is(err, fs.ErrNotExist) {
		dm.close()
		return nil, fmt.Errorf("%s@%s: %w", modPath, version, err)
	}

	return dm, nil
}

func diffGoSumEntry(goSum []byte, modPath, version string) string {
	for line := range strings.SplitSeq(string(goSum), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == modPath && fields[1] == version {
			return fields[2]
		}
	}
	return ""
}

func parseDiffModuleManifest(data []byte) (*diffModuleManifest, error) {
	if len(data) > maxDiffManifestBytes {
		return nil, fmt.Errorf("%s exceeds 1 MiB", diffManifestName)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("%s: empty YAML file", diffManifestName)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%s: %w", diffManifestName, err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("%s: multiple YAML documents are not allowed", diffManifestName)
		}
		return nil, fmt.Errorf("%s: %w", diffManifestName, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: must be a YAML mapping", diffManifestName)
	}
	if err := rejectCopyYAMLFeatures(&doc); err != nil {
		return nil, fmt.Errorf("%s: %w", diffManifestName, err)
	}
	var m diffModuleManifest
	if err := decodeStrictYAML(data, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", diffManifestName, err)
	}
	if m.Version != 1 {
		return nil, fmt.Errorf("%s: unsupported version %d; this nsgo supports version 1", diffManifestName, m.Version)
	}
	for name, dir := range m.Components {
		if !isValidCopyComponentName(name) {
			return nil, fmt.Errorf("%s: invalid component name %q", diffManifestName, name)
		}
		if dir == "" || strings.HasPrefix(dir, "/") || strings.Contains(dir, "\\") || !filepath.IsLocal(filepath.FromSlash(dir)) || dir == "." {
			return nil, fmt.Errorf("%s: invalid component directory %q for %q", diffManifestName, dir, name)
		}
		if err := module.CheckFilePath(dir); err != nil {
			return nil, fmt.Errorf("%s: invalid component directory %q for %q: %w", diffManifestName, dir, name, err)
		}
	}
	return &m, nil
}

// reconstructCopyFiles builds the exact map of relative path -> bytes that #2467
// writes for dm's component c.record.Component (with c.record.Tests, placed at
// c.importPath with other copies where their records are now, formatted, and
// without generated files).
func reconstructCopyFiles(m *diffModuleContext, dm *diffZipModule, c moduleCopy, allCopies []moduleCopy) (map[string][]byte, error) {
	if dm.manifest == nil || dm.manifest.Components == nil {
		return nil, fmt.Errorf("%s@%s declares no component %q", dm.path, dm.version, c.record.Component)
	}
	srcDir, ok := dm.manifest.Components[c.record.Component]
	if !ok {
		return nil, fmt.Errorf("%s@%s declares no component %q", dm.path, dm.version, c.record.Component)
	}

	// Build import rewrite table matching #2467:
	// - this component and its locales/ -> c.importPath
	// - every other component of the same module -> where its record is now,
	//   else its default destination under m.compBase.
	rewrites := map[string]string{
		dm.path + "/" + srcDir:              c.importPath,
		dm.path + "/" + srcDir + "/locales": c.importPath + "/locales",
	}
	copyByComp := map[string]moduleCopy{}
	for _, other := range allCopies {
		if other.record.Module == dm.path {
			copyByComp[other.record.Component] = other
		}
	}
	for otherComp, otherSrcDir := range dm.manifest.Components {
		if otherSrcDir == srcDir {
			continue
		}
		var destImp string
		if existing, ok := copyByComp[otherComp]; ok {
			destImp = existing.importPath
		} else {
			pkgName := path.Base(otherSrcDir)
			destImp = m.modPath + "/" + path.Join(m.compBase, pkgName)
		}
		rewrites[dm.path+"/"+otherSrcDir] = destImp
		rewrites[dm.path+"/"+otherSrcDir+"/locales"] = destImp + "/locales"
	}

	// Collect component files from zip.
	prefix := srcDir + "/"
	type rawEntry struct {
		relPath string
		data    []byte
	}
	var rawEntries []rawEntry
	var totalBytes int64
	for _, p := range dm.sortedPaths {
		relPath, ok := strings.CutPrefix(p, prefix)
		if !ok || relPath == "" {
			continue
		}
		if !c.record.Tests && isComponentTestFile(relPath) {
			continue
		}
		if err := componentFileAllowed(relPath); err != nil {
			continue
		}
		zf := dm.entries[p]
		size := int64(zf.UncompressedSize64)
		if size > maxDiffComponentFileSz {
			return nil, fmt.Errorf("%s@%s: %s exceeds 1 MiB", dm.path, dm.version, p)
		}
		totalBytes += size
		if totalBytes > maxDiffComponentTotal {
			return nil, fmt.Errorf("%s@%s: component %q exceeds 4 MiB", dm.path, dm.version, c.record.Component)
		}
		data, err := dm.readFile(p, maxDiffComponentFileSz)
		if err != nil {
			return nil, fmt.Errorf("%s@%s: read %s: %w", dm.path, dm.version, p, err)
		}
		// Skip generated catalog files (locales/catalog_gen.go).
		if isGeneratedCatalogFile(relPath, data) {
			continue
		}
		rawEntries = append(rawEntries, rawEntry{relPath: relPath, data: data})
	}
	if len(rawEntries) == 0 {
		return nil, fmt.Errorf("%s@%s: component directory %q has no files", dm.path, dm.version, srcDir)
	}

	// Collect ancestor license documents (module root and directories strictly
	// between module root and srcDir), placed at the copy's root.
	seenRootLower := map[string]string{}
	for _, e := range rawEntries {
		if path.Dir(e.relPath) == "." {
			seenRootLower[strings.ToLower(e.relPath)] = srcDir + "/" + e.relPath
		}
	}
	ancestors := []string{""}
	parts := strings.Split(srcDir, "/")
	for i := 1; i < len(parts); i++ {
		ancestors = append(ancestors, strings.Join(parts[:i], "/"))
	}
	for _, dir := range ancestors {
		for _, p := range dm.sortedPaths {
			parent := path.Dir(p)
			if parent == "." {
				parent = ""
			}
			if parent != dir {
				continue
			}
			base := path.Base(p)
			if _, include := isLicenseDocument(base); !include {
				continue
			}
			if err := componentFileAllowed(base); err != nil {
				continue
			}
			low := strings.ToLower(base)
			if _, dup := seenRootLower[low]; dup {
				continue
			}
			seenRootLower[low] = p
			data, err := dm.readFile(p, maxDiffComponentFileSz)
			if err != nil {
				return nil, fmt.Errorf("%s@%s: read %s: %w", dm.path, dm.version, p, err)
			}
			if !utf8.Valid(data) {
				return nil, fmt.Errorf("%s@%s: %s is not valid UTF-8", dm.path, dm.version, p)
			}
			rawEntries = append(rawEntries, rawEntry{relPath: base, data: data})
		}
	}

	// Rewrite Go files and normalize text line endings.
	out := make(map[string][]byte, len(rawEntries))
	for _, e := range rawEntries {
		data := e.data
		if isComponentTextFile(e.relPath) {
			data = normalizeLineEndings(data)
		}
		if strings.HasSuffix(strings.ToLower(e.relPath), ".go") {
			rewritten, err := rewriteDiffGoFile(e.relPath, data, rewrites)
			if err != nil {
				return nil, fmt.Errorf("%s@%s: rewrite %s: %w", dm.path, dm.version, e.relPath, err)
			}
			data = rewritten
		}
		out[e.relPath] = data
	}
	return out, nil
}

// isGeneratedCatalogFile reports whether relPath in locales/ is a catalog file
// generated by nsgo i18n (excluded from diff comparison).
func isGeneratedCatalogFile(relPath string, data []byte) bool {
	low := strings.ToLower(relPath)
	if low == "locales/"+genFile {
		return true
	}
	if path.Dir(low) == "locales" && strings.HasSuffix(low, ".go") {
		return bytes.HasPrefix(normalizeLineEndings(data), []byte(genHeader))
	}
	return false
}

// rewriteDiffGoFile rewrites import paths (preserving import names) and
// whole-path occurrences in comments, then formats with go/format, matching
// #2467 byte-for-byte.
func rewriteDiffGoFile(relPath string, data []byte, rewrites map[string]string) ([]byte, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, relPath, data, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	oldPaths := slices.SortedFunc(maps.Keys(rewrites), func(a, b string) int {
		if len(a) != len(b) {
			return cmp.Compare(len(b), len(a))
		}
		return cmp.Compare(a, b)
	})

	for _, imp := range file.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		if newP, ok := rewrites[p]; ok {
			imp.Path.Value = strconv.Quote(newP)
		}
	}

	if len(oldPaths) > 0 {
		for _, cg := range file.Comments {
			for _, c := range cg.List {
				c.Text = rewriteDiffCommentWholePaths(c.Text, oldPaths, rewrites)
			}
		}
	}

	var buf bytes.Buffer
	if err := format.Node(&buf, fset, file); err != nil {
		return nil, err
	}
	return format.Source(buf.Bytes())
}

func isDiffImportPathChar(b byte) bool {
	return 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || '0' <= b && b <= '9' || b == '.' || b == '_' || b == '-' || b == '/'
}

func isDiffTrailingImportPathChar(s string, pos int) bool {
	if pos >= len(s) {
		return false
	}
	if s[pos] == '.' {
		return pos+1 < len(s) && isDiffImportPathChar(s[pos+1]) && s[pos+1] != '.'
	}
	return isDiffImportPathChar(s[pos])
}

func rewriteDiffCommentWholePaths(text string, oldPaths []string, rewrites map[string]string) string {
	var b strings.Builder
	changed := false
	i := 0
	for i < len(text) {
		replaced := false
		for _, oldP := range oldPaths {
			if strings.HasPrefix(text[i:], oldP) {
				end := i + len(oldP)
				leftOK := i == 0 || !isDiffImportPathChar(text[i-1])
				rightOK := !isDiffTrailingImportPathChar(text, end)
				if leftOK && rightOK {
					b.WriteString(rewrites[oldP])
					i = end
					replaced = true
					changed = true
					break
				}
			}
		}
		if !replaced {
			b.WriteByte(text[i])
			i++
		}
	}
	if !changed {
		return text
	}
	return b.String()
}

// readLocalCopyFiles reads every non-generated file in copyDir through an
// os.Root opened at copyDir. Every recorded file is checked first so recorded
// symlinks are refused explicitly even if dangling or pointing outside copyDir;
// any other symlink encountered in copyDir is also refused.
func readLocalCopyFiles(copyDir string, rec *copyRecord) (map[string][]byte, error) {
	info, err := os.Lstat(copyDir)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("symlinks are not allowed")
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("not a directory")
	}

	root, err := os.OpenRoot(copyDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	// Check every recorded path through os.Root first, refusing symlinks.
	for _, p := range rec.Files {
		if err := validateCopyRecordPath(p); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if _, err := checkRootNoSymlinks(root, p); err != nil {
			return nil, err
		}
	}
	generatedSet := make(map[string]bool, len(rec.Generated))
	for _, p := range rec.Generated {
		if err := validateCopyRecordPath(p); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		generatedSet[p] = true
	}

	files := map[string][]byte{}
	var walkDir func(relDir string) error
	walkDir = func(relDir string) error {
		openPath := "."
		if relDir != "" {
			openPath = filepath.FromSlash(relDir)
		}
		dirFile, err := root.Open(openPath)
		if err != nil {
			return err
		}
		entries, readErr := dirFile.ReadDir(-1)
		dirFile.Close()
		if readErr != nil {
			return readErr
		}
		slices.SortFunc(entries, func(a, b fs.DirEntry) int {
			return cmp.Compare(a.Name(), b.Name())
		})
		for _, e := range entries {
			name := e.Name()
			rel := name
			if relDir != "" {
				rel = relDir + "/" + name
			}
			// Skip nsgo-copy.yaml at the copy root and temporary preview hosts.
			if rel == copyRecordName {
				continue
			}
			if relDir == "" && strings.HasPrefix(name, ".nsgo-") && e.IsDir() {
				continue
			}
			if generatedSet[rel] {
				continue
			}
			st, err := root.Lstat(filepath.FromSlash(rel))
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			if st.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%s: symlinks are not allowed", rel)
			}
			if st.IsDir() {
				if err := walkDir(rel); err != nil {
					return err
				}
				continue
			}
			if !st.Mode().IsRegular() {
				return fmt.Errorf("%s: not a regular file", rel)
			}
			f, err := root.Open(filepath.FromSlash(rel))
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			data, readErr := io.ReadAll(f)
			f.Close()
			if readErr != nil {
				return fmt.Errorf("%s: %w", rel, readErr)
			}
			if isGeneratedCatalogFile(rel, data) {
				continue
			}
			files[rel] = data
		}
		return nil
	}
	if err := walkDir(""); err != nil {
		return nil, err
	}
	return files, nil
}

// compareCopyTrees compares oldFiles against newFiles and returns sorted
// fileDiff entries with paths relative to the module root. When
// checkFormattedGo is true, a local Go file whose go/format output equals the
// base (with CRLF read as LF) counts as unedited.
func compareCopyTrees(copyRelDir string, oldFiles, newFiles map[string][]byte, checkFormattedGo bool) []fileDiff {
	keySet := map[string]bool{}
	for k := range oldFiles {
		keySet[k] = true
	}
	for k := range newFiles {
		keySet[k] = true
	}
	keys := slices.Sorted(maps.Keys(keySet))

	var diffs []fileDiff
	for _, k := range keys {
		oldData, hasOld := oldFiles[k]
		newData, hasNew := newFiles[k]
		modRelPath := copyRelDir + "/" + k

		switch {
		case !hasOld && hasNew:
			if isBinaryContent(k, newData) {
				diffs = append(diffs, fileDiff{
					Path:    modRelPath,
					Kind:    diffAdded,
					Binary:  true,
					OldSize: 0,
					NewSize: len(newData),
				})
				continue
			}
			newNorm := normalizeLineEndings(newData)
			diffs = append(diffs, *buildTextFileDiff(modRelPath, diffAdded, nil, newNorm))

		case hasOld && !hasNew:
			if isBinaryContent(k, oldData) {
				diffs = append(diffs, fileDiff{
					Path:    modRelPath,
					Kind:    diffDeleted,
					Binary:  true,
					OldSize: len(oldData),
					NewSize: 0,
				})
				continue
			}
			oldNorm := normalizeLineEndings(oldData)
			diffs = append(diffs, *buildTextFileDiff(modRelPath, diffDeleted, oldNorm, nil))

		default:
			if isBinaryContent(k, oldData) || isBinaryContent(k, newData) {
				if bytes.Equal(oldData, newData) {
					continue
				}
				diffs = append(diffs, fileDiff{
					Path:    modRelPath,
					Kind:    diffModified,
					Binary:  true,
					OldSize: len(oldData),
					NewSize: len(newData),
				})
				continue
			}
			oldNorm := normalizeLineEndings(oldData)
			newNorm := normalizeLineEndings(newData)
			if bytes.Equal(oldNorm, newNorm) {
				continue
			}
			if checkFormattedGo && strings.HasSuffix(strings.ToLower(k), ".go") {
				if formatted, err := format.Source(newNorm); err == nil && bytes.Equal(oldNorm, formatted) {
					continue
				}
			}
			if d := buildTextFileDiff(modRelPath, diffModified, oldNorm, newNorm); d != nil {
				diffs = append(diffs, *d)
			}
		}
	}
	return diffs
}
