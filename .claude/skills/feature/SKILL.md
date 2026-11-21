---
name: feature
description: Develop a new Spokebook feature with the subagent team (developer, tester, reviewer, security-inspector). Use for every new feature or larger change to behavior, even when the user just says "build X".
argument-hint: <feature description>
---

You are the lead. You plan, delegate, review and commit; the subagents in `.claude/agents` do the
implementation, testing and reviews. Feature: $ARGUMENTS

1. **Understand.** Read `docs/konzept.md` (relevant sections) and the affected code. If the feature
   is ambiguous or conflicts with the concept, ask the user before going on.
2. **Impact analysis.** Find comparable existing fields or features (`grep` for a similar field,
   e.g. `part_number` for a new component field) and list every flow that handles them: create,
   edit, duplicate, replace, install/remove, retire, maintenance copies, backup export and import,
   ride imports, list and detail pages, search. Decide for each flow whether the feature affects it
   and how. This list is part of the plan.
3. **Plan.** Create a branch `feat/<short-name>` from `main`. Write a short plan: affected layers
   and files, data model and migration, message IDs, UI pages, the flows from step 2, whether
   `docs/konzept.md` changes, and numbered **acceptance criteria** (one per affected flow at least).
   Describe what and why; leave room for the developer to find details. For larger features, show
   the plan to the user first.
4. **Implement.** Start the `developer` agent with the plan and the acceptance criteria. Large
   features may be split into several developer runs (e.g. store + logic, then handlers + UI), and
   independent parts may run in parallel (see "Parallel instances" below).
5. **Test.** After the developer is done, start the `tester` agent with the feature description as
   the user stated it, the acceptance criteria and the scope. The tester writes the tests and runs
   all checks in the same run. Only for larger features with interfaces fixed in the plan may the
   tester start in parallel with the developer; then it must run all checks again once the developer
   is done. Send failures and missing cases the tester reports back to the `developer` with the exact
   output; after two rounds without success, look at the problem yourself.
6. **Review.** Start the `reviewer` and the `security-inspector` in parallel on the full diff. Give
   the reviewer the feature description as the user stated it, not your plan, so it judges the
   result independently. Fix "must fix" and "should fix" findings of the reviewer and critical, high
   and medium findings of the inspector via the `developer` (missing tests via the `tester`), then
   re-run the agent that found them on the fix. Decide about nits and low findings and tell the user.
7. **Commit.** Read the diff yourself (concept, conventions, i18n in all catalogs, generated sqlc
   code committed, `docs/konzept.md` updated when the data model or a flow changed). Run
   `gofumpt -l .`, `go vet ./...`, `golangci-lint run ./...`, `sqlc diff` and `go test ./...`
   yourself. Commit with Conventional Commits (`feat: ...`). Push or open a PR only when the user
   asks.

Finish with: what was built, acceptance criteria status, test, review and security results, open
points.

## Parallel instances

For larger features you may start several instances of the same agent type at once (several
`Agent` calls in one message) where the work splits into independent parts:

- **developer**: one instance per independent area (e.g. two business logic packages, or the
  handlers and templates of separate pages), but only after the shared foundation is done in one run:
  store interfaces, migration, `sqlc generate` and the message IDs in `web/locales/` are written by a
  single instance first, because parallel edits to these files conflict. Give each instance an
  explicit list of files or packages it owns. If areas overlap, run them one after another, or use
  `isolation: "worktree"` and merge the results yourself.
- **tester**: one instance per package or area; tests in separate `*_test.go` files do not conflict.
  Only one instance runs the UI check, because it starts the app.
- **reviewer** and **security-inspector**: for very large diffs, one instance per area (e.g. auth
  and sessions, uploads and parsers, handlers and templates), each told which part to review.

Do not parallelize small features: the overhead of splitting and merging costs more than it saves.
Wait for all instances of a step before starting the next step.
