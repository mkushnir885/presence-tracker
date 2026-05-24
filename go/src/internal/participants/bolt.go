package participants

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	bucketParticipants = []byte("participants")  // entryID → participantRecord (JSON)
	bucketNameIndex    = []byte("name_index_v2") // norm(displayName) → entryID
	bucketHandleIndex  = []byte("handle_index")  // norm(messenger:handle) → []entryID (JSON)
)

type participantRecord struct {
	ID             string
	DisplayName    string // canonical casing as registered
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
		for _, name := range [][]byte{bucketParticipants, bucketNameIndex, bucketHandleIndex} {
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

func (r *BoltRegistry) Resolve(displayName string) (ParticipantID, bool) {
	key := normName(displayName)
	var pid string
	_ = r.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketNameIndex).Get([]byte(key))
		if v != nil {
			pid = string(v)
		}
		return nil
	})
	if pid == "" {
		return "", false
	}
	return ParticipantID(pid), true
}

func (r *BoltRegistry) Register(_ context.Context, messengerName string, handle Handle, messengerLabel, displayName string) (ParticipantID, error) {
	nameKey := []byte(normName(displayName))
	hKey := []byte(handleKey(messengerName, handle))
	var pid ParticipantID

	err := r.db.Update(func(tx *bolt.Tx) error {
		nameBucket := tx.Bucket(bucketNameIndex)
		partBucket := tx.Bucket(bucketParticipants)
		handleBucket := tx.Bucket(bucketHandleIndex)

		if existing := nameBucket.Get(nameKey); existing != nil {
			raw := partBucket.Get(existing)
			var rec participantRecord
			if raw != nil {
				_ = json.Unmarshal(raw, &rec)
			}
			if rec.Handle != string(handle) || rec.MessengerName != messengerName {
				return ErrNameTaken
			}
			// Same handle overwriting its own entry: refresh label and canonical casing.
			rec.DisplayName = displayName
			rec.MessengerLabel = messengerLabel
			data, err := json.Marshal(rec)
			if err != nil {
				return fmt.Errorf("participants: marshal record: %w", err)
			}
			if err := partBucket.Put(existing, data); err != nil {
				return err
			}
			pid = ParticipantID(rec.ID)
			return nil
		}

		ids, err := readHandleIDs(handleBucket, hKey)
		if err != nil {
			return err
		}
		if len(ids) >= MaxNamesPerHandle {
			return ErrTooManyNames
		}

		rec := participantRecord{
			ID:             newID(),
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
		pidBytes := []byte(rec.ID)
		if err := partBucket.Put(pidBytes, data); err != nil {
			return err
		}
		if err := nameBucket.Put(nameKey, pidBytes); err != nil {
			return err
		}
		ids = append(ids, rec.ID)
		if err := writeHandleIDs(handleBucket, hKey, ids); err != nil {
			return err
		}
		pid = ParticipantID(rec.ID)
		return nil
	})
	return pid, err
}

func (r *BoltRegistry) Unregister(_ context.Context, id ParticipantID) error {
	return r.db.Update(func(tx *bolt.Tx) error {
		return removeByID(tx, string(id))
	})
}

func (r *BoltRegistry) UnregisterByName(_ context.Context, messengerName string, handle Handle, displayName string) (ParticipantID, bool, error) {
	var (
		removed ParticipantID
		found   bool
	)
	err := r.db.Update(func(tx *bolt.Tx) error {
		idBytes := tx.Bucket(bucketNameIndex).Get([]byte(normName(displayName)))
		if idBytes == nil {
			return nil
		}
		raw := tx.Bucket(bucketParticipants).Get(idBytes)
		if raw == nil {
			return nil
		}
		var rec participantRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return fmt.Errorf("participants: decode record: %w", err)
		}
		if rec.MessengerName != messengerName || rec.Handle != string(handle) {
			return nil // owned by someone else; not a match
		}
		if err := removeByID(tx, rec.ID); err != nil {
			return err
		}
		removed = ParticipantID(rec.ID)
		found = true
		return nil
	})
	return removed, found, err
}

func removeByID(tx *bolt.Tx, id string) error {
	pidBytes := []byte(id)
	raw := tx.Bucket(bucketParticipants).Get(pidBytes)
	if raw == nil {
		return nil
	}
	var rec participantRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return fmt.Errorf("participants: decode record: %w", err)
	}
	if err := tx.Bucket(bucketNameIndex).Delete([]byte(normName(rec.DisplayName))); err != nil {
		return err
	}
	handleBucket := tx.Bucket(bucketHandleIndex)
	hKey := []byte(handleKey(rec.MessengerName, Handle(rec.Handle)))
	ids, err := readHandleIDs(handleBucket, hKey)
	if err != nil {
		return err
	}
	ids = slices.DeleteFunc(ids, func(s string) bool { return s == id })
	if len(ids) == 0 {
		if err := handleBucket.Delete(hKey); err != nil {
			return err
		}
	} else if err := writeHandleIDs(handleBucket, hKey, ids); err != nil {
		return err
	}
	return tx.Bucket(bucketParticipants).Delete(pidBytes)
}

func (r *BoltRegistry) Handle(p ParticipantID, messengerName string) (Handle, bool) {
	var h Handle
	_ = r.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketParticipants).Get([]byte(p))
		if raw == nil {
			return nil
		}
		var rec participantRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil
		}
		if rec.MessengerName == messengerName {
			h = Handle(rec.Handle)
		}
		return nil
	})
	return h, h != ""
}

func (r *BoltRegistry) ListByHandle(messengerName string, handle Handle) ([]RegistryEntry, error) {
	hKey := []byte(handleKey(messengerName, handle))
	var out []RegistryEntry
	err := r.db.View(func(tx *bolt.Tx) error {
		ids, err := readHandleIDs(tx.Bucket(bucketHandleIndex), hKey)
		if err != nil {
			return err
		}
		partBucket := tx.Bucket(bucketParticipants)
		for _, id := range ids {
			raw := partBucket.Get([]byte(id))
			if raw == nil {
				continue
			}
			var rec participantRecord
			if err := json.Unmarshal(raw, &rec); err != nil {
				return err
			}
			out = append(out, entryFromRecord(rec))
		}
		return nil
	})
	return out, err
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
		for _, name := range [][]byte{bucketParticipants, bucketNameIndex, bucketHandleIndex} {
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
		ID:             ParticipantID(rec.ID),
		DisplayName:    rec.DisplayName,
		MessengerName:  rec.MessengerName,
		Handle:         Handle(rec.Handle),
		MessengerLabel: rec.MessengerLabel,
		RegisteredAt:   rec.RegisteredAt,
	}
}

func readHandleIDs(b *bolt.Bucket, key []byte) ([]string, error) {
	raw := b.Get(key)
	if raw == nil {
		return nil, nil
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil {
		return nil, fmt.Errorf("participants: decode handle index: %w", err)
	}
	return ids, nil
}

func writeHandleIDs(b *bolt.Bucket, key []byte, ids []string) error {
	data, err := json.Marshal(ids)
	if err != nil {
		return fmt.Errorf("participants: encode handle index: %w", err)
	}
	return b.Put(key, data)
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
