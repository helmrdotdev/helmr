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
        ids: ["guides/tutorials/first-agent", "guides/tutorials/durable-agent"],
      },
      {
        label: "How-to",
        ids: [
          "guides/how-to/deploy-a-project",
          "guides/how-to/create-a-computer",
          "guides/how-to/start-an-agent",
          "guides/how-to/define-an-agent",
          "guides/how-to/send-input-and-read-output",
          "guides/how-to/inspect-a-turn",
          "guides/how-to/wait-for-human-input",
          "guides/how-to/use-secrets",
          "guides/how-to/build-a-custom-image",
          "guides/how-to/schedule-an-agent",
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
          "concepts/agents",
          "concepts/sessions",
          "concepts/computers",
          "concepts/turns",
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
          "reference/sdk/agents-and-sessions",
          "reference/sdk/computers",
          "reference/sdk/schedules",
          "reference/sdk/questions",
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
          "reference/cli/agent",
          "reference/cli/session",
          "reference/cli/computer",
          "reference/cli/secret",
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
          "reference/session-events",
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
