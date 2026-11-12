import { useQuery } from "@connectrpc/connect-query";
import { createFileRoute } from "@tanstack/react-router";

import { ErrorState } from "@/components/app/error-state";
import { Skeleton } from "@/components/ui/skeleton";
import { JobService } from "@/gen/syncr/v1/api_pb";
import { RunJobPage } from "@/features/jobs/run-job-page";
import { validateRunJobSearch } from "@/features/jobs/search";

export const Route = createFileRoute("/_app/jobs/new")({
  validateSearch: validateRunJobSearch,
  component: NewJob,
});

function NewJob() {
  const { from, kind, repository } = Route.useSearch();
  const q = useQuery(JobService.method.getJob, { jobId: from ?? "" }, { enabled: !!from });
  if (!from) return <RunJobPage kind={kind} repository={repository} />;
  if (q.error) return <ErrorState error={q.error} onRetry={() => void q.refetch()} />;
  if (!q.data?.job) return <Skeleton className="h-96 w-full max-w-5xl" />;
  return <RunJobPage key={from} from={q.data.job} />;
}
