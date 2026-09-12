export function runHref(runID: string, projectID: string, environmentID: string): string {
  const params = new URLSearchParams({
    project_id: projectID,
    environment_id: environmentID,
  });
  return `/runs/${runID}?${params.toString()}`;
}
