import { docTarget, type DocTarget } from "@/features/docs/doc-link";

/**
 * channelHelp maps a notification channel ID (ChannelType.id) to the
 * section of the docs that explains its fields. The targets are checked by
 * the typecheck; a channel added later falls back to the page
 * (channelHelpFor).
 */
export const channelHelp: Readonly<Record<string, DocTarget>> = {
  discord: docTarget("notifications", "discord"),
  email: docTarget("notifications", "email"),
  webhook: docTarget("notifications", "webhook"),
};

/** channelHelpFor is the docs section for a channel, or the notifications page for any other. */
export function channelHelpFor(channel: string | undefined): DocTarget {
  return channel !== undefined && Object.hasOwn(channelHelp, channel)
    ? channelHelp[channel]
    : docTarget("notifications");
}
