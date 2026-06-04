package participants

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ---- NormalizeName ----

func TestNormalizeName(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"Alice", "Alice"},
		{"  Alice  ", "Alice"},
		{"alice", "alice"}, // case is preserved
		{" ", ""},
		{"", ""},
		{"John Smith", "John Smith"}, // interior spaces kept
	}
	for _, tc := range tests {
		if got := NormalizeName(tc.in); got != tc.want {
			t.Errorf("NormalizeName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---- Filter ----

func TestFilterIsZero(t *testing.T) {
	zero := Filter{}
	if !zero.IsZero() {
		t.Error("zero Filter should be zero")
	}
	withMessenger := Filter{MessengerName: "tg"}
	if withMessenger.IsZero() {
		t.Error("non-zero Filter should not be zero")
	}
	withNames := Filter{DisplayNames: []string{"Alice"}}
	if withNames.IsZero() {
		t.Error("non-zero Filter should not be zero")
	}
}

func TestFilterMatch(t *testing.T) {
	now := time.Now()
	e := RegistryEntry{
		DisplayName:   "Alice Smith",
		MessengerName: "telegram",
		Handle:        "tg123",
		Language:      "en",
		RegisteredAt:  now,
	}

	tests := []struct {
		name   string
		filter Filter
		want   bool
	}{
		{"zero matches all", Filter{}, true},
		{"display name exact", Filter{DisplayNames: []string{"Alice Smith"}}, true},
		{"display name whitespace trimmed", Filter{DisplayNames: []string{"  Alice Smith  "}}, true},
		{"display name wrong", Filter{DisplayNames: []string{"Bob"}}, false},
		{"display name one of many", Filter{DisplayNames: []string{"Bob", "Alice Smith"}}, true},
		{"contains match", Filter{DisplayNameContains: "alice"}, true},
		{"contains case insensitive", Filter{DisplayNameContains: "SMITH"}, true},
		{"contains no match", Filter{DisplayNameContains: "charlie"}, false},
		{"messenger name match", Filter{MessengerName: "telegram"}, true},
		{"messenger name mismatch", Filter{MessengerName: "discord"}, false},
		{"handle match", Filter{Handle: "tg123"}, true},
		{"handle mismatch", Filter{Handle: "tg999"}, false},
		{"registered from inclusive", Filter{RegisteredFrom: now}, true},
		{"registered from future", Filter{RegisteredFrom: now.Add(time.Second)}, false},
		{"registered to exclusive at boundary", Filter{RegisteredTo: now}, false},
		{"registered to future", Filter{RegisteredTo: now.Add(time.Second)}, true},
		{"combined match", Filter{MessengerName: "telegram", Handle: "tg123"}, true},
		{"combined one mismatch", Filter{MessengerName: "telegram", Handle: "tg999"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.filter.Match(e); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// ---- BoltRegistry ----

func openTestRegistry(t *testing.T) *BoltRegistry {
	t.Helper()
	reg, err := OpenBolt(t.TempDir())
	if err != nil {
		t.Fatalf("OpenBolt: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return reg
}

func TestBoltRegistryRegisterAndResolve(t *testing.T) {
	reg := openTestRegistry(t)
	ctx := context.Background()

	if err := reg.Register(ctx, "tg", "u1", "TG", "Alice", "en"); err != nil {
		t.Fatalf("Register: %v", err)
	}

	entry, ok := reg.Resolve("Alice")
	if !ok {
		t.Fatal("expected to find Alice")
	}
	if entry.DisplayName != "Alice" || entry.Handle != "u1" || entry.Language != "en" {
		t.Errorf("unexpected entry: %+v", entry)
	}
}

func TestBoltRegistryResolveUnknown(t *testing.T) {
	reg := openTestRegistry(t)
	_, ok := reg.Resolve("nobody")
	if ok {
		t.Error("expected not found")
	}
}

func TestBoltRegistryErrNameTaken(t *testing.T) {
	reg := openTestRegistry(t)
	ctx := context.Background()

	if err := reg.Register(ctx, "tg", "u1", "TG", "Alice", "en"); err != nil {
		t.Fatal(err)
	}
	// Different handle tries to claim the same name.
	err := reg.Register(ctx, "tg", "u2", "TG", "Alice", "en")
	if !errors.Is(err, ErrNameTaken) {
		t.Errorf("expected ErrNameTaken, got %v", err)
	}
}

func TestBoltRegistryReRegisterReplacesName(t *testing.T) {
	reg := openTestRegistry(t)
	ctx := context.Background()

	if err := reg.Register(ctx, "tg", "u1", "TG", "Alice", "en"); err != nil {
		t.Fatal(err)
	}
	// Same handle re-registers under a new name.
	if err := reg.Register(ctx, "tg", "u1", "TG", "Bob", "en"); err != nil {
		t.Fatalf("re-register: %v", err)
	}

	_, aliceFound := reg.Resolve("Alice")
	if aliceFound {
		t.Error("Alice should be deregistered after re-register")
	}
	entry, bobFound := reg.Resolve("Bob")
	if !bobFound {
		t.Fatal("Bob should be registered")
	}
	if entry.Handle != "u1" {
		t.Errorf("handle: got %q", entry.Handle)
	}
}

func TestBoltRegistryReRegisterPreservesLanguageWhenEmpty(t *testing.T) {
	reg := openTestRegistry(t)
	ctx := context.Background()

	if err := reg.Register(ctx, "tg", "u1", "TG", "Alice", "uk"); err != nil {
		t.Fatal(err)
	}
	// Re-register with an empty language → carry over the previous language.
	if err := reg.Register(ctx, "tg", "u1", "TG", "AliceNew", ""); err != nil {
		t.Fatal(err)
	}
	entry, _ := reg.Resolve("AliceNew")
	if entry.Language != "uk" {
		t.Errorf("language should be preserved when empty, got %q", entry.Language)
	}
}

func TestBoltRegistryReRegisterUpdatesLanguageWhenProvided(t *testing.T) {
	reg := openTestRegistry(t)
	ctx := context.Background()

	if err := reg.Register(ctx, "tg", "u1", "TG", "Alice", "uk"); err != nil {
		t.Fatal(err)
	}
	// Re-register with a non-empty language → use the new language.
	if err := reg.Register(ctx, "tg", "u1", "TG", "AliceNew", "en"); err != nil {
		t.Fatal(err)
	}
	entry, _ := reg.Resolve("AliceNew")
	if entry.Language != "en" {
		t.Errorf("language should be updated when provided, got %q", entry.Language)
	}
}

func TestBoltRegistrySetLanguage(t *testing.T) {
	reg := openTestRegistry(t)
	ctx := context.Background()

	if err := reg.Register(ctx, "tg", "u1", "TG", "Alice", "en"); err != nil {
		t.Fatal(err)
	}
	ok, err := reg.SetLanguage(ctx, "tg", "u1", "uk")
	if err != nil || !ok {
		t.Fatalf("SetLanguage: ok=%v err=%v", ok, err)
	}
	entry, _ := reg.Resolve("Alice")
	if entry.Language != "uk" {
		t.Errorf("language: got %q, want uk", entry.Language)
	}
}

func TestBoltRegistrySetLanguageUnknown(t *testing.T) {
	reg := openTestRegistry(t)
	ctx := context.Background()
	ok, err := reg.SetLanguage(ctx, "tg", "nobody", "en")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected ok=false for unknown handle")
	}
}

func TestBoltRegistryHandleForName(t *testing.T) {
	reg := openTestRegistry(t)
	ctx := context.Background()

	if err := reg.Register(ctx, "tg", "u1", "TG", "Alice", "en"); err != nil {
		t.Fatal(err)
	}
	handle, ok := reg.HandleForName("Alice", "tg")
	if !ok || handle != "u1" {
		t.Errorf("HandleForName: ok=%v handle=%q", ok, handle)
	}
	_, ok = reg.HandleForName("Alice", "discord")
	if ok {
		t.Error("HandleForName should miss on wrong messenger")
	}
}

func TestBoltRegistryLookupByHandle(t *testing.T) {
	reg := openTestRegistry(t)
	ctx := context.Background()

	if err := reg.Register(ctx, "tg", "u1", "TG", "Alice", "en"); err != nil {
		t.Fatal(err)
	}
	entry, ok := reg.LookupByHandle("tg", "u1")
	if !ok || entry.DisplayName != "Alice" {
		t.Errorf("LookupByHandle: ok=%v name=%q", ok, entry.DisplayName)
	}
	_, ok = reg.LookupByHandle("tg", "unknown")
	if ok {
		t.Error("LookupByHandle should miss for unknown handle")
	}
}

func TestBoltRegistryFind(t *testing.T) {
	reg := openTestRegistry(t)
	ctx := context.Background()

	if err := reg.Register(ctx, "tg", "u1", "TG", "Alice", "en"); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(ctx, "tg", "u2", "TG", "Bob", "uk"); err != nil {
		t.Fatal(err)
	}

	all, err := reg.Find(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("Find all: got %d, want 2", len(all))
	}

	filtered, err := reg.Find(ctx, Filter{Handle: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].DisplayName != "Alice" {
		t.Errorf("Find filtered: %+v", filtered)
	}
}

func TestBoltRegistryDeleteZeroFilterClearsAll(t *testing.T) {
	reg := openTestRegistry(t)
	ctx := context.Background()

	if err := reg.Register(ctx, "tg", "u1", "TG", "Alice", "en"); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(ctx, "tg", "u2", "TG", "Bob", "en"); err != nil {
		t.Fatal(err)
	}

	n, err := reg.Delete(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("Delete all: deleted %d, want 2", n)
	}

	all, _ := reg.Find(ctx, Filter{})
	if len(all) != 0 {
		t.Errorf("expected empty registry, got %d entries", len(all))
	}
}

func TestBoltRegistryDeleteByFilter(t *testing.T) {
	reg := openTestRegistry(t)
	ctx := context.Background()

	if err := reg.Register(ctx, "tg", "u1", "TG", "Alice", "en"); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(ctx, "tg", "u2", "TG", "Bob", "en"); err != nil {
		t.Fatal(err)
	}

	n, err := reg.Delete(ctx, Filter{Handle: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("Delete filtered: deleted %d, want 1", n)
	}

	_, aliceFound := reg.Resolve("Alice")
	if aliceFound {
		t.Error("Alice should be deleted")
	}
	_, bobFound := reg.Resolve("Bob")
	if !bobFound {
		t.Error("Bob should still be present")
	}
}
