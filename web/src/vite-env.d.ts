/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** VITE_SYNCR_MOCK=1 (dev only) serves fixture data instead of the API. */
  readonly VITE_SYNCR_MOCK?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
