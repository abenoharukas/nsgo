package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/format"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"
)

// Updating copied components file by file with nsgo add (#2469, #2459).
//
// When nsgo add targets an already-copied component at another version
// (via <name>@<version>, <name>@upgrade, a downgrade, <dir>@<version>, or
// <module>@<version>#<component>), it reconstructs both the recorded base
// version and the target version, compares each file across (base, app, author),
// keeps app-only edits, updates unedited files (preserving CRLF line endings),
// deletes unedited dropped files, flags edited dropped files (refused under -y),
// refuses files both sides changed differently or case-insensitive add collisions,
// regenerates catalogs from the resulting locales, updates transitive same-module
// components in the same plan, vets the resulting copy, and applies atomically
// through os.Root with full rollback on error or interrupt.

type copyFileActionKind int

const (
	copyActionKeep copyFileActionKind = iota
	copyActionUpdate
	copyActionAdd
	copyActionDelete
	copyActionDeleteEdited
)

type copyFileAction struct {
	relPath   string
	kind      copyFileActionKind
	data      []byte // bytes to write for copyActionUpdate and copyActionAdd
	crlf      bool   // true when CRLF line endings were preserved on update
	generated bool   // true for generated catalog files
	reason    string // human-readable explanation for keep/flagged actions
}

type copyFileConflict struct {
	relPath string
	reason  string
}

// copyUpdateApplyRename wraps root.Rename during update application; tests may
// replace it to verify full rollback on a mid-apply failure.
var copyUpdateApplyRename = func(root *os.Root, oldName, newName string) error {
	return root.Rename(oldName, newName)
}

// resolveAddRequests resolves nsgo add arguments in module m, matching existing
// copied components by directory or unique component name first (as #2468 does,
// so copies resolve offline) before falling back to parseAddRequests.
func resolveAddRequests(m *addModule, args []string) ([]addRequest, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("add: name a module or component: %s add module[@version][#component]", nsgoCommand())
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			return nil, fmt.Errorf("add: flags go before the modules: %s add %s %s", nsgoCommand(), arg, args[0])
		}
	}

	var (
		copiesLoaded bool
		copies       []moduleCopy
		appModPath   string
	)
	loadCopies := func() ([]moduleCopy, error) {
		if copiesLoaded {
			return copies, nil
		}
		goModBytes, err := os.ReadFile(filepath.Join(m.dir, "go.mod"))
		if err != nil {
			return nil, fmt.Errorf("add: %w", err)
		}
		appModPath = modfile.ModulePath(goModBytes)
		found, err := findModuleCopies(m.dir, appModPath)
		if err != nil {
			return nil, fmt.Errorf("add: %w", err)
		}
		copies = found
		copiesLoaded = true
		return copies, nil
	}

	var reqs []addRequest
	for _, arg := range args {
		var req addRequest
		matchedCopy := false

		if !strings.Contains(arg, "#") {
			spec, version, hasVersion := strings.Cut(arg, "@")
			if hasVersion && version == "" {
				return nil, fmt.Errorf("add: %q is not a module path with an optional @version", arg)
			}
			if hasVersion && version == "none" {
				return nil, fmt.Errorf("add: %s@none would remove %s; add never removes a module", spec, spec)
			}

			// 1. Check whether spec names a directory inside this module holding nsgo-copy.yaml.
			if dirCopy, found, err := lookupCopyByDir(m.dir, m.cwd, spec); err != nil {
				return nil, fmt.Errorf("add: %w", err)
			} else if found {
				req = addRequest{
					path:          dirCopy.record.Module,
					version:       version,
					typed:         hasVersion,
					component:     dirCopy.record.Component,
					fromName:      spec,
					targetCopyDir: dirCopy.relDir,
				}
				matchedCopy = true
			} else if isValidCopyComponentName(spec) {
				// 2. Match a bare component name against the module's records first (works offline).
				existing, err := loadCopies()
				if err != nil {
					return nil, err
				}
				var matches []moduleCopy
				for _, ec := range existing {
					if ec.record.Component == spec {
						matches = append(matches, ec)
					}
				}
				switch len(matches) {
				case 1:
					ec := matches[0]
					req = addRequest{
						path:          ec.record.Module,
						version:       version,
						typed:         hasVersion,
						component:     ec.record.Component,
						fromName:      spec,
						targetCopyDir: ec.relDir,
					}
					matchedCopy = true
				case 0:
					// Fall through to registry / module parsing.
				default:
					dirs := make([]string, len(matches))
					for i, ec := range matches {
						dirs[i] = ec.relDir
					}
					return nil, fmt.Errorf("add: %q matches %d copied components (%s); name the copy's directory", spec, len(matches), strings.Join(dirs, ", "))
				}
			}
		}

		if !matchedCopy {
			parsed, err := parseAddRequests([]string{arg})
			if err != nil {
				return nil, err
			}
			req = parsed[0]
		}

		if slices.ContainsFunc(reqs, func(r addRequest) bool { return r.path == req.path && r.component == req.component }) {
			return nil, fmt.Errorf("add: %s is named twice", req.displayName())
		}
		reqs = append(reqs, req)
	}
	return reqs, nil
}

