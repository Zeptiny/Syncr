import { useNavigate } from "@tanstack/react-router";

import { PageHeader } from "@/components/app/page-header";
import { Panel } from "@/components/app/panel";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";

import { ActiveEnrollment, PassiveSetup } from "./enrollment";

/** AddWorkerTab is the selected way of connecting a worker. */
export type AddWorkerTab = "active" | "passive";

/** AddWorkerPage enrolls an active worker or registers a passive one. */
export function AddWorkerPage({ tab }: { tab: AddWorkerTab }) {
  const navigate = useNavigate();
  return (
    <>
      <PageHeader
        back={{ to: "/workers", label: "Workers" }}
        title="Add a worker"
        description="Install the worker on the machine that holds the files, then connect it to this controller."
      />
      <Tabs
        value={tab}
        onValueChange={(v) =>
          void navigate({ to: "/workers/new", search: { tab: v as AddWorkerTab }, replace: true })
        }
        className="gap-4"
      >
        <TabsList>
          <TabsTrigger value="active" className="px-4">
            Active
          </TabsTrigger>
          <TabsTrigger value="passive" className="px-4">
            Passive
          </TabsTrigger>
        </TabsList>
        <TabsContent value="active">
          <Panel
            title="The worker connects to this controller"
            description="Best for most machines, including ones behind a router or firewall: nothing needs to be opened on the worker's side."
          >
            <ActiveEnrollment />
          </Panel>
        </TabsContent>
        <TabsContent value="passive">
          <Panel
            title="This controller connects to the worker"
            description="For machines that can't reach the controller but accept incoming connections, such as a server in another network."
          >
            <PassiveSetup />
          </Panel>
        </TabsContent>
      </Tabs>
    </>
  );
}
