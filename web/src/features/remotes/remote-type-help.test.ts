import { describe, expect, it } from "vitest";

import { docHref } from "@/features/docs/doc-link";

import { remoteHelp, remoteTypeHelp } from "./remote-type-help";

describe("remoteHelp", () => {
  it("links each built-in type to its section", () => {
    expect(docHref(remoteHelp("smb"))).toBe("/docs/remotes#smb-windows-share");
    expect(docHref(remoteHelp("ftp"))).toBe("/docs/remotes#ftp--ftps");
    expect(docHref(remoteHelp("crypt"))).toBe("/docs/encrypted-remotes");
    expect(Object.keys(remoteTypeHelp).sort()).toEqual([
      "azureblob",
      "b2",
      "crypt",
      "ftp",
      "gcs",
      "local",
      "s3",
      "sftp",
      "smb",
      "union",
      "webdav",
    ]);
  });

  it("falls back to the remotes page for an unknown or missing type", () => {
    expect(remoteHelp("chunker")).toEqual({ page: "remotes" });
    expect(remoteHelp(undefined)).toEqual({ page: "remotes" });
    // Never a property of Object.prototype.
    expect(remoteHelp("toString")).toEqual({ page: "remotes" });
  });
});