// lookupCopyByDir checks whether spec names an existing directory inside modDir
// that holds an nsgo-copy.yaml record. A symlinked directory or invalid record
// returns an error.
func lookupCopyByDir(modDir, cwd, spec string) (moduleCopy, bool, error) {
	if spec == "" || spec == "." || spec == ".." {
		return moduleCopy{}, false, nil
	}
	var candidates []string
	cleaned := filepath.FromSlash(spec)
	if filepath.IsAbs(cleaned) {
		candidates = append(candidates, filepath.Clean(cleaned))
	} else {
		candidates = append(candidates, filepath.Clean(filepath.Join(cwd, cleaned)))
		modCand := filepath.Clean(filepath.Join(modDir, cleaned))
		if modCand != candidates[0] {
			candidates = append(candidates, modCand)
		}
	}

	absMod, err := filepath.Abs(modDir)
	if err != nil {
		return moduleCopy{}, false, err
	}
	for _, cand := range candidates {
		absCand, err := filepath.Abs(cand)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(absMod, absCand)
		if err != nil || rel == "." || !filepath.IsLocal(rel) {
			continue
		}
		info, err := os.Lstat(absCand)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return moduleCopy{}, false, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return moduleCopy{}, false, fmt.Errorf("%s: symlinks are not allowed", spec)
		}
		if !info.IsDir() {
			continue
		}
		rec, err := readCopyRecord(absCand)
		if err != nil {
			return moduleCopy{}, false, fmt.Errorf("%s/%s: %w", filepath.ToSlash(rel), copyRecordName, err)
		}
		if rec == nil {
			continue
		}
		goModBytes, err := os.ReadFile(filepath.Join(absMod, "go.mod"))
		if err != nil {
			return moduleCopy{}, false, err
		}
		appModPath := modfile.ModulePath(goModBytes)
		slashRel := filepath.ToSlash(rel)
		impPath := slashRel
		if appModPath != "" {
			impPath = appModPath + "/" + slashRel
		}
		return moduleCopy{
			dir:        absCand,
			relDir:     slashRel,
			importPath: impPath,
			record:     rec,
		}, true, nil
	}
	return moduleCopy{}, false, nil
}

