type DocsNavGroup = {
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
    label: "Concepts",
    groups: [
      {
        ids: [
          "concepts/how-helmr-works",
          "concepts/agents",
          "concepts/computers",
          "concepts/sessions",
          "concepts/turns",
          "concepts/questions",
          "concepts/durability",
          "concepts/slack",
          "concepts/delegation",
          "concepts/triggers",
          "concepts/secrets",
          "concepts/projects",
          "concepts/security",
        ],
      },
    ],
  },
  {
    label: "Guides",
    groups: [
      {
        ids: [
          "guides/build-a-durable-agent",
          "guides/deploy",
          "guides/prepare-a-computer",
          "guides/run-commands",
          "guides/start-and-observe-a-session",
          "guides/answer-questions",
          "guides/connect-slack",
          "guides/manage-secrets",
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
          "reference/sdk/agent",
          "reference/sdk/computer",
          "reference/sdk/client",
          "reference/sdk/triggers",
          "reference/sdk/questions",
          "reference/sdk/mcp",
        ],
      },
      {
        label: "CLI",
        ids: [
          "reference/cli/overview",
          "reference/cli/init",
          "reference/cli/login",
          "reference/cli/status",
          "reference/cli/build",
          "reference/cli/deploy",
          "reference/cli/project",
          "reference/cli/deployment",
          "reference/cli/agent",
          "reference/cli/template",
          "reference/cli/session",
          "reference/cli/computer",
          "reference/cli/command",
          "reference/cli/secret",
          "reference/cli/trigger",
        ],
      },
      {
        label: "API",
        ids: [
          "reference/api/overview",
          "reference/api/authentication",
          "reference/api/errors",
          "reference/api/pagination",
        ],
      },
      {
        ids: ["reference/config", "reference/env", "reference/events"],
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
          "self-hosting/evaluation",
          "self-hosting/production",
          "self-hosting/control-plane",
          "self-hosting/workers",
          "self-hosting/capacity",
          "self-hosting/authentication",
          "self-hosting/platform-secrets",
          "self-hosting/upgrades",
          "self-hosting/troubleshooting",
        ],
      },
    ],
  },
] as const satisfies readonly DocsNavSection[];
