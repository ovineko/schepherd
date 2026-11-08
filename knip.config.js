import { defineConfig } from "./.datamitsu/knip.config.js";

export default defineConfig((base) => ({
  ...base,
  entry: [...base.entry, "stylelint.config.mjs", "oxfmt.config.ts"],
  ignore: [...(base.ignore ?? []), ".claude/worktrees/**"],
  workspaces: {
    ...base.workspaces,
    "**": {
      ...base.workspaces["**"],
      entry: [
        ...base.workspaces["**"].entry,
        "bin/*.{js,mjs,cjs,jsx,ts,tsx,mts,cts}!",
        "lib/launcher.js!",
        "stylelint.config.mjs",
        "oxfmt.config.ts",
      ],
    },
  },
}));
