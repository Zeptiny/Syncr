/** URL_SAFE_ALPHABET is the base64url alphabet: letters, digits, "-" and "_". */
export const URL_SAFE_ALPHABET = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";

/** SECRET_LENGTH is the length of a generated secret (32 characters, 192 bits). */
export const SECRET_LENGTH = 32;

/**
 * randomSecret returns length characters of the URL-safe alphabet, drawn
 * with the browser's secure generator. The alphabet has 64 characters, so
 * the low six bits of each byte pick one without bias.
 */
export function randomSecret(length = SECRET_LENGTH): string {
  const bytes = crypto.getRandomValues(new Uint8Array(length));
  let out = "";
  for (const b of bytes) out += URL_SAFE_ALPHABET[b & 63];
  return out;
}
