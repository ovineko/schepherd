---
name: cleanup-agents-md
description: Cleanup and compact the project AGENTS.md — remove duplicates of the shared datamitsu chunks and of other rules in the same file, extract non-rule content (architecture docs, templates, release notes) to separate files, strip migration cruft, move deferred work to docs/backlog/, and shrink the rules that remain without losing a single rule, condition, exception, obligation or exact command. Use this skill whenever the user asks to compress, clean up, deduplicate, slim down, sync, compact, or reduce the token cost of AGENTS.md against the datamitsu config — even if they describe it informally as "AGENTS.md got too big", "remove duplicates from AGENTS.md", "fix my agents file", or "extract architecture from AGENTS.md".
---

# Cleanup AGENTS.md

Read `.datamitsu/ai/skills/cleanup-agents-md/instructions.md` from the project root and follow it precisely.

The instructions file is the source of truth for this skill — it is regenerated automatically by `datamitsu` from the central `datamitsu-config` repository, so it is always up to date with the current chunks.
