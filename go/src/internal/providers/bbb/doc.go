// Package bbb implements [providers.Provider] for BigBlueButton by polling
// the getMeetingInfo API on a fixed interval. It requires no public
// address — only outbound HTTP access to the BBB server — and works with
// every BBB installation, including ones reachable only over the campus
// VPN. Authentication uses the standard BBB shared-secret checksum scheme.
package bbb
