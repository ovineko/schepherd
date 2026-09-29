# /cleanup-agents-md

Cleanup and compact the project AGENTS.md — remove duplicates of the shared datamitsu chunks and of other rules in the same file, extract non-rule content (architecture docs, templates, release notes) to separate files, strip migration cruft, move deferred work to `docs/backlog/`, and shrink the rules that remain without losing a single rule, condition, exception, obligation or exact command.

Read `.datamitsu/ai/skills/cleanup-agents-md/instructions.md` from the project root and follow it precisely.

The instructions file is regenerated automatically by `datamitsu` from the central `datamitsu-config` repository, so it is always up to date with the current chunks.
