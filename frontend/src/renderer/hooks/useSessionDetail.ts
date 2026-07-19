export const sessionDetailQueryKey = (sessionId?: string) =>
	sessionId ? (["session-detail", sessionId] as const) : (["session-detail"] as const);
