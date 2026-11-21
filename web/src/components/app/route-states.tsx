import { Link, useRouter, type ErrorComponentProps } from "@tanstack/react-router";
import { Compass } from "lucide-react";

import { Button } from "@/components/ui/button";

import { EmptyState } from "./empty-state";
import { ErrorState } from "./error-state";

/**
 * isChunkLoadError reports a failed load of one of the UI's own code chunks.
 * A tab left open across a controller upgrade asks for chunks the new
 * version no longer has: only a reload gets the new ones.
 */
function isChunkLoadError(err: unknown): boolean {
  return (
    err instanceof Error &&
    /dynamically imported module|Importing a module script failed|Unable to preload CSS/i.test(
      err.message,
    )
  );
}

/** RouteError is the router's default error view. */
export function RouteError({ error, reset }: ErrorComponentProps) {
  const router = useRouter();
  if (isChunkLoadError(error)) {
    return (
      <ErrorState
        error={error}
        title="This page needs a reload"
        description="Syncr may have been updated since this tab was opened."
        retryLabel="Reload"
        onRetry={() => window.location.reload()}
      />
    );
  }
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
