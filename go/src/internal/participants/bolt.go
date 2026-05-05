package participants

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	bucketParticipants  = []byte("participants")   // entryID → participantRecord (JSON)
	bucketPlatformIndex = []byte("platform_index") // norm(platform:displayName) → entryID
)

type participantRecord struct {
	ID             string
	Platform       string
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
		for _, name := range [][]byte{bucketParticipants, bucketPlatformIndex} {
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

// normKey returns the canonical index key for (platform, displayName).
func normKey(platform, displayName string) string {
	return strings.ToLower(platform) + ":" + strings.ToLower(strings.TrimSpace(displayName))
}

func (r *BoltRegistry) Resolve(platform, displayName string) (ParticipantID, bool) {
	key := normKey(platform, displayName)
	var pid string
	_ = r.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketPlatformIndex).Get([]byte(key))
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

func (r *BoltRegistry) Register(_ context.Context, messengerName string, handle Handle, messengerLabel, platform, displayName string) (ParticipantID, error) {
	key := []byte(normKey(platform, displayName))
	var pid ParticipantID

	err := r.db.Update(func(tx *bolt.Tx) error {
		platBucket := tx.Bucket(bucketPlatformIndex)
		partBucket := tx.Bucket(bucketParticipants)

		existing := platBucket.Get(key)
		if existing != nil {
			raw := partBucket.Get(existing)
			var rec participantRecord
			if raw != nil {
				_ = json.Unmarshal(raw, &rec)
			}
			if rec.Handle != string(handle) || rec.MessengerName != messengerName {
				return ErrNameTaken
			}
			// Same handle overwriting its own entry: update label and canonical casing.
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

		rec := participantRecord{
			ID:             newID(),
			Platform:       platform,
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
		if err := platBucket.Put(key, pidBytes); err != nil {
			return err
		}
		pid = ParticipantID(rec.ID)
		return nil
	})
	return pid, err
}

func (r *BoltRegistry) Unregister(_ context.Context, id ParticipantID) error {
	return r.db.Update(func(tx *bolt.Tx) error {
		pidBytes := []byte(id)
		raw := tx.Bucket(bucketParticipants).Get(pidBytes)
		if raw == nil {
			return nil // already gone; idempotent
		}
		var rec participantRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			return fmt.Errorf("participants: decode record: %w", err)
		}
		key := []byte(normKey(rec.Platform, rec.DisplayName))
		if err := tx.Bucket(bucketPlatformIndex).Delete(key); err != nil {
			return err
		}
		return tx.Bucket(bucketParticipants).Delete(pidBytes)
	})
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

func (r *BoltRegistry) List(_ context.Context) ([]RegistryEntry, error) {
	var out []RegistryEntry
	err := r.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketParticipants).ForEach(func(_, v []byte) error {
			var rec participantRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return err
			}
			out = append(out, RegistryEntry{
				ID:             ParticipantID(rec.ID),
				Platform:       rec.Platform,
				DisplayName:    rec.DisplayName,
				MessengerName:  rec.MessengerName,
				Handle:         Handle(rec.Handle),
				MessengerLabel: rec.MessengerLabel,
				RegisteredAt:   rec.RegisteredAt,
			})
			return nil
		})
	})
	return out, err
}

func (r *BoltRegistry) ClearAll(_ context.Context) error {
	return r.db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketParticipants, bucketPlatformIndex} {
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

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
