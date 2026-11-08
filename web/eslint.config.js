import js from "@eslint/js";
import reactHooks from "eslint-plugin-react-hooks";
import reactRefresh from "eslint-plugin-react-refresh";
import { defineConfig, globalIgnores } from "eslint/config";
import globals from "globals";
import tseslint from "typescript-eslint";

export default defineConfig([
  globalIgnores(["dist", "node_modules", "src/gen", "src/routeTree.gen.ts", "placeholder"]),
  {
    files: ["**/*.{ts,tsx}"],
    extends: [
      js.configs.recommended,
      tseslint.configs.recommendedTypeChecked,
      reactHooks.configs.flat.recommended,
      reactRefresh.configs.vite,
    ],
    languageOptions: {
      ecmaVersion: 2023,
      globals: globals.browser,
      parserOptions: {
        projectService: true,
        tsconfigRootDir: import.meta.dirname,
      },
    },
    rules: {
      "@typescript-eslint/no-unused-vars": [
        "error",
        { argsIgnorePattern: "^_", varsIgnorePattern: "^_" },
      ],
      "@typescript-eslint/no-misused-promises": [
        "error",
        { checksVoidReturn: { attributes: false } },
      ],
      // TanStack Table and react-hook-form are known to the compiler rules;
      // the app does not use the React Compiler.
      "react-hooks/incompatible-library": "off",
      // TanStack Router redirects and not-found results are thrown on purpose.
      "@typescript-eslint/only-throw-error": [
        "error",
        {
          allow: [
            {
              from: "package",
              package: "@tanstack/router-core",
              name: ["Redirect", "NotFoundError"],
            },
          ],
        },
      ],
    },
  },
  {
    // shadcn/ui sources export variants next to components; routes export
    // their Route object; test helpers are not components.
    files: [
      "src/components/ui/**",
      "src/routes/**",
      "src/test/**",
      "src/mock/**",
      "src/**/*.test.tsx",
      "src/lib/**",
      "src/hooks/**",
      // Feature modules are page bundles; their helpers sit next to the page.
      "src/features/**",
      "src/router.tsx",
    ],
    rules: { "react-refresh/only-export-components": "off" },
  },
  {
    files: ["scripts/**/*.mjs", "*.js"],
    extends: [js.configs.recommended],
    languageOptions: { globals: globals.node },
  },
]);
