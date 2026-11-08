import { Code } from "@connectrpc/connect";
import { useMutation } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useState } from "react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { AuthService } from "@/gen/syncr/v1/auth_pb";
import { forgetSession } from "@/hooks/use-session";
import { errorMessage, hasCode } from "@/lib/errors";

import { AuthLayout } from "./auth-layout";

/** LoginPage signs a user in and returns them to where they were going. */
export function LoginPage({ redirect }: { redirect?: string }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const login = useMutation(AuthService.method.login);
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | undefined>();

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(undefined);
    try {
      await login.mutateAsync({ username: username.trim().toLowerCase(), password });
      forgetSession(queryClient);
      await navigate({ href: redirect ?? "/", replace: true });
    } catch (err) {
      setError(
        hasCode(err, Code.Unauthenticated) || hasCode(err, Code.InvalidArgument)
          ? "That username and password don't match. Try again."
          : errorMessage(err),
      );
    }
  };

  return (
    <AuthLayout>
      <h1 className="text-[22px] leading-8 font-semibold tracking-[-0.01em]">Sign in</h1>
      <p className="mt-1 text-muted-foreground">
        Use the account an administrator created for you.
      </p>
      <form onSubmit={(e) => void submit(e)} className="mt-8 flex flex-col gap-5" noValidate>
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="username">Username</Label>
          <Input
            id="username"
            autoComplete="username"
            autoCapitalize="none"
            spellCheck={false}
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            required
            autoFocus
            className="h-10 bg-card"
          />
        </div>
        <div className="flex flex-col gap-1.5">
          <Label htmlFor="password">Password</Label>
          <Input
            id="password"
            type="password"
            autoComplete="current-password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            required
            className="h-10 bg-card"
          />
        </div>
        {error && (
          <p role="alert" className="rounded-md bg-danger-soft px-3 py-2 text-[13px] text-danger">
            {error}
          </p>
        )}
        <Button type="submit" size="lg" disabled={login.isPending || !username || !password}>
          {login.isPending ? "Signing in…" : "Sign in"}
        </Button>
      </form>
      <p className="mt-8 text-xs leading-5 text-muted-foreground">
        Forgot your password? An administrator can reset it from the Users page.
      </p>
    </AuthLayout>
  );
}
