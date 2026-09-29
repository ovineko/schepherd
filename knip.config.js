import { defineConfig } from "./.datamitsu/knip.config.js";

export default defineConfig({
  // oxfmt and stylelint read only the git root's config; these copies exist
  // for editors opened on the launcher directory. The repository has no
  // stylesheets for stylelint to check.
  ignore: [
    "packaging/npm/schepherd/oxfmt.config.ts",
    "packaging/npm/schepherd/stylelint.config.mjs",
    "stylelint.config.mjs",
  ],
  workspaces: {
    // The npm launcher is published from this directory: its bin script and
    // its tests are what use the lib modules.
    "packaging/npm/schepherd": { entry: ["bin/schepherd.js", "test/*.test.js"] },
  },
});