// readExistingCopyTree reads every file currently in ec.dir through os.Root,
// refusing symlinks anywhere in the directory tree and validating all recorded
// paths. Returns the raw nsgo-copy.yaml bytes, exact on-disk path -> bytes, and
// lower-case path -> exact on-disk path.
func readExistingCopyTree(ec moduleCopy) (recordRaw []byte, diskFiles map[string][]byte, diskLower map[string]string, err error) {
	info, err := os.Lstat(ec.dir)
	if err != nil {
		return nil, nil, nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, nil, fmt.Errorf("%s: symlinks are not allowed", ec.relDir)
	}
	if !info.IsDir() {
		return nil, nil, nil, fmt.Errorf("%s: not a directory", ec.relDir)
	}

	root, err := os.OpenRoot(ec.dir)
	if err != nil {
		return nil, nil, nil, err
	}
	defer root.Close()

	st, err := root.Lstat(copyRecordName)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%s/%s: %w", ec.relDir, copyRecordName, err)
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return nil, nil, nil, fmt.Errorf("%s/%s: symlinks are not allowed", ec.relDir, copyRecordName)
	}
	if !st.Mode().IsRegular() {
		return nil, nil, nil, fmt.Errorf("%s/%s: must be a regular file", ec.relDir, copyRecordName)
	}
	recordRaw, err = root.ReadFile(copyRecordName)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%s/%s: %w", ec.relDir, copyRecordName, err)
	}
	if _, err := parseCopyRecord(recordRaw); err != nil {
		return nil, nil, nil, fmt.Errorf("%s/%s: %w", ec.relDir, copyRecordName, err)
	}

	// Re-check every recorded path for validity and symlinks.
	for _, relPath := range slices.Concat(ec.record.Files, ec.record.Generated) {
		if err := validateCopyRecordPath(relPath); err != nil {
			return nil, nil, nil, fmt.Errorf("%s/%s: %w", ec.relDir, relPath, err)
		}
		if st, err := root.Lstat(filepath.FromSlash(relPath)); err == nil {
			if st.Mode()&os.ModeSymlink != 0 {
				return nil, nil, nil, fmt.Errorf("%s/%s: symlinks are not allowed", ec.relDir, relPath)
			}
			if !st.Mode().IsRegular() {
				return nil, nil, nil, fmt.Errorf("%s/%s: not a regular file", ec.relDir, relPath)
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil, fmt.Errorf("%s/%s: %w", ec.relDir, relPath, err)
		}
	}

	diskFiles = map[string][]byte{}
	diskLower = map[string]string{}

	var walk func(relDir string) error
	walk = func(relDir string) error {
		openPath := "."
		if relDir != "" {
			openPath = filepath.FromSlash(relDir)
		}
		dirFile, err := root.Open(openPath)
		if err != nil {
			return err
		}
		entries, readErr := dirFile.ReadDir(-1)
		closeErr := dirFile.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, copyStagingPrefix) || strings.HasPrefix(name, ".nsgo-preview-") {
				continue
			}
			slash := name
			if relDir != "" {
				slash = relDir + "/" + name
			}
			local := filepath.FromSlash(slash)
			st, err := root.Lstat(local)
			if err != nil {
				return fmt.Errorf("%s/%s: %w", ec.relDir, slash, err)
			}
			if st.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%s/%s: symlinks are not allowed", ec.relDir, slash)
			}
			if st.IsDir() {
				if err := walk(slash); err != nil {
					return err
				}
				continue
			}
			if !st.Mode().IsRegular() {
				return fmt.Errorf("%s/%s: not a regular file", ec.relDir, slash)
			}
			if slash == copyRecordName {
				continue
			}
			data, err := root.ReadFile(local)
			if err != nil {
				return fmt.Errorf("%s/%s: %w", ec.relDir, slash, err)
			}
			diskFiles[slash] = data
			diskLower[strings.ToLower(slash)] = slash
		}
		return nil
	}
	if err := walk(""); err != nil {
		return nil, nil, nil, err
	}
	return recordRaw, diskFiles, diskLower, nil
}

