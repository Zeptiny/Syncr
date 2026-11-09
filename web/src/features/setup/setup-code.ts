/** The shortest setup code the server accepts, in characters besides dashes and spaces. */
export const MIN_SETUP_CODE_LEN = 16;

/** The length of a code the controller generates (20 base32 characters). */
const GENERATED_LEN = 20;

/** Only the characters of a generated code (base32) plus its separators. */
const GENERATED_SHAPE = /^[A-Za-z2-7\s-]*$/;

/**
 * formatSetupCode groups input as XXXXX-XXXXX-XXXXX-XXXXX while it still looks
 * like a code the controller generated (base32 letters and digits, at most 20
 * of them). Anything else is a preset auth.setup_code: it is returned as typed,
 * never stripped or truncated. The server ignores case, dashes and spaces in
 * both kinds, so the grouping only helps the eye.
 */
export function formatSetupCode(raw: string): string {
  if (!GENERATED_SHAPE.test(raw)) return raw;
  const chars = raw.replace(/[\s-]/g, "").toUpperCase();
  if (chars.length > GENERATED_LEN) return raw;
  return chars.match(/.{1,5}/g)?.join("-") ?? "";
}

/** setupCodeLength counts the characters the server counts: all but dashes and white space. */
export function setupCodeLength(code: string): number {
  return Array.from(code.replace(/[\s-]/g, "")).length;
}
