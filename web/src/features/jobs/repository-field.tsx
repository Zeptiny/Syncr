import { Link } from "@tanstack/react-router";

import { FieldShell } from "@/components/app/descriptor-form";
import { RepositoryPicker } from "@/components/app/repository-picker";
import type { Repository } from "@/gen/syncr/v1/restic_pb";

/** RepositoryField is the labelled repository picker of the restic editors. */
export function RepositoryField({
  id,
  repositories,
  value,
  onChange,
  error,
}: {
  id: string;
  repositories: Repository[];
  value: string;
  onChange: (id: string) => void;
  error?: string;
}) {
  return (
    <FieldShell
      id={id}
      label="Repository"
      error={error}
      help={
        repositories.length === 0 ? (
          <>
            You have no repository yet.{" "}
            <Link to="/repositories/new" className="text-primary hover:underline">
              Add a repository
            </Link>{" "}
            first.
          </>
        ) : (
          "Jobs on the same repository run one after another."
        )
      }
    >
      <RepositoryPicker
        id={id}
        repositories={repositories}
        value={value}
        onChange={onChange}
        invalid={!!error}
        className="bg-card"
      />
    </FieldShell>
  );
}