// reconstructBaseCopy downloads (from cache or proxy) and reconstructs the
// canonical copy of ec.record.ModuleVersion at ec's current location with the
// other copies where their records are now, formatted and without generated files.
func (m *addModule) reconstructBaseCopy(
	ctx context.Context,
	downloads map[string]*downloadedModule,
	ec moduleCopy,
	existingCopies []moduleCopy,
	appGoSum []byte,
) (baseFiles map[string][]byte, unavailable string, err error) {
	baseDM, dlErr := m.getOrDownloadModule(ctx, downloads, ec.record.Module, ec.record.ModuleVersion, "", true, appGoSum)
	if dlErr != nil {
		return nil, dlErr.Error(), nil
	}
	if baseDM.manifest == nil || baseDM.manifest.Components[ec.record.Component] == "" {
		return nil, fmt.Sprintf("%s %s declares no component %s", ec.record.Module, ec.record.ModuleVersion, ec.record.Component), nil
	}
	baseSrcDir := baseDM.manifest.Components[ec.record.Component]

	_, baseZipRaws, err := collectComponentZipFiles(baseDM, baseSrcDir, ec.record.Tests)
	if err != nil {
		return nil, "", fmt.Errorf("reconstruct %s@%s#%s: %w", ec.record.Module, ec.record.ModuleVersion, ec.record.Component, err)
	}
	baseAncestorLicenses, err := collectAncestorLicenses(baseDM, baseSrcDir, baseZipRaws)
	if err != nil {
		return nil, "", fmt.Errorf("reconstruct %s@%s#%s: %w", ec.record.Module, ec.record.ModuleVersion, ec.record.Component, err)
	}

	// Build import rewrite map using the current locations of ec and any other
	// existing copies from the same source module.
	baseRewrites := map[string]string{
		ec.record.Module + "/" + baseSrcDir:              ec.importPath,
		ec.record.Module + "/" + baseSrcDir + "/locales": ec.importPath + "/locales",
	}
	for _, other := range existingCopies {
		if other.record.Module != ec.record.Module {
			continue
		}
		if otherSrcDir := baseDM.manifest.Components[other.record.Component]; otherSrcDir != "" {
			baseRewrites[other.record.Module+"/"+otherSrcDir] = other.importPath
			baseRewrites[other.record.Module+"/"+otherSrcDir+"/locales"] = other.importPath + "/locales"
		}
	}

	baseFiles = make(map[string][]byte, len(baseZipRaws)+len(baseAncestorLicenses))
	for _, rf := range baseZipRaws {
		if strings.EqualFold(rf.relPath, "locales/"+genFile) {
			continue
		}
		data := rf.data
		if isComponentTextFile(rf.relPath) {
			data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
		}
		if strings.HasSuffix(strings.ToLower(rf.relPath), ".go") {
			formatted, _, _, _, _, err := rewriteComponentGoFile(rf.relPath, data, baseRewrites)
			if err != nil {
				return nil, "", fmt.Errorf("reconstruct %s@%s/%s: %w", ec.record.Module, ec.record.ModuleVersion, rf.relPath, err)
			}
			data = formatted
		}
		baseFiles[rf.relPath] = data
	}
	for _, al := range baseAncestorLicenses {
		baseFiles[al.relPath] = bytes.ReplaceAll(al.data, []byte("\r\n"), []byte("\n"))
	}
	return baseFiles, "", nil
}

// normalizeForCompare normalizes a file for edit detection: CRLF becomes LF for
// text files, and Go files are formatted with go/format (falling back to raw LF
// if the app's Go file does not currently parse).
func normalizeForCompare(relPath string, data []byte) []byte {
	if !isComponentTextFile(relPath) {
		return data
	}
	norm := bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	if strings.HasSuffix(strings.ToLower(relPath), ".go") {
		if formatted, err := format.Source(norm); err == nil {
			return formatted
		}
	}
	return norm
}

// isGeneratedCopyPath reports whether relPath is a generated catalog file.
func isGeneratedCopyPath(relPath string, rec *copyRecord) bool {
	if strings.EqualFold(relPath, "locales/"+genFile) {
		return true
	}
	if rec != nil {
		for _, g := range rec.Generated {
			if strings.EqualFold(relPath, g) {
				return true
			}
		}
	}
	return false
}

