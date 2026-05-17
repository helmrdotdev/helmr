import { readFile } from "node:fs/promises";
import { join } from "node:path";

export const prerender = true;

export async function GET() {
  const installScript = await readRootInstallScript();

  return new Response(installScript, {
    headers: {
      "Content-Type": "text/x-shellscript; charset=utf-8",
      "Cache-Control": "public, max-age=300",
    },
  });
}

async function readRootInstallScript() {
  const cwd = process.cwd();
  const candidates = [
    join(cwd, "install"),
    join(cwd, "..", "install"),
    join(cwd, "..", "..", "install"),
  ];

  for (const candidate of candidates) {
    try {
      return await readFile(candidate, "utf8");
    } catch (error) {
      if (!(error instanceof Error) || !("code" in error) || error.code !== "ENOENT") {
        throw error;
      }
    }
  }

  throw new Error("root install script not found");
}
