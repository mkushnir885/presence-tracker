// Package config loads, validates, and persists the JSON configuration.
//
// One file, config.json, stored under configDir() by default,
// holds every tunable plus inline secrets (no separate secrets file).
// The Config type wraps an atomic snapshot of resolved [Values] so
// callers can read fresh values per use without locking; writers
// (Apply, Reload) serialize through a mutex and rewrite the file in
// canonical form with default-equal fields pruned.
package config
