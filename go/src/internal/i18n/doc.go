// Package i18n provides a small key-based translation resolver shared
// across the GUI and messenger adapters.
//
// A Catalog accumulates translations for multiple languages from one
// or more JSON namespaces (typically one per consumer: GUI, shared
// messenger keys, messenger-specific keys). Callers obtain a Locale
// bound to a language and call Locale.T to resolve keys; missing keys
// fall through to the key itself so untranslated strings are visible
// rather than blank.
package i18n
