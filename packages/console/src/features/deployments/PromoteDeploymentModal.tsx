import { useQueryClient } from "@tanstack/solid-query";
import { onCleanup } from "solid-js";
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
  const controller = new AbortController();
  onCleanup(() => controller.abort());
  return (
    <ConfirmDialog
      title="Promote deployment to current?"
      confirmLabel="Promote"
      busyLabel="Promoting…"
      target={shortVersion(props.deployment.version)}
      environment={environmentChip(props.environment)}
      onClose={props.onClose}
      onConfirm={async () => {
        await promoteDeployment(props.deployment.id, { projectID: props.projectID, environmentID: props.environmentID }, controller.signal);
        await queryClient.invalidateQueries({ queryKey: ["deployments"] });
        await queryClient.invalidateQueries({ queryKey: ["triggers"] });
      }}
      errorMessage={promoteErrorMessage}
    >
      Environment preparation completes before this deployment becomes current. New sessions then use it and its triggers take effect. Open sessions keep their deployment. Agents missing from it stop taking new sessions and Slack conversations.
    </ConfirmDialog>
  );
}
