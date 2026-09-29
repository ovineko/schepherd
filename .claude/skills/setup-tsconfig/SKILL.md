---
name: setup-tsconfig
description: Configure or fix the project's tsconfig.json to extend the correct preset from @shibanet0/datamitsu-config/tsconfig/* based on the rules in .datamitsu/tsconfig.md. Detects project type (Next.js app, React app, React library, Node library, backend service, CLI, E2E tests) and monorepo status, picks the matching preset (base/library/service/nextjs/react-library/shared-library/shared-react-library), and rewrites tsconfig.json with a minimal override block. Use this skill whenever the user asks to set up, configure, fix, switch, migrate, or pick a TypeScript config — even if they describe it informally as "what tsconfig should I use", "my tsconfig is wrong", "extend datamitsu tsconfig", "set up TS for this package".
---

# Setup TypeScript Config

Read `.datamitsu/ai/skills/setup-tsconfig/instructions.md` from the project root and follow it precisely.

The instructions file is the source of truth for this skill — it is regenerated automatically by `datamitsu` from the central `datamitsu-config` repository, so it is always up to date with the current presets and rules in `.datamitsu/tsconfig.md`.
