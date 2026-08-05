import { request } from "./api";

export type Task = {
  id: string;
  deployment: {
    id: string;
    version: string;
  };
  queue: string;
  concurrency_limit: number | null;
  ttl: string | null;
  created_at: string;
};

export type ListTasksResponse = {
  tasks: Task[];
};

export async function listTasks(options: {
  projectID: string;
  environmentID: string;
}): Promise<ListTasksResponse> {
  return request<ListTasksResponse>(
    `/api/projects/${encodeURIComponent(options.projectID)}/environments/${encodeURIComponent(options.environmentID)}/tasks`,
  );
}
