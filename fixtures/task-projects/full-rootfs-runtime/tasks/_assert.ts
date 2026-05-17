export function assert(condition: unknown, message: string): asserts condition {
  if (!condition) {
    throw new Error(message)
  }
}

export async function readText(path: string): Promise<string> {
  return await Bun.file(path).text()
}

export async function command(args: readonly string[]): Promise<string> {
  const proc = Bun.spawn([...args], { stdout: "pipe", stderr: "pipe" })
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(proc.stdout).text(),
    new Response(proc.stderr).text(),
    proc.exited,
  ])
  assert(exitCode === 0, `${args.join(" ")} exited ${exitCode}: ${stderr}`)
  return stdout
}

export async function assertVisibleFile(path: string, expected: string): Promise<void> {
  await Bun.write(path, expected)
  assert((await readText(path)) === expected, `${path} was not visible during the run`)
}
