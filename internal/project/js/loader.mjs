const EXTENSIONLESS_RELATIVE_OR_ABSOLUTE = /^(?:\.{1,2}\/|\/|file:)/
const KNOWN_EXTENSION = /\.[cm]?[jt]sx?(?:[?#].*)?$/

export async function resolve(specifier, context, nextResolve) {
  try {
    return await nextResolve(specifier, context)
  } catch (error) {
    if (!shouldTryTypeScriptExtension(specifier, error)) {
      throw error
    }
    for (const candidate of extensionCandidates(specifier)) {
      try {
        return await nextResolve(candidate, context)
      } catch (candidateError) {
        if (!isModuleNotFound(candidateError)) {
          throw candidateError
        }
      }
    }
    throw error
  }
}

function shouldTryTypeScriptExtension(specifier, error) {
  return isRetryableResolutionError(error) &&
    EXTENSIONLESS_RELATIVE_OR_ABSOLUTE.test(specifier) &&
    !KNOWN_EXTENSION.test(specifier)
}

function isModuleNotFound(error) {
  return error && typeof error === "object" && error.code === "ERR_MODULE_NOT_FOUND"
}

function isRetryableResolutionError(error) {
  return error && typeof error === "object" &&
    (error.code === "ERR_MODULE_NOT_FOUND" || error.code === "ERR_UNSUPPORTED_DIR_IMPORT")
}

function extensionCandidates(specifier) {
  return [
    `${specifier}.ts`,
    `${specifier}.mts`,
    `${specifier}.js`,
    `${specifier}.mjs`,
    `${specifier}/index.ts`,
    `${specifier}/index.mts`,
    `${specifier}/index.js`,
    `${specifier}/index.mjs`,
  ]
}
