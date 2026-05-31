package i18n

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
)

type Catalog struct {
	mu    sync.RWMutex
	langs map[string]map[string]string
}

func New() *Catalog {
	return &Catalog{langs: map[string]map[string]string{}}
}

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

func (c *Catalog) Load(component string, sources map[string][]byte) {
	for lang, data := range sources {
		if err := c.Add(lang, data); err != nil {
			slog.Warn(component+": load locale", "lang", lang, "err", err)
		}
	}
}

func (c *Catalog) Locale(lang string) Locale {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return Locale{Lang: lang, table: c.langs[lang]}
}

type Locale struct {
	Lang  string
	table map[string]string
}

func (l Locale) T(key string) string {
	if v, ok := l.table[key]; ok {
		return v
	}
	return key
}

// Subtree returns every key in the locale beginning with prefix.
func (l Locale) Subtree(prefix string) map[string]string {
	out := make(map[string]string)
	for k, v := range l.table {
		if strings.HasPrefix(k, prefix) {
			out[k] = v
		}
	}
	return out
}
