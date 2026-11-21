// The modules web/docs-plugin generates from docs/ at build time.

declare module "virtual:syncr-docs/manifest" {
  import type { DocPage } from "@/features/docs/docs.gen";

  /**
   * HTML rendered from the repository's docs at build time (no raw HTML from
   * authors, links and images checked). Only these modules produce it; it is
   * the one input the viewer may pass to dangerouslySetInnerHTML, after
   * replacing the GitHub ref token with a validated ref.
   */
  export type DocHtml = string & { readonly __brand: "DocHtml" };

  export interface DocHeading {
    depth: 2 | 3;
    id: string;
    text: string;
  }

  /** The default export of virtual:syncr-docs/page/<slug>. */
  export interface DocContent {
    title: string;
    html: DocHtml;
    /** H2 and H3, for "On this page". */
    headings: DocHeading[];
  }

  export interface DocNavItem {
    label: string;
    /** A shipped page. */
    slug?: DocPage;
    /** Not shipped: a GitHub URL that contains githubRefToken. */
    external?: string;
    children?: DocNavItem[];
  }

  export interface DocManifest {
    /** The H1 of docs/README.md. */
    title: string;
    /** For example https://github.com/Zeptiny/Syncr. */
    githubRepo: string;
    /** Stands for the Git ref in every GitHub URL (HTML and external entries). */
    githubRefToken: "__SYNCR_GITHUB_REF__";
    /** The sections of docs/README.md, in order. */
    groups: { title: string; items: DocNavItem[] }[];
    /** Reading order (Previous/Next); file is the path in the repository. */
    pages: { slug: DocPage; title: string; file: string }[];
    /** One lazy chunk per page. */
    loaders: Record<DocPage, () => Promise<{ default: DocContent }>>;
  }

  const manifest: DocManifest;
  export default manifest;
}

declare module "virtual:syncr-docs/page/*" {
  import type { DocContent } from "virtual:syncr-docs/manifest";

  const page: DocContent;
  export default page;
}

declare module "virtual:syncr-docs/search" {
  import type { DocPage } from "@/features/docs/docs.gen";

  /** [index into pages, heading ID ("" for the intro), heading ("" for the intro), plain text] */
  export type DocSearchSection = [number, string, string, string];

  export interface DocSearchIndex {
    /** Every shipped page except the index. */
    pages: { slug: DocPage; title: string }[];
    sections: DocSearchSection[];
  }

  const index: DocSearchIndex;
  export default index;
}
