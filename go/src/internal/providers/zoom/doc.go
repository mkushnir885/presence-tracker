// Package zoom implements the Provider interface for Zoom using the Zoom
// Webhooks API. Participant events arrive via HTTP POST to a local server
// on the configured webhook_port; the teacher must expose this port
// externally (e.g. via port-forwarding or ngrok) and register it in
// their Zoom app's Event Subscriptions configuration.
//
// Authentication uses OAuth 2.0 Authorization Code + PKCE. On first use,
// a browser window opens for the Zoom consent screen; tokens are persisted
// locally in the application data directory.
package zoom
