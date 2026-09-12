import { useQueryClient } from "@tanstack/solid-query";
import { ApiError } from "../../lib/api";
import { promoteDeployment, type Deployment } from "../../lib/deployments";
import { ConfirmModal } from "../../ui/ConfirmModal";

// The one Promote gate for the list row and the detail header: the caller must
// know the current Deployment before offering to replace it.
export function canPromoteDeployment(options: { permitted: boolean; currentLoaded: boolean; isCurrent: boolean }): boolean {
  return options.permitted && options.currentLoaded && !options.isCurrent;
}

function promoteErrorMessage(error: unknown): string {
  if (error instanceof ApiError && error.code === "forbidden") return "You do not have permission to promote Deployments.";
  if (error instanceof ApiError) return error.message;
  return "Could not promote this Deployment.";
}

export function PromoteDeploymentModal(props: {
  deployment: Deployment;
  projectID: string;
  environmentID: string;
  environmentName: string | undefined;
  onClose: () => void;
}) {
  const queryClient = useQueryClient();
  return (
    <ConfirmModal
      title="Promote Deployment"
      confirmLabel="Promote"
      busyLabel="Promoting..."
      onClose={props.onClose}
      onConfirm={async () => {
        await promoteDeployment(props.deployment.id, { projectID: props.projectID, environmentID: props.environmentID });
        await queryClient.invalidateQueries({ queryKey: ["deployments"] });
        await queryClient.invalidateQueries({ queryKey: ["schedules"] });
      }}
      errorMessage={promoteErrorMessage}
    >
      <strong>{props.deployment.version}</strong> becomes the current Deployment for{" "}
      <strong>{props.environmentName ?? "this environment"}</strong>. New Runs use its definitions and its schedules take effect.
    </ConfirmModal>
  );
}