// planComponentUpdate compares (base, app, author) file by file for c, computes
// per-file actions, conflicts, flagged deletions, resulting files, external
// imports, and license checks.
func (p *copyPlan) planComponentUpdate(
	ctx context.Context,
	downloads map[string]*downloadedModule,
	existingCopies []moduleCopy,
	c *plannedComponent,
) error {
	recordRaw, diskFiles, _, err := readExistingCopyTree(c.existing)
	if err != nil {
		return fmt.Errorf("add: %w", err)
	}
	c.existingRecordRaw = recordRaw
	c.existingFilesRaw = diskFiles

	// Split out non-generated files currently on disk.
	appNonGen := make(map[string][]byte, len(diskFiles))
	appNonGenLower := make(map[string]string, len(diskFiles))
	for relPath, data := range diskFiles {
		if isGeneratedCopyPath(relPath, c.existing.record) {
			continue
		}
		appNonGen[relPath] = data
		appNonGenLower[strings.ToLower(relPath)] = relPath
	}

	// Reconstruct the recorded base version.
	baseFiles, unavail, err := p.m.reconstructBaseCopy(ctx, downloads, c.existing, existingCopies, p.oldSum)
	if err != nil {
		return fmt.Errorf("add: %w", err)
	}
	c.baseUnavailable = unavail

	// Author's non-generated files for the target version (already rewritten & formatted).
	authorMap := make(map[string]copiedFile, len(c.files))
	c.recordFiles = make([]string, 0, len(c.files))
	for _, f := range c.files {
		if f.generated {
			continue
		}
		authorMap[f.relPath] = f
		c.recordFiles = append(c.recordFiles, f.relPath)
	}
	slices.Sort(c.recordFiles)

	if c.baseUnavailable != "" {
		// Recorded version cannot be downloaded: every file counts as edited.
		for _, relPath := range c.recordFiles {
			authorFile := authorMap[relPath]
			if appRaw, inApp := appNonGen[relPath]; inApp {
				appNorm := normalizeForCompare(relPath, appRaw)
				if bytes.Equal(appNorm, authorFile.data) {
					c.fileActions = append(c.fileActions, copyFileAction{
						relPath: relPath,
						kind:    copyActionKeep,
						reason:  "identical to " + c.srcVersion,
					})
				} else {
					c.conflicts = append(c.conflicts, copyFileConflict{
						relPath: relPath,
						reason:  fmt.Sprintf("recorded version %s is unavailable, so this file counts as edited", c.fromVersion),
					})
				}
			} else if existingRel, existsCI := appNonGenLower[strings.ToLower(relPath)]; existsCI {
				c.conflicts = append(c.conflicts, copyFileConflict{
					relPath: relPath,
					reason:  fmt.Sprintf("file %s already exists in %s", existingRel, c.destRel),
				})
			} else {
				c.fileActions = append(c.fileActions, copyFileAction{
					relPath: relPath,
					kind:    copyActionAdd,
					data:    authorFile.data,
				})
			}
		}
		for _, relPath := range c.existing.record.Files {
			if _, inAuthor := authorMap[relPath]; inAuthor {
				continue
			}
			if _, inApp := appNonGen[relPath]; inApp {
				c.fileActions = append(c.fileActions, copyFileAction{
					relPath: relPath,
					kind:    copyActionDeleteEdited,
					reason:  fmt.Sprintf("dropped in %s; counts as edited because %s is unavailable", c.srcVersion, c.fromVersion),
				})
				c.flaggedDels = append(c.flaggedDels, relPath)
			}
		}
	} else {
		// 3-way per-file comparison across (base, app, author).
		allPathsMap := make(map[string]bool, len(baseFiles)+len(authorMap))
		for k := range baseFiles {
			allPathsMap[k] = true
		}
		for k := range authorMap {
			allPathsMap[k] = true
		}
		allPaths := slices.Sorted(maps.Keys(allPathsMap))

		for _, relPath := range allPaths {
			baseData, inBase := baseFiles[relPath]
			authorFile, inAuthor := authorMap[relPath]
			appRaw, inApp := appNonGen[relPath]

			switch {
			case inAuthor && !inBase:
				// Author added this file.
				if existingRel, existsCI := appNonGenLower[strings.ToLower(relPath)]; existsCI {
					reason := fmt.Sprintf("added in %s, but %s already exists in %s", c.srcVersion, existingRel, c.destRel)
					c.conflicts = append(c.conflicts, copyFileConflict{relPath: relPath, reason: reason})
				} else {
					c.fileActions = append(c.fileActions, copyFileAction{
						relPath: relPath,
						kind:    copyActionAdd,
						data:    authorFile.data,
					})
				}

			case inBase && !inAuthor:
				// Author dropped this file.
				if !inApp {
					continue
				}
				appNorm := normalizeForCompare(relPath, appRaw)
				if bytes.Equal(appNorm, baseData) {
					c.fileActions = append(c.fileActions, copyFileAction{
						relPath: relPath,
						kind:    copyActionDelete,
					})
				} else {
					c.fileActions = append(c.fileActions, copyFileAction{
						relPath: relPath,
						kind:    copyActionDeleteEdited,
						reason:  fmt.Sprintf("edited by app and dropped in %s", c.srcVersion),
					})
					c.flaggedDels = append(c.flaggedDels, relPath)
				}

			case inBase && inAuthor:
				// File exists in both base and author.
				if !inApp {
					if bytes.Equal(baseData, authorFile.data) {
						// Author left it alone; keep the app's deletion.
						c.fileActions = append(c.fileActions, copyFileAction{
							relPath: relPath,
							kind:    copyActionKeep,
							reason:  "deleted by app; unchanged upstream",
						})
					} else {
						c.conflicts = append(c.conflicts, copyFileConflict{
							relPath: relPath,
							reason:  fmt.Sprintf("deleted by app and changed in %s", c.srcVersion),
						})
					}
					continue
				}

				appNorm := normalizeForCompare(relPath, appRaw)
				bothSame := bytes.Equal(appNorm, authorFile.data)
				appUnedited := bytes.Equal(appNorm, baseData)
				authorUnchanged := bytes.Equal(authorFile.data, baseData)

				switch {
				case bothSame:
					reason := "unchanged"
					if !appUnedited {
						reason = "app and author made the same change"
					}
					c.fileActions = append(c.fileActions, copyFileAction{
						relPath: relPath,
						kind:    copyActionKeep,
						reason:  reason,
					})
				case appUnedited:
					// App left it alone: take author's version, preserving CRLF if present on disk.
					crlf := isComponentTextFile(relPath) && bytes.Contains(appRaw, []byte("\r\n"))
					writeData := authorFile.data
					if crlf {
						writeData = bytes.ReplaceAll(authorFile.data, []byte("\n"), []byte("\r\n"))
					}
					c.fileActions = append(c.fileActions, copyFileAction{
						relPath: relPath,
						kind:    copyActionUpdate,
						data:    writeData,
						crlf:    crlf,
					})
				case authorUnchanged:
					// Author left it alone: keep app's edits untouched.
					c.fileActions = append(c.fileActions, copyFileAction{
						relPath: relPath,
						kind:    copyActionKeep,
						reason:  "app edits kept",
					})
				default:
					// Both changed differently: refuse and name the file.
					c.conflicts = append(c.conflicts, copyFileConflict{
						relPath: relPath,
						reason:  fmt.Sprintf("both the app and %s changed it", c.srcVersion),
					})
				}
			}
		}
	}

	slices.Sort(c.flaggedDels)

	// Build resulting non-generated files for catalog generation, external
	// imports, license classification, and vetting.
	resultingMap := make(map[string][]byte, len(appNonGen)+len(c.fileActions))
	for relPath, data := range appNonGen {
		resultingMap[relPath] = data
	}
	for _, act := range c.fileActions {
		switch act.kind {
		case copyActionAdd, copyActionUpdate:
			resultingMap[act.relPath] = act.data
		case copyActionDelete, copyActionDeleteEdited:
			delete(resultingMap, act.relPath)
		}
	}

	c.resultingFiles = make([]copiedFile, 0, len(resultingMap))
	extSet := map[string]bool{}
	for _, relPath := range slices.Sorted(maps.Keys(resultingMap)) {
		data := resultingMap[relPath]
		_, isLic := isLicenseDocument(path.Base(relPath))
		isTest := isComponentTestFile(relPath)
		isLoc := strings.EqualFold(path.Dir(relPath), "locales")
		c.resultingFiles = append(c.resultingFiles, copiedFile{
			relPath:   relPath,
			data:      data,
			isTest:    isTest,
			isLicense: isLic,
			isLocale:  isLoc,
		})
		if strings.HasSuffix(strings.ToLower(relPath), ".go") {
			for _, imp := range parseGoFileImports(data) {
				if !isStdlibImport(imp) && imp != p.appModPath && !strings.HasPrefix(imp, p.appModPath+"/") {
					extSet[imp] = true
				}
			}
		}
	}
	c.extImports = slices.Sorted(maps.Keys(extSet))

	// Classify licenses across the resulting files against app policy.
	if err := evaluateCopyLicenses(c, p.app.licenses); err != nil {
		return fmt.Errorf("add: %w", err)
	}

	// Check if any app catalog supplies this component's original locales.
	if hasComponentLocales(c) && p.app.i18nDir != "" && hasCatalogs(p.app.i18nDir) {
		origLocales := c.srcMod + "/" + c.srcDir + "/locales"
		copyLocales := c.importPath + "/locales"
		c.supplyHints = findAppCatalogSupplies(p.m.dir, p.app.i18nDir, origLocales, copyLocales)
	}

	return nil
}

