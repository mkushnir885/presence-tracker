package i18n

import (
	"encoding/json"
	"fmt"
	"maps"
	"sync"
)

// Catalog holds translations for multiple languages, accumulated from
// one or more JSON namespaces via Add. It is safe for concurrent reads
// and writes.
type Catalog struct {
	mu    sync.RWMutex
	langs map[string]map[string]string
}

// New returns an empty Catalog.
func New() *Catalog {
	return &Catalog{langs: map[string]map[string]string{}}
}

// Add merges a JSON object of key→value pairs into lang. Keys defined
// by an earlier Add for the same lang are overwritten; consumers
// namespace their keys to avoid accidental collisions.
func (c *Catalog) Add(lang string, data []byte) error {
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("i18n: parse %s catalog: %w", lang, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.langs[lang] == nil {
		c.langs[lang] = make(map[string]string, len(m))
	}
	maps.Copy(c.langs[lang], m)
	return nil
}

// Locale returns a Locale bound to lang. The returned Locale holds a
// snapshot of the catalog's table for that language; subsequent Add
// calls do not affect Locales already handed out.
func (c *Catalog) Locale(lang string) Locale {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return Locale{Lang: lang, table: c.langs[lang]}
}

// Locale carries the active language and a translation lookup table.
// The zero Locale is usable: T returns the key itself for every input.
type Locale struct {
	Lang  string
	table map[string]string
}

// T returns the translation of key in this Locale's language. When the
// key is absent (or the Locale is zero) the key itself is returned, so
// missing translations surface as visible strings in the UI.
func (l Locale) T(key string) string {
	if v, ok := l.table[key]; ok {
		return v
	}
	return key
}
