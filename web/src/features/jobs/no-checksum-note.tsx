import { DocLink } from "@/features/docs/doc-link";

import { noChecksumNote } from "./checksums";

/**
 * NoChecksumNote says why checksum comparison is not offered (the remotes
 * named keep none) and links to the docs' explanation.
 */
export function NoChecksumNote({ names }: { names: string[] }) {
  return (
    <>
      <span>{noChecksumNote(names)}</span>{" "}
      <DocLink
        page="remotes"
        anchor="checksums"
        newTab
        className="font-medium text-primary hover:underline"
      >
        Which remotes keep none
      </DocLink>
    </>
  );
}
