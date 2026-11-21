// npm run generate:docs: checks docs/ and writes src/features/docs/docs.gen.ts
// with the options of web/vite.config.ts. Node runs this TypeScript natively.

import { fileURLToPath } from "node:url";

import { docsOptions } from "../vite.config.ts";
import { generateDocs } from "./index.ts";

const webRoot = fileURLToPath(new URL("..", import.meta.url));
const result = generateDocs(docsOptions, webRoot);
if (result.errors !== undefined) {
  console.error(result.errors);
  process.exitCode = 1;
} else {
  console.log(`${docsOptions.genFile} ${result.written ? "written" : "is up to date"}`);
}
