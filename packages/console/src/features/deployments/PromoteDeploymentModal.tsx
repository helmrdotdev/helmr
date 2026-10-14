import { useQueryClient } from "@tanstack/solid-query";
import { errorMessage } from "../../lib/error";
import { promoteDeployment, type Deployment } from "../../lib/deployments";
import { ConfirmDialog } from "../../ui/Dialog";

// The one Promote gate for the list row and the detail header: the caller must
// know the current Deployment before offering to replace it.
export function canPromoteDeployment(options: { permitted: boolean; currentLoaded: boolean; isCurrent: boolean }): boolean {
  return options.permitted && options.currentLoaded && !options.isCurrent;
}

const promoteErrorMessage = (error: unknown) =>
  errorMessage(error, "Could not promote this Deployment.", { forbidden: "You do not have permission to promote Deployments." });

export function PromoteDeploymentModal(props: {
  deployment: Deployment;
  projectID: string;
  environmentID: string;
  environmentName: string | undefined;
  onClose: () => void;
}) {
  const queryClient = useQueryClient();
  return (
    <ConfirmDialog
      cancelLabel="Keep"
      title="Promote Deployment"
      confirmLabel="Promote"
      busyLabel="Promoting..."
      onClose={props.onClose}
      onConfirm={async () => {
        await promoteDeployment(props.deployment.id, { projectID: props.projectID, environmentID: props.environmentID });
        await queryClient.invalidateQueries({ queryKey: ["deployments"] });
        await queryClient.invalidateQueries({ queryKey: ["triggers"] });
      }}
      errorMessage={promoteErrorMessage}
    >
      <strong>{props.deployment.version}</strong> becomes the current Deployment for{" "}
      <strong>{props.environmentName ?? "this environment"}</strong>. New Sessions use its Agents and its triggers take effect. Agents absent from this Deployment stop accepting new independent Sessions and new Slack conversations; their identity and Slack connections remain. Reintroduced Agent IDs retain their identity. Existing Sessions keep their pinned Deployment, and inactive trigger intervals are not replayed.
    </ConfirmDialog>
  );
}
