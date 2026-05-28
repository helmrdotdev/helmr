import { mkdir, writeFile } from "node:fs/promises"

const artifactDir = ".helmr-workflow-artifacts"

export function artifactPath(path: string): string {
  return `${artifactDir}/${path}`
}

export async function writeMarkdown(path: string, value: string): Promise<void> {
  await mkdir(artifactDir, { recursive: true })
  await writeFile(artifactPath(path), value.endsWith("\n") ? value : `${value}\n`)
}

export async function writeJson(path: string, value: unknown): Promise<void> {
  await mkdir(artifactDir, { recursive: true })
  await writeFile(artifactPath(path), `${JSON.stringify(value, null, 2)}\n`)
}
