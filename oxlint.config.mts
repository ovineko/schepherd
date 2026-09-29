import { defineConfig } from "./.datamitsu/oxlint.config.js";
import packageJSON from "./package.json" with { type: "json" };

export default defineConfig(packageJSON);
