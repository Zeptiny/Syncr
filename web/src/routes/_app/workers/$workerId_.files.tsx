import { createFileRoute } from "@tanstack/react-router";

import { validateWorkerFilesSearch } from "@/features/files/search";
import { WorkerFilesPage } from "@/features/files/worker-files-page";

export const Route = createFileRoute("/_app/workers/$workerId_/files")({
  validateSearch: validateWorkerFilesSearch,
  component: WorkerFiles,
});

function WorkerFiles() {
  const { workerId } = Route.useParams();
  const { path } = Route.useSearch();
  return <WorkerFilesPage key={workerId} workerId={workerId} path={path} />;
}
