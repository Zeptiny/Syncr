// URL state of the notification pages, kept apart from them for code splitting.

/** NotificationsSearch is the notifications page's URL state. */
export interface NotificationsSearch {
  tab?: "deliveries";
  /** status filters the delivery log: "problems" shows retrying and dead ones. */
  status?: "problems";
}

/** validateNotificationsSearch parses /notifications' search params. */
export function validateNotificationsSearch(s: Record<string, unknown>): NotificationsSearch {
  return {
    tab: s.tab === "deliveries" ? "deliveries" : undefined,
    status: s.status === "problems" ? "problems" : undefined,
  };
}

/** NewContactSearch picks the channel of a new contact. */
export interface NewContactSearch {
  type?: string;
}

/** validateNewContactSearch parses /notifications/new's search params. */
export function validateNewContactSearch(s: Record<string, unknown>): NewContactSearch {
  return { type: typeof s.type === "string" && s.type ? s.type : undefined };
}
