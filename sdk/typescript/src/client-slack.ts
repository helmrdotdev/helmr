export function slackStartOption(value: unknown): Readonly<{ channel_id: string }> {
  if (value === null || typeof value !== "object" || Array.isArray(value) || Object.keys(value).length !== 1 || !("channelId" in value) || typeof value.channelId !== "string") throw new Error("slack must contain only channelId")
  if (!/^[CG][A-Z0-9]{1,99}$/.test(value.channelId)) throw new Error("channelId must be a Slack channel ID")
  return { channel_id: value.channelId }
}
