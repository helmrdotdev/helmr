import type { IncomingMessage, ServerResponse } from "node:http";
import type { Plugin } from "vite";

// Dev server only: states a local stack cannot reach on its own, for reviewing
// the Console in this browser. Each link sets a cookie and returns to `next`.
//
// - `/__dev/delay?ms=1500` slows every API response (`ms=0` off).
// - `/__dev/fail?match=sessions&status=500` answers API requests whose path
//   contains `match` with that error (`/__dev/fail` off).
// - `/__dev/device` starts a CLI sign-in request and opens its approval page.
// - `/__dev/slack?state=connected` answers Slack reads as if Slack were set up:
//   every Agent's connection in that state (`none`, `setup`, `install`,
//   `connected`, `reauth`, `disconnected`, `unconfigured`), a linked identity,
//   and delivery issues on every Session. `&role=developer` views it as a
//   member who does not manage Slack. `/__dev/slack` off.
// - `/__dev/setup?deployment=none&secret=missing` shows the Overview's setup
//   panel: `deployment` is `none` (no current deployment), `empty` (one without
//   agents) or `agents`; `secret` is `missing`, `active` or `revoked` for every
//   provider key, and saving a key makes it active. With `&role=developer` the
//   viewer cannot manage secrets. `/__dev/setup` off.
// - `/__dev/go?delay=1500&fail=500:sessions&slack=connected&setup=none:missing&role=developer&to=/sessions?…`
//   sets exactly these states (anything omitted is turned off) and opens `to`,
//   which comes last and is taken as written, query string included.
export function devStates(backendURL: string): Plugin {
  return {
    name: "helmr-dev-states",
    apply: "serve",
    configureServer(server) {
      server.middlewares.use((req, res, next) => {
        const url = new URL(req.url ?? "/", "http://dev");
        const back = url.searchParams.get("next");
        if (url.pathname === "/__dev/delay") {
          const ms = Math.min(60_000, Math.max(0, Number.parseInt(url.searchParams.get("ms") ?? "0", 10) || 0));
          redirect(res, back, [setCookie("helmr_dev_delay", ms === 0 ? "" : String(ms))]);
          return;
        }
        if (url.pathname === "/__dev/fail") {
          const match = url.searchParams.get("match") ?? "";
          redirect(res, back, [setCookie("helmr_dev_fail", match === "" ? "" : `${url.searchParams.get("status") ?? "500"}:${match}`)]);
          return;
        }
        if (url.pathname === "/__dev/slack") {
          redirect(res, back, [setCookie("helmr_dev_slack", url.searchParams.get("state") ?? ""), setCookie("helmr_dev_role", url.searchParams.get("role") ?? "")]);
          return;
        }
        if (url.pathname === "/__dev/setup") {
          savedKeys.clear();
          const deployment = url.searchParams.get("deployment") ?? "";
          redirect(res, back, [setCookie("helmr_dev_setup", deployment === "" ? "" : `${deployment}:${url.searchParams.get("secret") ?? "missing"}`), setCookie("helmr_dev_role", url.searchParams.get("role") ?? "")]);
          return;
        }
        if (url.pathname === "/__dev/go") {
          const raw = req.url ?? "";
          const toAt = raw.indexOf("to=");
          const to = toAt >= 0 ? decodeURIComponent(raw.slice(toAt + 3)) : "/";
          savedKeys.clear();
          const ms = Math.min(60_000, Math.max(0, Number.parseInt(url.searchParams.get("delay") ?? "0", 10) || 0));
          redirect(res, to, [
            setCookie("helmr_dev_delay", ms === 0 ? "" : String(ms)),
            setCookie("helmr_dev_fail", url.searchParams.get("fail") ?? ""),
            setCookie("helmr_dev_slack", url.searchParams.get("slack") ?? ""),
            setCookie("helmr_dev_setup", url.searchParams.get("setup") ?? ""),
            setCookie("helmr_dev_role", url.searchParams.get("role") ?? ""),
          ]);
          return;
        }
        if (url.pathname === "/__dev/device") {
          void fetch(`${backendURL}/api/auth/device/start`, { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" })
            .then((response) => response.json() as Promise<{ user_code?: string }>)
            .then((started) => redirect(res, `/auth/device?code=${encodeURIComponent(started.user_code ?? "")}`, []))
            .catch(next);
          return;
        }
        if (!url.pathname.startsWith("/api/")) {
          next();
          return;
        }
        const fail = cookie(req, "helmr_dev_fail");
        const separator = fail.indexOf(":");
        if (separator > 0 && url.pathname.includes(fail.slice(separator + 1))) {
          const status = Number.parseInt(fail.slice(0, separator), 10) || 500;
          const code = status === 403 ? "forbidden" : status === 404 ? "not_found" : "internal_error";
          json(res, status, { error: { code, message: `simulated ${status} for ${url.pathname}` } });
          return;
        }
        const answer = () => {
          if (setupFixture(req, res, url)) return;
          if (slackFixture(req, res, url, backendURL)) return;
          next();
        };
        const ms = Number.parseInt(cookie(req, "helmr_dev_delay") || "0", 10) || 0;
        if (ms > 0) setTimeout(answer, Math.min(ms, 60_000));
        else answer();
      });
    },
  };
}

function cookie(req: IncomingMessage, name: string): string {
  const value = new RegExp(`(?:^|;\\s*)${name}=([^;]*)`).exec(req.headers.cookie ?? "")?.[1] ?? "";
  return decodeURIComponent(value);
}

function setCookie(name: string, value: string): string {
  return `${name}=${encodeURIComponent(value)}; Path=/; SameSite=Lax${value === "" ? "; Max-Age=0" : ""}`;
}

function redirect(res: ServerResponse, next: string | null, cookies: string[]) {
  const target = next ?? "/";
  res.statusCode = 302;
  res.setHeader("Cache-Control", "no-store");
  res.setHeader("Set-Cookie", cookies);
  res.setHeader("Location", /^\/(?![/\\])/.test(target) ? target : "/");
  res.end();
}

function json(res: ServerResponse, status: number, body: unknown) {
  res.statusCode = status;
  res.setHeader("Content-Type", "application/json");
  res.end(JSON.stringify(body));
}

// Reads the backend's answer and changes it before the Console sees it.
function patched(req: IncomingMessage, res: ServerResponse, backendURL: string, change: (body: Record<string, unknown>) => unknown) {
  void fetch(`${backendURL}${req.url}`, { headers: { cookie: req.headers.cookie ?? "" } })
    .then(async (response) => {
      if (!response.ok) {
        json(res, response.status, await response.json().catch(() => ({})));
        return;
      }
      json(res, 200, change(await response.json() as Record<string, unknown>));
    })
    .catch(() => json(res, 502, { error: { code: "bad_gateway", message: "dev fixture could not reach the backend" } }));
}

const now = () => new Date().toISOString();
const ago = (minutes: number) => new Date(Date.now() - minutes * 60_000).toISOString();
const id = (tail: number) => `00000000-0000-7000-8000-${String(tail).padStart(12, "0")}`;

function slackFixture(req: IncomingMessage, res: ServerResponse, url: URL, backendURL: string): boolean {
  const state = cookie(req, "helmr_dev_slack");
  const role = cookie(req, "helmr_dev_role");
  const path = url.pathname;
  if (role && path === "/api/me" && req.method === "GET") {
    // This preview persona omits connection and secret management grants.
    const manages = role === "owner" || role === "admin";
    patched(req, res, backendURL, (body) => ({ ...body, permissions: (body["permissions"] as string[] | undefined ?? []).filter((permission) => manages || !["secrets.create", "secrets.rotate", "secrets.revoke", "agent_connections.configure", "agent_connections.delete"].includes(permission)), permission_scopes: (body["permission_scopes"] as {permissions:string[]}[] | undefined ?? []).map(scope => ({...scope, permissions:scope.permissions.filter(permission => manages || !["secrets.create", "secrets.rotate", "secrets.revoke", "agent_connections.configure", "agent_connections.delete"].includes(permission))})) }));
    return true;
  }
  if (!state) return false;
  const slackRead = /\/agents\/[^/]+\/slack$/.test(path) || path.startsWith("/api/slack/") || path.endsWith("/slack-delivery");
  if (state === "unconfigured" && slackRead) {
    json(res, 503, { error: { code: "service_unavailable", message: "Slack integration is not configured" } });
    return true;
  }
  if (/\/agents\/[^/]+\/slack$/.test(path)) {
    if (req.method !== "GET") {
      json(res, 200, { redirect_url: "/" });
      return true;
    }
    const agent = decodeURIComponent(path.split("/").at(-2) ?? "agent");
    json(res, 200, { connection: connection(state, agent), manifest: manifest(agent), create_app_url: "https://api.slack.com/apps?new_app=1" });
    return true;
  }
  // Writes succeed without changing the fixture; authorizing "returns" here.
  if (/\/agents\/[^/]+\/slack\//.test(path) || (path.startsWith("/api/slack/user-links/") && req.method === "DELETE")) {
    if (path.endsWith("/authorize")) json(res, 200, { redirect_url: "/" });
    else { res.statusCode = 204; res.end(); }
    return true;
  }
  if (path === "/api/slack/user-links" && req.method === "GET") {
    json(res, 200, { links: [{ team_id: "T0DEMO", slack_user_id: "U0DEMO123", linked_at: ago(60 * 24) }] });
    return true;
  }
  if (path === "/api/slack/link-workspaces" && req.method === "GET") {
    json(res, 200, { configured: true, workspaces: [{ installation_id: id(951), team_id: "T0DEMO", workspace_name: "Demo Workspace" }, { installation_id: id(952), team_id: "T0OTHER", workspace_name: "Other Workspace" }] });
    return true;
  }
  if (path.endsWith("/slack-delivery") && req.method === "GET") {
    json(res, 200, { posts: deliveryPosts() });
    return true;
  }
  if (/^\/api\/projects\/[^/]+\/environments\/[^/]+\/sessions\/[^/]+$/.test(path) && req.method === "GET") {
    patched(req, res, backendURL, (body) => ({ ...body, slack_channel_id: "C0DEMO" }));
    return true;
  }
  return false;
}

// Keys saved while the setup fixture is on; choosing a setup state forgets them.
const savedKeys = new Set<string>();

function setupFixture(req: IncomingMessage, res: ServerResponse, url: URL): boolean {
  const setting = cookie(req, "helmr_dev_setup");
  if (!setting) return false;
  const [deployment, secret] = setting.split(":");
  const path = url.pathname;
  const environment = /^(\/api\/projects\/[^/]+\/environments\/[^/]+)\//.exec(path)?.[1];
  if (!environment) return false;
  const rest = path.slice(environment.length);
  if (rest === "/deployments/current" && req.method === "GET") {
    if (deployment === "none") json(res, 404, { error: { code: "no_current_deployment", message: "Environment has no current Deployment" } });
    else json(res, 200, { id: id(961), version: "20261018.1", bundle_digest: "sha256:demo", created_at: ago(30) });
    return true;
  }
  if (rest === `/deployments/${id(961)}/events` && req.method === "GET") {
    json(res, 200, { events: [{ id: "1", kind: "deployment.promoted", message: "Deployment promoted", at: ago(30), attributes: {} }] });
    return true;
  }
  if (rest === "/agents" && req.method === "GET") {
    json(res, 200, { deployment_id: id(961), agents: deployment === "agents" ? [{ agent: "noether", template: "noether" }] : [] });
    return true;
  }
  // Like the server, only people who manage secrets can list them.
  const role = cookie(req, "helmr_dev_role");
  if (rest === "/secrets" && req.method === "GET" && role && role !== "owner" && role !== "admin") {
    json(res, 403, { error: { code: "forbidden", message: "secrets.read is required" } });
    return true;
  }
  if (rest === "/secrets" && req.method === "GET") {
    const keys = ["ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENCODE_GO_API_KEY"];
    const status = (key: string) => (savedKeys.has(`${environment}/${key}`) ? "active" : secret);
    json(res, 200, { secrets: keys.filter((key) => status(key) !== "missing").map((key) => ({ secret: key, status: status(key), created_at: ago(90), ...(status(key) === "revoked" ? { revoked_at: ago(10) } : {}) })) });
    return true;
  }
  const write = /^\/secrets(?:\/([^/]+)\/rotate)?$/.exec(rest);
  if (write && req.method === "POST") {
    let body = "";
    req.on("data", (chunk) => { body += chunk; });
    req.on("end", () => {
      const key = write[1] ? decodeURIComponent(write[1]) : (JSON.parse(body || "{}") as { secret?: string }).secret ?? "";
      savedKeys.add(`${environment}/${key}`);
      json(res, 200, { secret: key, status: "active", created_at: now() });
    });
    return true;
  }
  return false;
}

function connection(state: string, agent: string) {
  if (state === "none") return null;
  const installed = state !== "setup" && state !== "install";
  const credentials = state !== "setup";
  const status = { setup: "setup_incomplete", install: "setup_incomplete", connected: "connected", reauth: "reauthorization_required", disconnected: "disconnected" }[state] ?? "connected";
  return {
    id: "connection-demo", registration_id: "registration-demo", installation_id: installed ? "installation-demo" : null,
    status, direct_messages_authorized: installed, credentials_configured: credentials, client_id: credentials ? "1234567890.0987654321" : null,
    app_id: credentials ? "A0DEMOAPP" : null, team_id: installed ? "T0DEMO" : null, workspace_name: installed ? "Demo Workspace" : null,
    app_name: agent, app_icon_url: null,
  };
}

function manifest(agent: string) {
  return {
    display_information: { name: agent },
    features: { bot_user: { display_name: agent, always_online: true } },
    oauth_config: { scopes: { bot: ["app_mentions:read", "chat:write", "channels:history"] } },
    settings: { event_subscriptions: { request_url: "https://helmr.example/integrations/slack/events", bot_events: ["app_mention", "message.channels"] }, interactivity: { is_enabled: true }, org_deploy_enabled: false, socket_mode_enabled: false },
  };
}

function deliveryPosts() {
  const post = (tail: number, fields: Record<string, unknown>) => ({
    id: id(tail), turn_id: null, sequence: 1, role: "intermediate", continuation_ordinal: 0, status: "uncertain",
    text: "Agent progress that may already be in Slack", payload_expired_at: null, created_at: ago(3), desired_revision: 2,
    confirmed_revision: 0, method: "chat.postMessage", attempt_id: id(795), message_ts: null,
    root_known: true, opening: false, stream_status: "none", disposed_at: null,
    error: "delivery_outcome_unknown", ...fields,
  });
  return [
    post(791, { role: "response", sequence: 2, error: "history_attempt_limit", status: "failed", disposed_at: ago(1), text: "Final answer: 3 duplicate tickets merged." }),
    post(792, { role: "question", sequence: 3, status: "failed", error: "channel_not_found", text: "Which Slack channel should urgent escalations go to?" }),
    post(793, { sequence: 4, status: "failed", disposed_at: ago(2), confirmed_revision: 1, text: "Partial confirmation retained" }),
    post(794, { role: "request_feedback", sequence: 5, status: "suppressed", attempt_id: null, error: null, root_known: false, text: "Acknowledged your request" }),
    post(796, { sequence: 6, status: "failed", attempt_id: null, payload_expired_at: ago(1), text: "" }),
  ];
}
