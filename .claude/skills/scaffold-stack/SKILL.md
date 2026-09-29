---
name: scaffold-stack
description: Scaffold or extend a project using the preferred stack — pick libraries from the Preferred Stack catalogue, resolve the correct versions (match the repo, else latest stable), and wire the pnpm-workspace / Turborepo / Taskfile conventions. Detects what is being built (Go service, Node service, Vite+React web app, monorepo, Typst/Slidev docs) and whether a pnpm stack is present (choosing `pnpm dm exec` vs a direct `datamitsu` call). Use whenever the user asks to create, init, scaffold, bootstrap, or set up a new app/service/library/monorepo, or to add a major library to an existing one — even phrased informally as "start a React app", "new Go service", "set up a monorepo", "add a database layer".
---

# Scaffold Preferred Stack

Read `.datamitsu/ai/skills/scaffold-stack/instructions.md` from the project root and follow it precisely.

The instructions file is the source of truth for this skill — it is regenerated automatically by `datamitsu` from the central `datamitsu-config` repository, so it always reflects the current Preferred Stack rules.
