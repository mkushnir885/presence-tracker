package mockfixture_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/apache/arrow/go/v17/arrow/array"
	"github.com/apache/arrow/go/v17/arrow/memory"
	arrowfile "github.com/apache/arrow/go/v17/parquet/file"
	"github.com/apache/arrow/go/v17/parquet/pqarrow"

	"presence-tracker/src/internal/challenges"
	"presence-tracker/src/internal/config"
	"presence-tracker/src/internal/eventstore"
	"presence-tracker/src/internal/messengers"
	mockmsngr "presence-tracker/src/internal/messengers/mock"
	"presence-tracker/src/internal/mockfixture"
	"presence-tracker/src/internal/participants"
	mockprov "presence-tracker/src/internal/providers/mock"
	"presence-tracker/src/internal/session"
)

// testDir returns the absolute path of the root test/ directory.
func testDir() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..", "test")
}

// TestScenarios finds every subdirectory of test/ that contains both
// fixture.jsonl and expected.json and runs each as a sub-test.
func TestScenarios(t *testing.T) {
	root := testDir()
	des, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", root, err)
	}

	ran := 0
	for _, de := range des {
		if !de.IsDir() {
			continue
		}
		dir := filepath.Join(root, de.Name())
		if !fileExists(filepath.Join(dir, "fixture.jsonl")) || !fileExists(filepath.Join(dir, "expected.json")) {
			continue
		}
		name := de.Name()
		ran++
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runScenario(t, dir)
		})
	}
	if ran == 0 {
		t.Fatal("no scenario directories found under test/")
	}
}

func runScenario(t *testing.T, dir string) {
	t.Helper()

	fixture, err := mockfixture.Load(filepath.Join(dir, "fixture.jsonl"))
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	// 100× speed: a 200 s fixture becomes ≈2 s of real test time.
	fixture.WithSpeed(100)

	expected := loadExpected(t, filepath.Join(dir, "expected.json"))

	tmp := t.TempDir()
	meetingsDir := filepath.Join(tmp, "meetings")

	cfg, err := config.Load(filepath.Join(tmp, "config.json"))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	// Force test-friendly settings: no eligibility gap between polls,
	// short answer window so orphaned goroutines don't delay the test.
	if err := cfg.Apply(func(v *config.Values) {
		v.MeetingsDir = meetingsDir
		v.Challenges.Defaults.MinGapBetweenChallengesSecs = 0
		v.Challenges.Defaults.AnswerWindowSeconds = 1
	}); err != nil {
		t.Fatalf("cfg.Apply: %v", err)
	}

	registry, err := participants.OpenBolt(tmp)
	if err != nil {
		t.Fatalf("OpenBolt: %v", err)
	}
	defer registry.Close()

	tmpl, err := eventstore.ParseDirTemplate(cfg.Get().MeetingDirFormat)
	if err != nil {
		t.Fatalf("parse dir template: %v", err)
	}
	store, err := eventstore.NewWriter(meetingsDir, "integration-test", tmpl, time.Now())
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	prov := mockprov.New(fixture)
	msngr := mockmsngr.New(fixture, registry)

	coord := session.New(
		session.Config{
			MeetingID:         "integration-test",
			PlatformMeetingID: filepath.Base(dir),
			ProviderName:      prov.Name(),
		},
		cfg, prov, msngr, registry, store,
	)

	// Poll handler: fixture poll entries POST here, mirroring the real daemon.
	srv := httptest.NewServer(pollMux(coord))
	defer srv.Close()
	fixture.SetDaemonAddr(srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	router := messengers.NewRouter(msngr)
	router.SetHandler(coord)
	if err := router.Start(ctx); err != nil {
		t.Fatalf("router.Start: %v", err)
	}

	if err := coord.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("coord.Run: %v", err)
	}

	cancel()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := router.Stop(stopCtx); err != nil {
		t.Logf("router.Stop: %v", err)
	}

	// Locate the single meeting directory written under meetingsDir.
	des, err := os.ReadDir(meetingsDir)
	if err != nil {
		t.Fatalf("ReadDir meetingsDir: %v", err)
	}
	var meetingDir string
	for _, de := range des {
		if de.IsDir() {
			meetingDir = filepath.Join(meetingsDir, de.Name())
			break
		}
	}
	if meetingDir == "" {
		t.Fatal("no meeting directory was created")
	}

	actual := eventCounts(t, meetingDir)
	compareEventCounts(t, expected, actual)
}

// pollMux wires POST /poll to the coordinator, matching the real daemon's
// poll endpoint so the fixture's in-band poll entries reach the pipeline.
func pollMux(coord *session.Coordinator) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /poll", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			AutoSubmitted bool   `json:"auto_submitted"`
			BankContent   string `json:"bank"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		bank, err := challenges.Parse([]byte(req.BankContent))
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		if _, err := coord.RunPollBank(r.Context(), bank, req.AutoSubmitted); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// loadExpected reads expected.json from path and returns its contents as a
// map of event-type → expected count.
func loadExpected(t *testing.T, path string) map[string]int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read expected.json: %v", err)
	}
	var m map[string]int
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse expected.json: %v", err)
	}
	return m
}

// eventCounts reads the event_type column from meetingDir/events.parquet and
// returns a count per event type.
func eventCounts(t *testing.T, meetingDir string) map[string]int {
	t.Helper()
	path := filepath.Join(meetingDir, eventstore.EventsFile)
	pf, err := arrowfile.OpenParquetFile(path, false)
	if err != nil {
		t.Fatalf("open events.parquet: %v", err)
	}
	defer pf.Close()

	reader, err := pqarrow.NewFileReader(pf, pqarrow.ArrowReadProperties{}, memory.NewGoAllocator())
	if err != nil {
		t.Fatalf("parquet reader: %v", err)
	}
	tbl, err := reader.ReadTable(context.Background())
	if err != nil {
		t.Fatalf("read table: %v", err)
	}
	defer tbl.Release()

	counts := make(map[string]int)
	// event_type is column 2 per the canonical schema.
	for _, chunk := range tbl.Column(2).Data().Chunks() {
		arr := chunk.(*array.String)
		for i := range arr.Len() {
			counts[arr.Value(i)]++
		}
	}
	return counts
}

// compareEventCounts reports a test failure for any mismatch between the
// expected and actual event-type counts, in both directions.
func compareEventCounts(t *testing.T, expected, actual map[string]int) {
	t.Helper()
	for evtType, want := range expected {
		if actual[evtType] != want {
			t.Errorf("%-35s want %d, got %d", evtType, want, actual[evtType])
		}
	}
	for evtType, got := range actual {
		if _, ok := expected[evtType]; !ok {
			t.Errorf("%-35s unexpected event type (got %d)", evtType, got)
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
