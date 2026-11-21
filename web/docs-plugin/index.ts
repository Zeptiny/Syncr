// syncrDocs: the Vite plugin that renders docs/ at build time into lazy
// virtual modules ("Documentation in the UI" in docs/development.md).
//
//   virtual:syncr-docs/manifest       title, navigation, reading order, page loaders
//   virtual:syncr-docs/page/<slug>    { title, html, headings } of one page ("README" is the index)
//   virtual:syncr-docs/search         the search index

import fs from "node:fs";
import path from "node:path";

import { normalizePath, type Plugin, type ViteDevServer } from "vite";

import {
  genFileText,
  MANIFEST_ID,
  manifestModule,
  PAGE_PREFIX,
  pageModule,
  SEARCH_ID,
  searchModule,
  slugFromKey,
} from "./codegen.ts";
import { buildDocs, type DocsBuild, type DocsModel, type DocsOptions } from "./model.ts";

export type { DocsOptions } from "./model.ts";

const VIRTUAL_PREFIX = "\0virtual:syncr-docs/";
const WATCHED_RE = /\.(md|png|webp|svg)$/i;

/** The file's text with \n line endings (a Windows checkout may have CRLF). */
function readText(file: string): string | undefined {
  try {
    return fs.readFileSync(file, "utf8").replace(/\r\n/g, "\n");
  } catch {
    return undefined;
  }
}

/** Writes the generated types file when its text changed; true when written. */
function writeIfChanged(file: string, text: string): boolean {
  if (readText(file) === text) return false;
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, text);
  return true;
}

export interface GenerateResult {
  /** The formatted errors, or undefined when the docs are fine. */
  errors?: string;
  written: boolean;
}

/** What `npm run generate:docs` does: check the docs and write the types file. */
export function generateDocs(options: DocsOptions, webRoot: string): GenerateResult {
  const { model, errors } = buildDocs(options);
  if (errors.size > 0) return { errors: errors.format(), written: false };
  return { written: writeIfChanged(path.resolve(webRoot, options.genFile), genFileText(model)) };
}

export function syncrDocs(options: DocsOptions): Plugin {
  let command: "build" | "serve" = "serve";
  let root = process.cwd();
  /** The model served: the last good one (best effort before the first). */
  let current: DocsModel | undefined;
  /** The error shown in the dev overlay until the docs are fixed. */
  let devError: string | undefined;
  const strict = (): boolean => command === "build" || process.env.VITEST !== undefined;
  const genPath = (): string => path.resolve(root, options.genFile);
  const imagePath = (file: string): string =>
    normalizePath(path.join(options.repoRoot, ...file.split("/")));

  const build = (): DocsBuild => buildDocs(options);

  const sendError = (server: ViteDevServer): void => {
    if (devError === undefined) return;
    server.ws.send({ type: "error", err: { message: devError, stack: "", plugin: "syncr-docs" } });
  };

  return {
    name: "syncr-docs",

    config() {
      // Page chunks are named after the page: assets/doc-<slug>-<hash>.js.
      return {
        build: {
          rolldownOptions: {
            output: {
              chunkFileNames: (chunk: { facadeModuleId: string | null }) => {
                const id = chunk.facadeModuleId ?? "";
                if (id.startsWith(`\0${PAGE_PREFIX}`)) {
                  return `assets/doc-${id.slice(PAGE_PREFIX.length + 1).replaceAll("/", "-")}-[hash].js`;
                }
                if (id === `\0${SEARCH_ID}`) return "assets/doc-search-[hash].js";
                if (id === `\0${MANIFEST_ID}`) return "assets/doc-manifest-[hash].js";
                return "assets/[name]-[hash].js";
              },
            },
          },
        },
      };
    },

    configResolved(config) {
      command = config.command;
      root = config.root;
    },

    buildStart() {
      const { model, errors } = build();
      if (errors.size > 0) {
        if (strict()) this.error(errors.format());
        devError = errors.format();
        this.warn(devError);
        current ??= model;
        return;
      }
      current = model;
      devError = undefined;
      const text = genFileText(model);
      if (command === "build") {
        if (readText(genPath()) !== text) {
          this.error(`${normalizePath(options.genFile)} is out of date: run npm run generate:docs`);
        }
      } else {
        writeIfChanged(genPath(), text);
      }
    },

    resolveId(id) {
      if (id === MANIFEST_ID || id === SEARCH_ID || id.startsWith(PAGE_PREFIX)) return `\0${id}`;
      return undefined;
    },

    load(id) {
      if (!id.startsWith(VIRTUAL_PREFIX)) return undefined;
      current ??= build().model;
      if (id === `\0${MANIFEST_ID}`) return manifestModule(current);
      if (id === `\0${SEARCH_ID}`) return searchModule(current);
      const slug = slugFromKey(id.slice(PAGE_PREFIX.length + 1));
      const page = id.startsWith(`\0${PAGE_PREFIX}`)
        ? current.pages.find((p) => p.slug === slug)
        : undefined;
      if (!page) this.error(`no docs page "${slug}" (see docs/README.md)`);
      return pageModule(page, imagePath);
    },

    configureServer(server) {
      const docsDir = normalizePath(options.docsDir);
      server.watcher.add(options.docsDir);
      server.ws.on("connection", () => sendError(server));

      let timer: ReturnType<typeof setTimeout> | undefined;
      const rebuild = (): void => {
        timer = undefined;
        const { model, errors } = build();
        if (errors.size > 0) {
          devError = errors.format();
          server.config.logger.error(devError, { timestamp: true });
          sendError(server);
          return; // the last good model keeps serving
        }
        current = model;
        devError = undefined;
        writeIfChanged(genPath(), genFileText(model));
        for (const env of Object.values(server.environments)) {
          for (const [id, mod] of env.moduleGraph.idToModuleMap) {
            if (id.startsWith(VIRTUAL_PREFIX)) env.moduleGraph.invalidateModule(mod);
          }
        }
        server.ws.send({ type: "full-reload" });
      };
      const onChange = (file: string): void => {
        const f = normalizePath(file);
        if (!f.startsWith(`${docsDir}/`) || !WATCHED_RE.test(f)) return;
        // One rebuild for a burst of changes (an editor saving, a git checkout).
        if (timer !== undefined) clearTimeout(timer);
        timer = setTimeout(rebuild, 50);
      };
      server.watcher.on("add", onChange);
      server.watcher.on("change", onChange);
      server.watcher.on("unlink", onChange);
    },
  };
}
