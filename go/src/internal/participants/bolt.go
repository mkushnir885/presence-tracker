package participants

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	ErrCodeNotFound = errors.New("participants: pairing code not found or expired")
	ErrCodeExpired  = errors.New("participants: pairing code expired")
)

var (
	bucketParticipants   = []byte("participants")
	bucketPlatformIndex  = []byte("platform_index")  // "<platform>:<id>" → participantID
	bucketPairingCodes   = []byte("pairing_codes")   // code → pairingEntry (JSON)
	bucketMessengerIndex = []byte("messenger_index") // "<messenger>:<handle>" → participantID
)

type pairingEntry struct {
	MessengerName string
	Handle        string
	ExpiresAt     time.Time
}

type participantRecord struct {
	ID        string
	Platforms map[string]string
	Handles   map[string]string // messengerName → handle
}

// BoltRegistry is a BoltDB-backed Registry implementation.
type BoltRegistry struct {
	db        *bolt.DB
	codeAlpha string
	codeTTL   time.Duration
}

// OpenBolt opens (or creates) the participant database at dataDir/participants.db.
func OpenBolt(dataDir string, codeTTL time.Duration) (*BoltRegistry, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("participants: mkdir: %w", err)
	}
	db, err := bolt.Open(filepath.Join(dataDir, "participants.db"), 0o600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("participants: open db: %w", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketParticipants, bucketPlatformIndex, bucketPairingCodes, bucketMessengerIndex} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("participants: init buckets: %w", err)
	}
	return &BoltRegistry{db: db, codeAlpha: "ABCDEFGHJKLMNPQRSTUVWXYZ0123456789", codeTTL: codeTTL}, nil
}

func (r *BoltRegistry) Close() error { return r.db.Close() }

func (r *BoltRegistry) Resolve(platform, platformID string) (ParticipantID, bool) {
	key := platform + ":" + platformID
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

func (r *BoltRegistry) StartPairing(_ context.Context, messengerName string, handle Handle) (string, error) {
	code, err := r.randomCode(4)
	if err != nil {
		return "", fmt.Errorf("participants: generate code: %w", err)
	}
	entry := pairingEntry{
		MessengerName: messengerName,
		Handle:        string(handle),
		ExpiresAt:     time.Now().Add(r.codeTTL),
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return "", fmt.Errorf("participants: marshal pairing entry: %w", err)
	}
	if err := r.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketPairingCodes).Put([]byte(code), data)
	}); err != nil {
		return "", fmt.Errorf("participants: store code: %w", err)
	}
	return code, nil
}

func (r *BoltRegistry) CompletePairing(_ context.Context, platform, platformID, code string) (ParticipantID, error) {
	var pid ParticipantID
	err := r.db.Update(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketPairingCodes).Get([]byte(code))
		if raw == nil {
			return ErrCodeNotFound
		}
		var entry pairingEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			return fmt.Errorf("participants: decode pairing entry: %w", err)
		}
		if time.Now().After(entry.ExpiresAt) {
			_ = tx.Bucket(bucketPairingCodes).Delete([]byte(code))
			return ErrCodeExpired
		}

		platKey := []byte(platform + ":" + platformID)
		existingPID := tx.Bucket(bucketPlatformIndex).Get(platKey)
		var rec participantRecord
		if existingPID != nil {
			if raw := tx.Bucket(bucketParticipants).Get(existingPID); raw != nil {
				_ = json.Unmarshal(raw, &rec)
			}
		} else {
			rec.ID = newID()
			rec.Platforms = map[string]string{}
			rec.Handles = map[string]string{}
		}

		if rec.Platforms == nil {
			rec.Platforms = map[string]string{}
		}
		if rec.Handles == nil {
			rec.Handles = map[string]string{}
		}

		rec.Platforms[platform] = platformID
		rec.Handles[entry.MessengerName] = entry.Handle

		data, _ := json.Marshal(rec)
		pidBytes := []byte(rec.ID)

		if err := tx.Bucket(bucketParticipants).Put(pidBytes, data); err != nil {
			return err
		}
		if err := tx.Bucket(bucketPlatformIndex).Put(platKey, pidBytes); err != nil {
			return err
		}
		messengerKey := []byte(entry.MessengerName + ":" + entry.Handle)
		if err := tx.Bucket(bucketMessengerIndex).Put(messengerKey, pidBytes); err != nil {
			return err
		}
		if err := tx.Bucket(bucketPairingCodes).Delete([]byte(code)); err != nil {
			return err
		}

		pid = ParticipantID(rec.ID)
		return nil
	})
	return pid, err
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
		if v, ok := rec.Handles[messengerName]; ok {
			h = Handle(v)
		}
		return nil
	})
	return h, h != ""
}

func (r *BoltRegistry) All(_ context.Context) ([]Participant, error) {
	var out []Participant
	err := r.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketParticipants).ForEach(func(_, v []byte) error {
			var rec participantRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return err
			}
			handles := make(map[string]Handle, len(rec.Handles))
			for k, v := range rec.Handles {
				handles[k] = Handle(v)
			}
			out = append(out, Participant{
				ID:        ParticipantID(rec.ID),
				Platforms: rec.Platforms,
				Handles:   handles,
			})
			return nil
		})
	})
	return out, err
}

// ClearAll removes all registered participants, platform indexes, messenger
// indexes, and pending pairing codes. This is a destructive operation.
func (r *BoltRegistry) ClearAll(_ context.Context) error {
	return r.db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketParticipants, bucketPlatformIndex, bucketPairingCodes, bucketMessengerIndex} {
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

func (r *BoltRegistry) randomCode(n int) (string, error) {
	b := make([]byte, n)
	for i := range b {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(r.codeAlpha))))
		if err != nil {
			return "", err
		}
		b[i] = r.codeAlpha[idx.Int64()]
	}
	return string(b), nil
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// PurgeExpiredCodes removes all pairing codes that have passed their TTL.
func (r *BoltRegistry) PurgeExpiredCodes() error {
	return r.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketPairingCodes)
		var toDelete [][]byte
		_ = b.ForEach(func(k, v []byte) error {
			var entry pairingEntry
			if err := json.Unmarshal(v, &entry); err != nil || time.Now().After(entry.ExpiresAt) {
				toDelete = append(toDelete, append([]byte{}, k...))
			}
			return nil
		})
		for _, k := range toDelete {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
}
