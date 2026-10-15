import { useQueryClient } from "@tanstack/solid-query";
import { errorMessage } from "../../lib/error";
import { promoteDeployment, type Deployment } from "../../lib/deployments";
import type { Environment } from "../../lib/projects";
import { ConfirmDialog } from "../../ui/Dialog";
import { environmentChip } from "../projects/display";
import { shortVersion } from "./navigation";

// The one Promote gate for the list row and the detail header: the caller must
// know the current Deployment before offering to replace it.
export function canPromoteDeployment(options: { permitted: boolean; currentLoaded: boolean; isCurrent: boolean }): boolean {
  return options.permitted && options.currentLoaded && !options.isCurrent;
}

const promoteErrorMessage = (error: unknown) =>
  errorMessage(error, "Could not promote this deployment.", { forbidden: "You do not have permission to promote deployments." });

export function PromoteDeploymentModal(props: {
  deployment: Deployment;
  projectID: string;
  environmentID: string;
  environment: Environment | undefined;
  onClose: () => void;
}) {
  const queryClient = useQueryClient();
  return (
    <ConfirmDialog
      title="Promote deployment to current?"
      confirmLabel="Promote"
      busyLabel="Promoting..."
      target={shortVersion(props.deployment.version)}
      environment={environmentChip(props.environment)}
      onClose={props.onClose}
      onConfirm={async () => {
        await promoteDeployment(props.deployment.id, { projectID: props.projectID, environmentID: props.environmentID });
        await queryClient.invalidateQueries({ queryKey: ["deployments"] });
        await queryClient.invalidateQueries({ queryKey: ["triggers"] });
      }}
      errorMessage={promoteErrorMessage}
    >
      New sessions start on this deployment and its triggers take effect. Running sessions keep their deployment.
    </ConfirmDialog>
  );
}
