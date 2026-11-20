import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";

import { JobSchema, JobStatus } from "@/gen/syncr/v1/api_pb";
import { JobErrorCode } from "@/gen/syncr/v1/link_pb";
import * as fx from "@/test/fixtures";

import { errorHint } from "./model";

const backends = new Map(fx.backends.map((b) => [b.id, b]));

// The worker's wrong-password errors on an encrypted remote (rclone 1.75.2;
// CRYPT-STEP-A-REPORT), as a transfer from it reports them.
function failed(code: JobErrorCode, message: string, src = fx.ids.vault) {
  return create(JobSchema, {
    status: JobStatus.FAILED,
    kind: "rclone_transfer",
    definition: {
      operation: {
        case: "rcloneTransfer",
        value: { source: { remoteId: src, path: "photos" }, destination: { path: "/srv/x" } },
      },
    },
    remotes: [
      { id: fx.ids.vault, name: "Vault", backendType: "crypt" },
      { id: fx.ids.minio, name: "MinIO archive", backendType: "s3" },
    ],
    error: { code, message },
  });
}

describe("errorHint on a remote whose data needs its password", () => {
  it("suggests the password for a folder that is not found", () => {
    expect(errorHint(failed(JobErrorCode.NOT_FOUND, "rclone: directory not found"), backends)).toBe(
      "If the folder exists on Vault, the remote's password or salt (second password) may be wrong: with another one, the folder's stored name differs, so it is not found.",
    );
  });

  it("suggests the password when names or data cannot be decrypted", () => {
    for (const message of [
      "there were 2 undecryptable name errors",
      "failed to authenticate decrypted block - bad password?",
    ]) {
      expect(errorHint(failed(JobErrorCode.EXECUTION_FAILED, message), backends)).toMatch(
        /^Vault could not be read: its password or salt \(second password\) is probably wrong/,
      );
    }
  });

  it("adds nothing when the worker already names the password, or for other remotes", () => {
    expect(
      errorHint(
        failed(
          JobErrorCode.NOT_FOUND,
          'source: "photos" is not in its parent folder on the crypt remote "r1"; if it exists, the remote\'s password is wrong',
        ),
        backends,
      ),
    ).toBeUndefined();
    expect(
      errorHint(failed(JobErrorCode.NOT_FOUND, "directory not found", fx.ids.minio), backends),
    ).toBeUndefined();
    expect(
      errorHint(failed(JobErrorCode.EXECUTION_FAILED, "401 Unauthorized"), backends),
    ).toBeUndefined();
    // Without the descriptors nothing is known.
    expect(errorHint(failed(JobErrorCode.NOT_FOUND, "directory not found"))).toBeUndefined();
  });
});
