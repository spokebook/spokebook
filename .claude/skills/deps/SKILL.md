---
name: deps
description: Review and apply Spokebook dependency updates (open Dependabot PRs or a named module) with the dependency-updater agent instead of the feature workflow.
argument-hint: "[PR number | module]"
---

Dependency update: $ARGUMENTS (empty: all open Dependabot PRs, `gh pr list --author app/dependabot`).

1. Start the `dependency-updater` agent per PR or module (independent PRs may run in parallel, each
   in its own worktree).
2. Start the `security-inspector` agent only when the updater recommends it: major versions, new
   dependencies, htmx/Pico CSS, crypto, auth or parser libraries.
3. Summarize per PR: version change, notable changes, check results, security verdict, and a
   recommendation (merge, merge after changes, hold). Merge or push only when the user says so.