// updateSummaryLines formats the per-file plan lines for an updated component.
func (c *plannedComponent) updateSummaryLines() []string {
	var lines []string
	var unchangedCount int

	for _, act := range c.fileActions {
		switch act.kind {
		case copyActionUpdate:
			if act.crlf {
				lines = append(lines, fmt.Sprintf("~ %s (CRLF)", act.relPath))
			} else {
				lines = append(lines, fmt.Sprintf("~ %s", act.relPath))
			}
		case copyActionAdd:
			lines = append(lines, fmt.Sprintf("+ %s", act.relPath))
		case copyActionDelete:
			lines = append(lines, fmt.Sprintf("- %s", act.relPath))
		case copyActionDeleteEdited:
			lines = append(lines, fmt.Sprintf("- %s (edited by app; flagged)", act.relPath))
		case copyActionKeep:
			if act.reason == "unchanged" {
				unchangedCount++
			} else {
				lines = append(lines, fmt.Sprintf("= %s (%s)", act.relPath, act.reason))
			}
		}
	}
	if unchangedCount > 0 {
		lines = append(lines, fmt.Sprintf("= %s unchanged", countNoun(unchangedCount, "file")))
	}
	for _, act := range c.genActions {
		switch act.kind {
		case copyActionAdd, copyActionUpdate:
			lines = append(lines, fmt.Sprintf("~ %s, written by this nsgo", act.relPath))
		case copyActionDelete:
			lines = append(lines, fmt.Sprintf("- %s (no catalogs left)", act.relPath))
		}
	}
	lines = append(lines, fmt.Sprintf("~ %s (%s -> %s)", copyRecordName, c.fromVersion, c.srcVersion))
	return lines
}

