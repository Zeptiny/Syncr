import type { Interceptor, Transport } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";

/** CSRF_HEADER must accompany every cookie-authenticated call. */
export const CSRF_HEADER = "X-Syncr-CSRF";

/**
 * csrfHeader adds `X-Syncr-CSRF: 1`. Another origin cannot send it without
 * a CORS preflight, which the controller never allows.
 */
export const csrfHeader: Interceptor = (next) => (req) => {
  req.header.set(CSRF_HEADER, "1");
  return next(req);
};

/**
 * createTransport returns the Connect (JSON) transport for the controller
 * that served the page. The session cookie travels with same-origin calls.
 */
export function createTransport(baseUrl = "/"): Transport {
  return createConnectTransport({
    baseUrl,
    useBinaryFormat: false,
    interceptors: [csrfHeader],
    fetch: (input, init) => globalThis.fetch(input, { ...init, credentials: "same-origin" }),
  });
}
