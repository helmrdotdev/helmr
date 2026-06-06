export const HELMR_API_VERSION = "2026-06-06"
export const HELMR_API_VERSION_HEADER = "Helmr-API-Version"
export const HELMR_SDK_VERSION_HEADER = "Helmr-SDK-Version"

declare const HELMR_SDK_PACKAGE_VERSION: string | undefined

const SOURCE_PACKAGE_VERSION = "0.1.0"

export const HELMR_SDK_VERSION =
  typeof HELMR_SDK_PACKAGE_VERSION === "string" && HELMR_SDK_PACKAGE_VERSION.trim() !== ""
    ? HELMR_SDK_PACKAGE_VERSION
    : SOURCE_PACKAGE_VERSION
