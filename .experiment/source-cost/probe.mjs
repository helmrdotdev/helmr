import { createHash } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";
import { performance } from "node:perf_hooks";
var metrics = { configMs: 0, configCalls: 0, sourceReadMs: 0, sourceReadBytes: 0, emitMs: 0, transforms: 0, keyMs: 0, hits: 0, misses: 0, cacheReadValidateMs: 0, invalidCache: 0, writeMs: 0 };
var records = {};
var mode = "current";
var identity = "";
var outputPath = "";
var hash = (value) => createHash("sha256").update(value).digest("hex");
function sorted(value) {
  if (Array.isArray(value)) return value.map(sorted);
  if (value !== null && typeof value === "object") return Object.fromEntries(Object.keys(value).sort().map((key) => [key, sorted(value[key])]));
  return value;
}
function configure(options) {
  mode = options.mode;
  identity = options.identity;
  outputPath = options.cachePath ?? "";
  if (mode !== "reuse") return;
  const start = performance.now();
  try {
    const bytes = readFileSync(outputPath);
    if (hash(bytes) !== options.cacheDigest) throw Error("cache manifest identity mismatch");
    const value = JSON.parse(bytes.toString());
    if (value.identity !== identity) throw Error("toolchain identity mismatch");
    for (const [key, item] of Object.entries(value.records)) {
      if (typeof item.outputText !== "string" || hash(item.outputText) !== item.outputDigest) throw Error("emit digest mismatch");
      records[key] = item;
    }
  } catch {
    for (const key of Object.keys(records)) delete records[key];
    metrics.invalidCache++;
  }
  metrics.cacheReadValidateMs += performance.now() - start;
}
function readSource(path) {
  const start = performance.now(), source = readFileSync(path, "utf8");
  metrics.sourceReadMs += performance.now() - start;
  metrics.sourceReadBytes += Buffer.byteLength(source);
  return source;
}
function readConfig(operation) {
  const start = performance.now();
  try {
    return operation();
  } finally {
    metrics.configMs += performance.now() - start;
    metrics.configCalls++;
  }
}
function emitProbe(transform, source, options) {
  let key = "";
  if (mode === "record" || mode === "reuse") {
    const start2 = performance.now();
    key = hash(JSON.stringify(sorted({ identity, source, fileName: options.fileName, compilerOptions: options.compilerOptions })));
    metrics.keyMs += performance.now() - start2;
    if (mode === "reuse" && records[key]) {
      metrics.hits++;
      return { outputText: records[key].outputText, diagnostics: [] };
    }
    metrics.misses++;
  }
  const start = performance.now(), result = transform(source, options);
  metrics.emitMs += performance.now() - start;
  metrics.transforms++;
  if (mode === "record" && !result.diagnostics?.some((d) => d.category === 1)) records[key] = { outputText: result.outputText, outputDigest: hash(result.outputText) };
  return result;
}
function finish() {
  if (mode !== "record") return;
  const start = performance.now();
  writeFileSync(outputPath, JSON.stringify({ identity, records }));
  metrics.writeMs += performance.now() - start;
}
export {
  configure,
  emitProbe,
  finish,
  metrics,
  readConfig,
  readSource
};
