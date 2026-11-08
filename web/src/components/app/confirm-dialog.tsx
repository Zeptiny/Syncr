import { useState } from "react";

import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { errorMessage } from "@/lib/errors";

/** ConfirmDialogProps configures ConfirmDialog. */
export interface ConfirmDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: React.ReactNode;
  description?: React.ReactNode;
  confirmLabel: string;
  /** destructive styles the confirm button for irreversible actions. */
  destructive?: boolean;
  /** onConfirm may return a promise; the dialog stays open until it settles. */
  onConfirm: () => Promise<unknown> | void;
  children?: React.ReactNode;
}

/** ConfirmDialog asks before an action that cannot be undone. */
export function ConfirmDialog({
  open,
  onOpenChange,
  title,
  description,
  confirmLabel,
  destructive,
  onConfirm,
  children,
}: ConfirmDialogProps) {
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<unknown>(undefined);

  const confirm = async () => {
    setPending(true);
    setError(undefined);
    try {
      await onConfirm();
      onOpenChange(false);
    } catch (err) {
      setError(err);
    } finally {
      setPending(false);
    }
  };

  return (
    <AlertDialog
      open={open}
      onOpenChange={(o) => {
        if (!pending) {
          setError(undefined);
          onOpenChange(o);
        }
      }}
    >
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{title}</AlertDialogTitle>
          {description && <AlertDialogDescription>{description}</AlertDialogDescription>}
        </AlertDialogHeader>
        {children}
        {error !== undefined && (
          <p role="alert" className="text-[13px] text-danger">
            {errorMessage(error)}
          </p>
        )}
        <AlertDialogFooter>
          <AlertDialogCancel disabled={pending}>Cancel</AlertDialogCancel>
          <Button
            variant={destructive ? "destructive" : "default"}
            disabled={pending}
            onClick={() => void confirm()}
          >
            {pending ? "Working…" : confirmLabel}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
