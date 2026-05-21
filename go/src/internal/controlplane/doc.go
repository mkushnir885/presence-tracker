// Package controlplane exposes the JSON HTTP control plane shared by the
// GUI (ptrack serve) and the ptrack poll thin CLI client. The handler is
// mounted on the same loopback HTTP server in both serve and track modes;
// the daemon publishes its listener port to children via the PTRACK_PORT
// environment variable.
package controlplane
