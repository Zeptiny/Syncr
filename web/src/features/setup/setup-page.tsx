import { useMutation } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { Check } from "lucide-react";
import { useState } from "react";

import { FieldShell } from "@/components/app/descriptor-form";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { SetupService } from "@/gen/syncr/v1/auth_pb";
import { forgetSession } from "@/hooks/use-session";
import { errorMessage, fieldViolations } from "@/lib/errors";
import { cn } from "@/lib/utils";
import { AuthLayout } from "@/features/auth/auth-layout";
import { MIN_PASSWORD } from "@/features/users/users-page";
import { ActiveEnrollment } from "@/features/workers/enrollment";

/** SetupStep is the wizard's current step. */
export type SetupStep = "code" | "account" | "worker";

const steps: { id: SetupStep; label: string }[] = [
  { id: "code", label: "Setup code" },
  { id: "account", label: "Admin account" },
  { id: "worker", label: "First worker" },
];

/** formatSetupCode normalizes input to XXXXX-XXXXX-XXXXX-XXXXX. */
export function formatSetupCode(raw: string): string {
  const chars = raw
    .toUpperCase()
    .replace(/[^A-Z2-7]/g, "")
    .slice(0, 20);
  return chars.match(/.{1,5}/g)?.join("-") ?? "";
}

function Stepper({ current }: { current: SetupStep }) {
  const idx = steps.findIndex((s) => s.id === current);
  return (
    <ol className="mb-8 flex items-center gap-2" aria-label="Setup progress">
      {steps.map((s, i) => (
        <li
          key={s.id}
          className="flex flex-1 items-center gap-2"
          aria-current={i === idx ? "step" : undefined}
        >
          <span
            className={cn(
              "grid size-6 shrink-0 place-items-center rounded-full text-xs font-semibold",
              i < idx && "bg-primary text-primary-foreground",
              i === idx && "border-2 border-primary text-primary",
              i > idx && "border text-muted-foreground",
            )}
          >
            {i < idx ? <Check className="size-3.5" aria-hidden /> : i + 1}
          </span>
          <span
            className={cn("truncate text-xs", i === idx ? "font-medium" : "text-muted-foreground")}
          >
            {s.label}
          </span>
          {i < steps.length - 1 && (
            <span className="hidden h-px flex-1 bg-border sm:block" aria-hidden />
          )}
        </li>
      ))}
    </ol>
  );
}