// conflictRefusal returns the formatted refusal message for c's conflicts,
// naming every conflicting file and pointing to the docs' manual-update steps
// with nsgo diff.
func (c *plannedComponent) conflictRefusal() string {
	if len(c.conflicts) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s cannot be updated from %s to %s:\n", c.destRel, c.fromVersion, c.srcVersion)
	for _, cf := range c.conflicts {
		fmt.Fprintf(&b, "  - %s/%s: %s\n", c.destRel, cf.relPath, cf.reason)
	}
	diffName := c.fromName
	if diffName == "" {
		diffName = c.compName
	}
	cmd := nsgoCommand()
	fmt.Fprintf(&b, "Carry the app's edits over by hand with `%s diff %s` and `%s diff %s@%s` (see docs: copying-components.md).", cmd, diffName, cmd, diffName, c.srcVersion)
	return b.String()
}

// updateRollbackEntry records a single file mutation inside an updated copy
// directory so apply can restore the exact pre-apply state on error or interrupt.
type updateRollbackEntry struct {
	root    *os.Root
	relPath string
	existed bool
	oldData []byte
}

// verifyUpdateUnchanged checks through os.Root that an updated copy's
// nsgo-copy.yaml and every file it will read/write/delete have not changed on
// disk since planning, refusing symlinks.
func verifyUpdateUnchanged(c *plannedComponent) error {
	info, err := os.Lstat(c.destAbs)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s changed since the plan was made, so nothing was written", c.destRel)
	}
	root, err := os.OpenRoot(c.destAbs)
	if err != nil {
		return err
	}
	defer root.Close()

	st, err := root.Lstat(copyRecordName)
	if err != nil || st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return fmt.Errorf("%s/%s changed since the plan was made, so nothing was written", c.destRel, copyRecordName)
	}
	curRec, err := root.ReadFile(copyRecordName)
	if err != nil || !bytes.Equal(curRec, c.existingRecordRaw) {
		return fmt.Errorf("%s/%s changed since the plan was made, so nothing was written: run %s add again", c.destRel, copyRecordName, nsgoCommand())
	}

	allActions := slices.Concat(c.fileActions, c.genActions)
	for _, act := range allActions {
		if act.kind == copyActionKeep {
			continue
		}
		local := filepath.FromSlash(act.relPath)
		oldData, existed := c.existingFilesRaw[act.relPath]
		st, err := root.Lstat(local)
		if errors.Is(err, fs.ErrNotExist) {
			if existed {
				return fmt.Errorf("%s/%s changed since the plan was made, so nothing was written: run %s add again", c.destRel, act.relPath, nsgoCommand())
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("%s/%s: %w", c.destRel, act.relPath, err)
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s/%s: symlinks are not allowed", c.destRel, act.relPath)
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("%s/%s: not a regular file", c.destRel, act.relPath)
		}
		if !existed {
			return fmt.Errorf("%s/%s appeared since the plan was made, so nothing was written: run %s add again", c.destRel, act.relPath, nsgoCommand())
		}
		cur, err := root.ReadFile(local)
		if err != nil || !bytes.Equal(cur, oldData) {
			return fmt.Errorf("%s/%s changed since the plan was made, so nothing was written: run %s add again", c.destRel, act.relPath, nsgoCommand())
		}
	}
	return nil
}

