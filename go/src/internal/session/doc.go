// Package session contains the [Coordinator] that orchestrates a meeting
// session: it wires together a Provider, a Messenger, a challenge Poller, the
// participant Registry, and the EventStore, and drives the event loop for the
// duration of one meeting.
package session
