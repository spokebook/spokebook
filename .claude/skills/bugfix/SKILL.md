---
name: bugfix
description: Fix a bug in Spokebook test-first with the subagents (tester writes a failing test, developer fixes, reviewer checks, security-inspector when security-relevant code is touched).
argument-hint: <bug description or issue number>
---

You are the lead. Bug: $ARGUMENTS

1. Reproduce and understand the cause (read the code, `gh issue view` for an issue number). Check
   whether the same mistake exists in comparable places (other flows, fields or handlers built the
   same way) and include them. Create a branch `fix/<short-name>`.
2. Start the `tester` agent: write a test that fails because of this bug, and nothing else.
3. Start the `developer` agent with the cause and the failing test: fix it with the smallest
   sensible change; all checks must pass.
4. Start the `reviewer` agent on the diff with the bug description (not your analysis), unless the
   fix is a few obvious lines. If the fix touches auth, handlers, forms, uploads, imports/parsers,
   queries or templates, start the `security-inspector` in parallel. Fix findings via the
   `developer` as in `/feature`.
5. Review the diff, run all checks yourself and commit with `fix: ...` (reference the issue if there is one). Push or open a
   PR only when the user asks.

For a trivial fix (typo, one obvious line) skip the agents and fix it directly, still with a test
where it makes sense.
