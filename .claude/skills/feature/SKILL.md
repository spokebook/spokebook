---
name: feature
description: Develop a new Spokebook feature with the subagent team (developer, tester, security-inspector). Use for every new feature or larger change to behavior, even when the user just says "build X".
argument-hint: <feature description>
---

You are the lead. You plan, delegate, review and commit; the subagents in `.claude/agents` do the
implementation, testing and security review. Feature: $ARGUMENTS

1. **Understand.** Read `docs/konzept.md` (relevant sections) and the affected code. If the feature
   is ambiguous or conflicts with the concept, ask the user before going on.
2. **Plan.** Create a branch `feat/<short-name>` from `main`. Write a short plan: affected layers
   and files, data model and migration, message IDs, UI pages, and numbered **acceptance criteria**.
   For larger features, show the plan to the user first.
3. **Implement.** Start the `developer` agent with the plan and the acceptance criteria. Large
   features may be split into several developer runs (e.g. store + logic, then handlers + UI), and
   independent parts may run in parallel (see "Parallel instances" below).
4. **Test.** Start the `tester` agent with the acceptance criteria and the scope. When the store
   interfaces are fixed in the plan, the tester may run in parallel with the developer. Send
   failures back to the `developer` with the exact output; after two rounds without success, look at
   the problem yourself.
5. **Security.** Start the `security-inspector` agent on the full diff. Fix critical, high and medium
   findings via the `developer` (and re-run the inspector on the fix); decide about low findings
   and tell the user.
6. **Review and commit.** Read the diff yourself (concept, conventions, i18n in all catalogs,
   generated sqlc code committed). Commit with Conventional Commits (`feat: ...`). Push or open a PR
   only when the user asks.

Finish with: what was built, acceptance criteria status, test and security results, open points.

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
- **security-inspector**: for very large diffs, one instance per area (e.g. auth and sessions,
  uploads and parsers, handlers and templates), each told which part to review.

Do not parallelize small features: the overhead of splitting and merging costs more than it saves.
Wait for all instances of a step before starting the next step.
