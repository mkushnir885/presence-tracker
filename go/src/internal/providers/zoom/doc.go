// Package zoom implements [providers.Provider] for Zoom by polling the
// Zoom Dashboard API for live participant data. It requires no public
// address but does require a Zoom Pro plan (or higher) and an OAuth
// authorisation by an account admin — the dashboard_meetings:read:admin
// scope requires admin consent. Tokens are persisted in the application
// data directory.
package zoom
