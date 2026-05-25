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

var (
	bucketParticipants = []byte("participants_v3") // norm(displayName) → participantRecord (JSON)
	bucketHandleIndex  = []byte("handle_index_v3") // norm(messenger:handle) → norm(displayName)
)

type participantRecord struct {
	DisplayName    string
	MessengerName  string
	Handle         string
	MessengerLabel string
	RegisteredAt   time.Time
}

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
		for _, name := range [][]byte{bucketParticipants, bucketHandleIndex} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("participants: init buckets: %w", err)
	}
	return &BoltRegistry{db: db}, nil
}

func (r *BoltRegistry) Close() error { return r.db.Close() }

func normName(displayName string) string {
	return strings.ToLower(strings.TrimSpace(displayName))
}

func handleKey(messengerName string, handle Handle) string {
	return strings.ToLower(messengerName) + ":" + string(handle)
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
		var rec participantRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil
		}
		entry = entryFromRecord(rec)
		found = true
		return nil
	})
	return entry, found
}

func (r *BoltRegistry) Register(_ context.Context, messengerName string, handle Handle, messengerLabel, displayName string) error {
	nameKey := []byte(normName(displayName))
	hKey := []byte(handleKey(messengerName, handle))

	return r.db.Update(func(tx *bolt.Tx) error {
		partBucket := tx.Bucket(bucketParticipants)
		handleBucket := tx.Bucket(bucketHandleIndex)

		if existing := partBucket.Get(nameKey); existing != nil {
			var rec participantRecord
			_ = json.Unmarshal(existing, &rec)
			if rec.Handle != string(handle) || rec.MessengerName != messengerName {
				return ErrNameTaken
			}
			// Same handle re-registering the same name: refresh label/casing.
			rec.DisplayName = displayName
			rec.MessengerLabel = messengerLabel
			data, err := json.Marshal(rec)
			if err != nil {
				return fmt.Errorf("participants: marshal record: %w", err)
			}
			return partBucket.Put(nameKey, data)
		}

		// New name. If the handle already owns a different registration, drop it.
		if prevName := handleBucket.Get(hKey); prevName != nil {
			if err := partBucket.Delete(prevName); err != nil {
				return err
			}
		}

		rec := participantRecord{
			DisplayName:    displayName,
			MessengerName:  messengerName,
			Handle:         string(handle),
			MessengerLabel: messengerLabel,
			RegisteredAt:   time.Now().UTC(),
		}
		data, err := json.Marshal(rec)
		if err != nil {
			return fmt.Errorf("participants: marshal record: %w", err)
		}
		if err := partBucket.Put(nameKey, data); err != nil {
			return err
		}
		return handleBucket.Put(hKey, nameKey)
	})
}

func (r *BoltRegistry) UnregisterByName(_ context.Context, displayName string) (bool, error) {
	nameKey := []byte(normName(displayName))
	var found bool
	err := r.db.Update(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketParticipants).Get(nameKey)
		if raw == nil {
			return nil
		}
		var rec participantRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return fmt.Errorf("participants: decode record: %w", err)
		}
		if err := tx.Bucket(bucketHandleIndex).Delete([]byte(handleKey(rec.MessengerName, Handle(rec.Handle)))); err != nil {
			return err
		}
		if err := tx.Bucket(bucketParticipants).Delete(nameKey); err != nil {
			return err
		}
		found = true
		return nil
	})
	return found, err
}

func (r *BoltRegistry) UnregisterByHandle(_ context.Context, messengerName string, handle Handle) (bool, error) {
	hKey := []byte(handleKey(messengerName, handle))
	var found bool
	err := r.db.Update(func(tx *bolt.Tx) error {
		nameKey := tx.Bucket(bucketHandleIndex).Get(hKey)
		if nameKey == nil {
			return nil
		}
		if err := tx.Bucket(bucketParticipants).Delete(nameKey); err != nil {
			return err
		}
		if err := tx.Bucket(bucketHandleIndex).Delete(hKey); err != nil {
			return err
		}
		found = true
		return nil
	})
	return found, err
}

func (r *BoltRegistry) HandleForName(displayName, messengerName string) (Handle, bool) {
	entry, ok := r.Resolve(displayName)
	if !ok {
		return "", false
	}
	if entry.MessengerName != messengerName {
		return "", false
	}
	return entry.Handle, true
}

func (r *BoltRegistry) LookupByHandle(messengerName string, handle Handle) (RegistryEntry, bool) {
	var (
		entry RegistryEntry
		found bool
	)
	_ = r.db.View(func(tx *bolt.Tx) error {
		nameKey := tx.Bucket(bucketHandleIndex).Get([]byte(handleKey(messengerName, handle)))
		if nameKey == nil {
			return nil
		}
		raw := tx.Bucket(bucketParticipants).Get(nameKey)
		if raw == nil {
			return nil
		}
		var rec participantRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil
		}
		entry = entryFromRecord(rec)
		found = true
		return nil
	})
	return entry, found
}

func (r *BoltRegistry) List(_ context.Context) ([]RegistryEntry, error) {
	var out []RegistryEntry
	err := r.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketParticipants).ForEach(func(_, v []byte) error {
			var rec participantRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return err
			}
			out = append(out, entryFromRecord(rec))
			return nil
		})
	})
	return out, err
}

func (r *BoltRegistry) ClearAll(_ context.Context) error {
	return r.db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketParticipants, bucketHandleIndex} {
			if err := tx.DeleteBucket(name); err != nil {
				return err
			}
			if _, err := tx.CreateBucket(name); err != nil {
				return err
			}
		}
		return nil
	})
}

func entryFromRecord(rec participantRecord) RegistryEntry {
	return RegistryEntry{
		DisplayName:    rec.DisplayName,
		MessengerName:  rec.MessengerName,
		Handle:         Handle(rec.Handle),
		MessengerLabel: rec.MessengerLabel,
		RegisteredAt:   rec.RegisteredAt,
	}
}
