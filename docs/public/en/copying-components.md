---
title: Copying components
description: Inspect local edits and upstream changes in copied Go components with nsgo diff.
group: Components
order: 35
---

A copied component lives inside your app's module with an `nsgo-copy.yaml` record beside it naming its source module, version, component name, test choice, copied files and generated files. Once copied, the component is your app's own code.

## Comparing copies with `nsgo diff`

Use `nsgo diff` to see what your app changed in a copy, what another version would change, or which copies in the module have local edits:

```sh
nsgo diff                              # list each copy, its version, and edited/unedited status
nsgo diff chat                         # unified diff of local edits against the recorded version
nsgo diff components/chat              # target a copy by directory path
nsgo diff chat@v0.5.0                  # unified diff from the recorded version to v0.5.0
nsgo diff chat@upgrade                 # unified diff from the recorded version to the newest release
nsgo diff -stat chat                   # changed lines per file (also lists modified files for Apache-2.0 §4(b))
nsgo diff -exit-code chat              # exit with status 1 when there are differences
```

### How targets resolve

`nsgo diff` accepts a copy's directory or a component name:

1. A directory that holds `nsgo-copy.yaml` is used directly.
2. A bare name is matched first against the module's own `nsgo-copy.yaml` records so it works offline; if two copies from different modules share the same component name, `nsgo diff` asks for the directory (or `module#component`).
3. Otherwise it resolves the name as `nsgo add` does and matches a record by module and component.

### How comparisons work

- **Clean reference copy.** `nsgo diff` reconstructs the copy of the recorded (or requested) version directly from the verified module zip (`zip.CheckZip` and `dirhash.HashZip` checked against `go.sum`), applying the same test selection, ancestor license copy, Go import and comment rewrites (with other copied components at their current record directories) and `go/format` formatting. Generated files such as `locales/catalog_gen.go` are excluded.
- **Git-compatible output.** Unified diffs use `diff --git` headers and paths relative to the module root, with three lines of context, so `git apply` and `git apply -R` apply them directly. Binary files report `Binary files ... differ`.
- **Line endings and formatting.** Files are compared with CRLF read as LF, and Go files whose formatted output matches the recorded version count as unedited.
- **Offline use.** Recorded versions are read from the local Go module cache (`GOMODCACHE`) first, so `nsgo diff` works offline for versions the app has already fetched; otherwise it downloads them through Go module proxies with `GOTOOLCHAIN=local`.
- **Unavailable versions.** When a recorded version can no longer be downloaded, every non-generated file in the copy counts as edited and the output explains why.
- **Modified-file notices.** For Apache-2.0 components, section 4(b) asks you to mark modified files; `nsgo diff -stat` lists every changed file.
- **Safety.** Every path in `nsgo-copy.yaml` is validated on read (refusing escapes such as `../form/form.go` with the line number), all reads stay inside an `os.Root` at the copy's directory, and symlinks are refused.

## The `nsgo-copy.yaml` record

Each copied component directory holds a deterministic, hash-free `nsgo-copy.yaml`:

```yaml
# Written by nsgo add. nsgo diff compares this directory with the copy of this version.
version: 1
module: github.com/acme/kit
module_version: v1.0.0
component: field
tests: true
files:
  - LICENSE
  - field.go
  - locales/en.yaml
generated:
  - locales/catalog_gen.go
```

Moving a copy needs no extra command: `nsgo-copy.yaml` moves with its directory, and `nsgo diff` discovers copies by walking the module (skipping `vendor`, `testdata`, nested modules and directories starting with `.` or `_`).
