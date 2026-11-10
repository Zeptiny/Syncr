import { ResticRestoreOverwrite } from "@/gen/syncr/v1/link_pb";

import { ChoiceCards } from "./choice-cards";

/** overwriteOptions are the four answers to "a file is already there". */
const overwriteOptions: { value: ResticRestoreOverwrite; label: string; help: string }[] = [
  {
    value: ResticRestoreOverwrite.NEVER,
    label: "Keep existing",
    help: "Files that are already there are left alone. Only missing files are restored.",
  },
  {
    value: ResticRestoreOverwrite.IF_CHANGED,
    label: "Replace files that differ",
    help: "A file is replaced when its content differs from the snapshot.",
  },
  {
    value: ResticRestoreOverwrite.IF_NEWER,
    label: "Replace older files",
    help: "A file is replaced when the snapshot's copy is newer.",
  },
  {
    value: ResticRestoreOverwrite.ALWAYS,
    label: "Replace all",
    help: "Every file in the way is overwritten with the snapshot's copy.",
  },
];

/**
 * OverwriteChoice picks what a restore does with files that already exist
 * in the target. An unset value shows as "Keep existing", the safe default.
 */
export function OverwriteChoice({
  value,
  onChange,
  labelId = "restore-overwrite",
  className,
}: {
  value: ResticRestoreOverwrite;
  onChange: (v: ResticRestoreOverwrite) => void;
  labelId?: string;
  className?: string;
}) {
  return (
    <ChoiceCards
      label="If a file already exists"
      labelId={labelId}
      value={value === ResticRestoreOverwrite.UNSPECIFIED ? ResticRestoreOverwrite.NEVER : value}
      onChange={onChange}
      options={overwriteOptions}
      className={className ?? "sm:grid-cols-2"}
    />
  );
}