// writeFileInRootAtomically writes data to relPath inside root via a temporary
// sibling file and rename, refusing symlinks at target.
func writeFileInRootAtomically(root *os.Root, relPath string, data []byte, seq int) error {
	local := filepath.FromSlash(relPath)
	if st, err := root.Lstat(local); err == nil {
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s: symlinks are not allowed", relPath)
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("%s: not a regular file", relPath)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	dir := filepath.Dir(local)
	base := filepath.Base(local)
	tmpName := fmt.Sprintf("%s%d-%d-%s.tmp", copyStagingPrefix, os.Getpid(), seq, base)
	tmpLocal := tmpName
	if dir != "." {
		tmpLocal = filepath.Join(dir, tmpName)
	}

	f, err := root.OpenFile(tmpLocal, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = root.Remove(tmpLocal)
		return err
	}
	if err := copyUpdateApplyRename(root, tmpLocal, local); err != nil {
		_ = root.Remove(tmpLocal)
		return err
	}
	return nil
}

// mkdirAllInRootTracking creates missing subdirectories of dir inside root and
// returns the slash paths created (in parent-before-child order) for rollback.
func mkdirAllInRootTracking(root *os.Root, relDir string) ([]string, error) {
	if relDir == "" || relDir == "." {
		return nil, nil
	}
	var created []string
	parts := strings.Split(relDir, "/")
	cur := ""
	for _, part := range parts {
		if cur == "" {
			cur = part
		} else {
			cur += "/" + part
		}
		local := filepath.FromSlash(cur)
		if st, err := root.Lstat(local); err == nil {
			if st.Mode()&os.ModeSymlink != 0 {
				return created, fmt.Errorf("%s: symlinks are not allowed", cur)
			}
			if !st.IsDir() {
				return created, fmt.Errorf("%s: not a directory", cur)
			}
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return created, err
		}
		if err := root.Mkdir(local, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return created, err
		}
		created = append(created, cur)
	}
	return created, nil
}

// removeEmptyParentDirsInRoot removes empty subdirectories above relPath inside root.
func removeEmptyParentDirsInRoot(root *os.Root, relPath string) {
	for cur := path.Dir(relPath); cur != "." && cur != "/" && cur != ""; cur = path.Dir(cur) {
		if err := root.Remove(filepath.FromSlash(cur)); err != nil {
			break
		}
	}
}
