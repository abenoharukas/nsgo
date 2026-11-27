---
title: Copying components
description: Copy declared Go components into your app, track their source in nsgo-copy.yaml, and catch breaking upgrades in nsgo add, check, build and dev.
group: Components
order: 35
---

When an app needs to own and customize a component rather than only configure it, a copied component lives directly inside the app module beside an `nsgo-copy.yaml` record that names its source module, version, component name, test choice, copied files and generated files:

```yaml
# Written by nsgo add. nsgo diff compares this directory with the copy of this version.
version: 1
module: github.com/nsgo-dev/nsgo
module_version: v0.4.2
component: chat
tests: true
files:
  - LICENSE
  - NOTICE
  - chat.go
  - locales/en.yaml
generated:
  - locales/catalog_gen.go
```

Moving a copy needs no command because `nsgo-copy.yaml` moves with its directory. Commands find copies by walking the module and skipping `vendor`, `testdata`, nested modules, symlinks, and directories starting with `.` or `_`. Every path in `nsgo-copy.yaml` is validated whenever the record is read and confined to the copy's directory with symlinks refused.

## Upgrading NSGo with copies

NSGo and component modules can change APIs between releases. A copy compiles with the module version it was taken from, and may stop compiling after an upgrade. `nsgo` diagnoses broken copies before and after an upgrade without failing CI on version differences that break nothing:

1. **Before upgrading (`nsgo add`).** When an `nsgo add` plan moves a module that existing copies import—NSGo first among them—`nsgo add` vets those copies for the host against the planned `go.mod` and `go.sum` without modifying the module. Any copy that would stop compiling is listed under `copies` in the plan with its first compiler or vet error and the `nsgo diff <name>@<version>` and `nsgo add <name>@<version>` commands. Under `-y`, a plan that breaks copied components is refused so automated runs never break copies silently; reviewing the plan interactively in a terminal still lets you confirm if you plan to update the copy right after.
2. **After an external upgrade (`nsgo check`).** When `go.mod` moves outside `nsgo` (such as a `go get` or Dependabot update), `nsgo check` reads every `nsgo-copy.yaml` before vetting, validates its paths and confinement, and prints a warning for each copy whose source module `go.mod` requires at another version:

   ```text
   warn components/chat is chat from github.com/nsgo-dev/nsgo v0.4.2; this module requires v0.5.0: nsgo diff chat@v0.5.0, nsgo add chat@v0.5.0
   ```

   Up-to-date copies are not reported. If the moved module leaves the copy compiling, `nsgo check` prints the warning alone and exits 0.
3. **On compile or vet failures (`nsgo check`, `nsgo build`, `nsgo dev`).** Whenever `go vet` or `go build` fails in a copied component's package (matched from Go's `# <import path>` lines), `nsgo` prints the same diagnostic line for the copy after the `go` command's output so you immediately see which package is a copy, which version it came from, and which commands inspect and update it. If two copies in the module share a component name, the suggested commands use the copy's relative directory path.
