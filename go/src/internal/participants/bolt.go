package participants

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// normName is the canonical key under which entries are stored. It
// must stay in sync with session.normName, which converts the same
// display names into session map keys.
func normName(displayName string) string {
	return strings.TrimSpace(displayName)
}

// bucketParticipants stores norm(displayName) → RegistryEntry (JSON).
// Lookups by handle scan the bucket; at classroom scale this is cheaper than
// maintaining a secondary index in sync.
var bucketParticipants = []byte("participants")

// BoltRegistry is a BoltDB-backed Registry implementation.
type BoltRegistry struct {
	db *bolt.DB
}

// OpenBolt opens (or creates) the participant database at dataDir/participants.db.
func OpenBolt(dataDir string) (*BoltRegistry, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("participants: mkdir: %w", err)
	}
	db, err := bolt.Open(filepath.Join(dataDir, "participants.db"), 0o600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("participants: open db: %w", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketParticipants)
		return err
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("participants: init bucket: %w", err)
	}
	return &BoltRegistry{db: db}, nil
}

func (r *BoltRegistry) Close() error { return r.db.Close() }

// findByHandle scans the bucket for an entry owned by (messengerName, handle).
// Returns the bucket key (normalized display name), the entry, and whether a
// match was found. Bucket scan is fine at classroom scale.
func findByHandle(b *bolt.Bucket, messengerName, handle string) ([]byte, RegistryEntry, bool) {
	var (
		foundKey   []byte
		foundEntry RegistryEntry
		found      bool
	)
	_ = b.ForEach(func(k, v []byte) error {
		var entry RegistryEntry
		if err := json.Unmarshal(v, &entry); err != nil {
			return nil
		}
		if entry.MessengerName == messengerName && entry.Handle == handle {
			foundKey = append([]byte(nil), k...)
			foundEntry = entry
			found = true
		}
		return nil
	})
	return foundKey, foundEntry, found
}

func (r *BoltRegistry) Resolve(displayName string) (RegistryEntry, bool) {
	var (
		entry RegistryEntry
		found bool
	)
	_ = r.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketParticipants).Get([]byte(normName(displayName)))
		if raw == nil {
			return nil
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil
		}
		found = true
		return nil
	})
	return entry, found
}

func (r *BoltRegistry) Register(_ context.Context, messengerName, handle, messengerLabel, displayName string) error {
	nameKey := []byte(normName(displayName))

	return r.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketParticipants)

		if existing := b.Get(nameKey); existing != nil {
			var entry RegistryEntry
			_ = json.Unmarshal(existing, &entry)
			if entry.Handle != handle || entry.MessengerName != messengerName {
				return ErrNameTaken
			}
			// Same handle re-registering the same name: refresh label/casing.
			entry.DisplayName = displayName
			entry.MessengerLabel = messengerLabel
			return putEntry(b, nameKey, entry)
		}

		// New name. If the handle already owns a different registration, drop it.
		if prevKey, _, ok := findByHandle(b, messengerName, handle); ok {
			if err := b.Delete(prevKey); err != nil {
				return err
			}
		}

		return putEntry(b, nameKey, RegistryEntry{
			DisplayName:    displayName,
			MessengerName:  messengerName,
			Handle:         handle,
			MessengerLabel: messengerLabel,
			RegisteredAt:   time.Now().UTC(),
		})
	})
}

func putEntry(b *bolt.Bucket, key []byte, entry RegistryEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("participants: marshal entry: %w", err)
	}
	return b.Put(key, data)
}

func (r *BoltRegistry) HandleForName(displayName, messengerName string) (string, bool) {
	entry, ok := r.Resolve(displayName)
	if !ok {
		return "", false
	}
	if entry.MessengerName != messengerName {
		return "", false
	}
	return entry.Handle, true
}

func (r *BoltRegistry) LookupByHandle(messengerName, handle string) (RegistryEntry, bool) {
	var (
		entry RegistryEntry
		found bool
	)
	_ = r.db.View(func(tx *bolt.Tx) error {
		_, e, ok := findByHandle(tx.Bucket(bucketParticipants), messengerName, handle)
		if !ok {
			return nil
		}
		entry = e
		found = true
		return nil
	})
	return entry, found
}

func (r *BoltRegistry) Find(_ context.Context, f Filter) ([]RegistryEntry, error) {
	var out []RegistryEntry
	err := r.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketParticipants).ForEach(func(_, v []byte) error {
			var entry RegistryEntry
			if err := json.Unmarshal(v, &entry); err != nil {
				return err
			}
			if f.Match(entry) {
				out = append(out, entry)
			}
			return nil
		})
	})
	return out, err
}

func (r *BoltRegistry) Delete(_ context.Context, f Filter) (int, error) {
	if f.IsZero() {
		// Fast path: empty filter clears every entry. DeleteBucket +
		// CreateBucket avoids walking the index, then we return the
		// pre-clear count for the caller.
		var n int
		err := r.db.Update(func(tx *bolt.Tx) error {
			n = tx.Bucket(bucketParticipants).Stats().KeyN
			if err := tx.DeleteBucket(bucketParticipants); err != nil {
				return err
			}
			_, err := tx.CreateBucket(bucketParticipants)
			return err
		})
		return n, err
	}

	var n int
	err := r.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketParticipants)
		// Bolt forbids deleting keys during iteration, so collect first.
		var keys [][]byte
		_ = b.ForEach(func(k, v []byte) error {
			var entry RegistryEntry
			if err := json.Unmarshal(v, &entry); err != nil {
				return nil
			}
			if f.Match(entry) {
				keys = append(keys, append([]byte(nil), k...))
			}
			return nil
		})
		for _, k := range keys {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		n = len(keys)
		return nil
	})
	return n, err
}
