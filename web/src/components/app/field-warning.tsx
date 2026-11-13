import { TriangleAlert } from "lucide-react";

/**
 * FieldWarning is a warning under a field: something that will likely go
 * wrong but does not stop the form (an error does). Give it an id and
 * point the field's aria-describedby at it.
 */
export function FieldWarning({ id, children }: { id?: string; children: React.ReactNode }) {
  return (
    <p
      id={id}
      className="flex w-fit items-start gap-1.5 rounded-md bg-warning-soft px-2 py-1 text-xs leading-5 text-warning"
    >
      <TriangleAlert className="mt-[3px] size-3.5 shrink-0" aria-hidden />
      <span>{children}</span>
    </p>
  );
}
