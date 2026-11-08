import { Link, useRouter, type ErrorComponentProps } from "@tanstack/react-router";
import { Compass } from "lucide-react";

import { Button } from "@/components/ui/button";

import { EmptyState } from "./empty-state";
import { ErrorState } from "./error-state";

/** RouteError is the router's default error view. */
export function RouteError({ error, reset }: ErrorComponentProps) {
  const router = useRouter();
  return (
    <ErrorState
      error={error}
      title="This page couldn't load"
      onRetry={() => {
        reset();
        void router.invalidate();
      }}
    />
  );
}

/** NotFound is the router's default not-found view. */
export function NotFound() {
  return (
    <EmptyState
      icon={Compass}
      title="Nothing lives at this address"
      description="The link may be old, or the page was moved."
      action={
        <Button asChild variant="outline">
          <Link to="/jobs">Go to jobs</Link>
        </Button>
      }
    />
  );
}
