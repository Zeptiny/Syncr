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
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { errorMessage } from "@/lib/errors";

/** TypedConfirmDialogProps configures TypedConfirmDialog. */
export interface TypedConfirmDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: React.ReactNode;
  description?: React.ReactNode;
  /** name is what the user must type, exactly, to enable the button. */
  name: string;
  /** requireName turns the typed name off (default on): a plain confirmation. */
  requireName?: boolean;
  /** nameError is a server error for the typed name. */
  nameError?: string;
  confirmLabel: string;
  /** destructive styles the confirm button (default true). */
  destructive?: boolean;
  /** disabled blocks the confirm button for another reason, e.g. a missing choice. */
  disabled?: boolean;
  /** onConfirm gets the typed name. The dialog stays open until it settles. */
  onConfirm: (typed: string) => Promise<unknown> | void;
  /** children render above the typed name: warnings, options, lists. */
  children?: React.ReactNode;
}

/**
 * TypedConfirmDialog asks before an action that destroys data: the confirm
 * button stays disabled until the user types the object's name exactly.
 * ConfirmDialog stays the choice for simple confirmations.
 */
export function TypedConfirmDialog({
  open,
  onOpenChange,
  title,
  description,
  name,
  requireName = true,
  nameError,
  confirmLabel,
  destructive = true,
  disabled,
  onConfirm,
  children,
}: TypedConfirmDialogProps) {
  const [typed, setTyped] = useState("");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<unknown>(undefined);
  const matches = !requireName || typed === name;

  const confirm = async () => {
    setPending(true);
    setError(undefined);
    try {
      await onConfirm(typed);
      setTyped("");
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
        if (pending) return;
        setError(undefined);
        if (!o) setTyped("");
        onOpenChange(o);
      }}
    >
      <AlertDialogContent className="max-h-[calc(100dvh-2rem)] overflow-y-auto">
        <AlertDialogHeader>
          <AlertDialogTitle>{title}</AlertDialogTitle>
          {description && <AlertDialogDescription>{description}</AlertDialogDescription>}
        </AlertDialogHeader>
        {children}
        {requireName && (
          <div className="flex flex-col gap-1.5">
            <Label htmlFor="typed-confirm-name" className="text-[13px]">
              Type <span className="font-mono font-semibold">{name}</span> to confirm
            </Label>
            <Input
              id="typed-confirm-name"
              value={typed}
              onChange={(e) => setTyped(e.target.value)}
              autoComplete="off"
              spellCheck={false}
              aria-invalid={nameError ? true : undefined}
            />
            {nameError && <p className="text-xs text-danger">{nameError}</p>}
          </div>
        )}
        {error !== undefined && (
          <p role="alert" className="text-[13px] text-danger">
            {errorMessage(error)}
          </p>
        )}
        <AlertDialogFooter>
          <AlertDialogCancel disabled={pending}>Cancel</AlertDialogCancel>
          <Button
            variant={destructive ? "destructive" : "default"}
            disabled={pending || disabled || !matches}
            onClick={() => void confirm()}
          >
            {pending ? "Working…" : confirmLabel}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
