export type DocsNavGroup = {
  label?: string;
  ids: readonly string[];
};

export type DocsNavSection = {
  label: string;
  groups: readonly DocsNavGroup[];
};

export const docsNav = [
  {
    label: "Start",
    groups: [{ ids: ["quickstart"] }],
  },
  {
    label: "Guides",
    groups: [
      {
        label: "Tutorials",
        ids: ["guides/tutorials/first-task", "guides/tutorials/durable-agent"],
      },
      {
        label: "How-to",
        ids: [
          "guides/how-to/deploy-a-project",
          "guides/how-to/create-a-workspace",
          "guides/how-to/start-a-task",
          "guides/how-to/build-an-actor",
          "guides/how-to/send-input-and-read-output",
          "guides/how-to/inspect-a-run",
          "guides/how-to/wait-for-human-input",
          "guides/how-to/use-secrets",
          "guides/how-to/build-a-custom-image",
          "guides/how-to/schedule-a-task",
        ],
      },
    ],
  },
  {
    label: "Concepts",
    groups: [
      {
        ids: [
          "concepts/how-helmr-works",
          "concepts/tasks",
          "concepts/actors",
          "concepts/workspaces",
          "concepts/runs",
          "concepts/waits-and-input",
          "concepts/deployments-and-environments",
          "concepts/schedules",
          "concepts/secrets",
          "concepts/security",
        ],
      },
    ],
  },
  {
    label: "Reference",
    groups: [
      {
        label: "SDK",
        ids: [
          "reference/sdk/overview",
          "reference/sdk/tasks",
          "reference/sdk/actors-and-sessions",
          "reference/sdk/sandboxes-and-workspaces",
          "reference/sdk/schedules",
          "reference/sdk/tokens",
          "reference/sdk/timers",
          "reference/sdk/logger-and-metadata",
          "reference/sdk/helmr-client",
        ],
      },
      {
        label: "CLI",
        ids: [
          "reference/cli/overview",
          "reference/cli/deploy",
          "reference/cli/project",
          "reference/cli/deployment",
          "reference/cli/task",
          "reference/cli/actor",
          "reference/cli/workspace",
          "reference/cli/run",
          "reference/cli/secret",
          "reference/cli/token",
          "reference/cli/schedule",
        ],
      },
      {
        label: "REST API",
        ids: [
          "reference/rest-api/overview",
          "reference/rest-api/authentication",
          "reference/rest-api/errors-and-idempotency",
          "reference/rest-api/pagination",
        ],
      },
      {
        ids: [
          "reference/configuration",
          "reference/environment-variables",
          "reference/run-events",
        ],
      },
    ],
  },
  {
    label: "Self-hosting",
    groups: [
      {
        ids: [
          "self-hosting/overview",
          "self-hosting/requirements",
          "self-hosting/aws-evaluation",
          "self-hosting/aws-production",
          "self-hosting/control-plane",
          "self-hosting/workers",
          "self-hosting/capacity-scaling",
          "self-hosting/authentication",
          "self-hosting/secrets-and-data",
          "self-hosting/upgrades",
          "self-hosting/troubleshooting",
        ],
      },
    ],
  },
] as const satisfies readonly DocsNavSection[];
