// Package participants manages the persistent participant registry that
// binds display names to messenger handles. Display names are the
// identity used end to end (in Parquet events and in URLs); the registry
// is the source of truth for "which Telegram account owns this name."
package participants