/** SetupPage is the first-run wizard: setup code, admin account, first worker. */
export function SetupPage({ step }: { step: SetupStep }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const complete = useMutation(SetupService.method.completeSetup);
  const [current, setCurrent] = useState<SetupStep>(step);
  const [code, setCode] = useState("");
  const [account, setAccount] = useState({
    username: "admin",
    displayName: "",
    password: "",
    confirm: "",
  });
  const [errors, setErrors] = useState<Record<string, string>>({});

  const goTo = (s: SetupStep) => {
    setCurrent(s);
    setErrors({});
  };

  const submitCode = (e: React.FormEvent) => {
    e.preventDefault();
    if (code.replaceAll("-", "").length < 16) {
      setErrors({ setup_code: "Enter the whole code from the controller's log." });
      return;
    }
    goTo("account");
  };

  const submitAccount = async (e: React.FormEvent) => {
    e.preventDefault();
    const local: Record<string, string> = {};
    if (!/^[a-z0-9][a-z0-9._-]{0,63}$/.test(account.username)) {
      local.username = "Use lower-case letters, digits, dots, dashes or underscores.";
    }
    if (account.password.length < MIN_PASSWORD)
      local.password = `Use at least ${MIN_PASSWORD} characters.`;
    else if (account.password !== account.confirm) local.confirm = "The passwords don't match.";
    setErrors(local);
    if (Object.keys(local).length) return;
    try {
      await complete.mutateAsync({
        setupCode: code,
        username: account.username,
        displayName: account.displayName.trim(),
        password: account.password,
      });
      forgetSession(queryClient);
      setCurrent("worker");
      await navigate({ to: "/setup", search: { step: "worker" }, replace: true });
    } catch (err) {
      const v = Object.fromEntries(fieldViolations(err).map((x) => [x.field, x.message]));
      if (v.setup_code) {
        setCurrent("code");
        setErrors({ setup_code: v.setup_code });
      } else if (Object.keys(v).length) {
        setErrors(v);
      } else {
        setErrors({ form: errorMessage(err) });
      }
    }
  };

  return (
    <AuthLayout wide={current === "worker"}>
      <Stepper current={current} />
      {current === "code" && (
        <form onSubmit={submitCode} noValidate className="flex flex-col gap-6">
          <div>
            <h1 className="text-[22px] leading-8 font-semibold tracking-[-0.01em]">Set up Syncr</h1>
            <p className="mt-1 text-muted-foreground">
              To prove you run this controller, enter the setup code it wrote to its log when it
              started. Look for the line <span className="text-foreground">“first-run setup”</span>.
            </p>
          </div>
          <FieldShell id="setup-code" label="Setup code" error={errors.setup_code}>
            <Input
              id="setup-code"
              value={code}
              onChange={(e) => setCode(formatSetupCode(e.target.value))}
              placeholder="XXXXX-XXXXX-XXXXX-XXXXX"
              autoComplete="one-time-code"
              autoCapitalize="characters"
              spellCheck={false}
              autoFocus
              className="h-11 bg-card text-center font-mono text-[15px] tracking-[0.12em]"
            />
          </FieldShell>
          <Button type="submit" size="lg">
            Continue
          </Button>
        </form>
      )}
      {current === "account" && (
        <form onSubmit={(e) => void submitAccount(e)} noValidate className="flex flex-col gap-5">
          <div>
            <h1 className="text-[22px] leading-8 font-semibold tracking-[-0.01em]">
              Create the admin account
            </h1>
            <p className="mt-1 text-muted-foreground">
              You can add more users, and more administrators, later.
            </p>
          </div>
          <FieldShell id="setup-username" label="Username" error={errors.username}>
            <Input
              id="setup-username"
              autoComplete="username"
              autoCapitalize="none"
              className="bg-card"
              value={account.username}
              onChange={(e) => setAccount({ ...account, username: e.target.value.toLowerCase() })}
            />
          </FieldShell>
          <FieldShell id="setup-display" label="Your name" optional error={errors.display_name}>
            <Input
              id="setup-display"
              autoComplete="name"
              className="bg-card"
              value={account.displayName}
              onChange={(e) => setAccount({ ...account, displayName: e.target.value })}
            />
          </FieldShell>
          <FieldShell
            id="setup-password"
            label="Password"
            error={errors.password}
            help={`At least ${MIN_PASSWORD} characters.`}
          >
            <Input
              id="setup-password"
              type="password"
              autoComplete="new-password"
              className="bg-card"
              value={account.password}
              onChange={(e) => setAccount({ ...account, password: e.target.value })}
            />
          </FieldShell>
          <FieldShell id="setup-confirm" label="Repeat password" error={errors.confirm}>
            <Input
              id="setup-confirm"
              type="password"
              autoComplete="new-password"
              className="bg-card"
              value={account.confirm}
              onChange={(e) => setAccount({ ...account, confirm: e.target.value })}
            />
          </FieldShell>
          {errors.form && (
            <p role="alert" className="rounded-md bg-danger-soft px-3 py-2 text-[13px] text-danger">
              {errors.form}
            </p>
          )}
          <div className="flex gap-2">
            <Button type="button" variant="outline" size="lg" onClick={() => goTo("code")}>
              Back
            </Button>
            <Button type="submit" size="lg" className="flex-1" disabled={complete.isPending}>
              {complete.isPending ? "Creating…" : "Create account"}
            </Button>
          </div>
        </form>
      )}
      {current === "worker" && (
        <div className="flex flex-col gap-6">
          <div>
            <h1 className="text-[22px] leading-8 font-semibold tracking-[-0.01em]">
              Add your first worker
            </h1>
            <p className="mt-1 text-muted-foreground">
              Run this on a machine that holds files you want to move or back up. You can also do it
              later from Workers.
            </p>
          </div>
          <ActiveEnrollment />
          <Button size="lg" variant="outline" onClick={() => void navigate({ to: "/jobs" })}>
            Continue to Syncr
          </Button>
        </div>
      )}
    </AuthLayout>
  );
}
