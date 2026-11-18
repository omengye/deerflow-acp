package memory

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

type stage5TableSnapshot struct {
	Table   string   `json:"table"`
	Columns []string `json:"columns"`
	Rows    int      `json:"rows"`
	SHA256  string   `json:"sha256"`
}

func stage5Identifier(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// Hash only columns that existed before migration. Do not print private rows.
func stage5TableHash(t *testing.T, db *sql.DB, table string, columns []string) stage5TableSnapshot {
	t.Helper()
	if columns == nil {
		rows, err := db.Query("PRAGMA table_info(" + stage5Identifier(table) + ")")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var cid, notNull, pk int
			var name, kind string
			var defaultValue any
			if err = rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &pk); err != nil {
				t.Fatal(err)
			}
			columns = append(columns, name)
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	if len(columns) == 0 {
		t.Fatalf("missing snapshot table %s", table)
	}
	quoted := make([]string, len(columns))
	for i, column := range columns {
		quoted[i] = stage5Identifier(column)
	}
	rows, err := db.Query("SELECT " + strings.Join(quoted, ",") + " FROM " + stage5Identifier(table))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var encodedRows []string
	for rows.Next() {
		values := make([]any, len(columns))
		ptrs := make([]any, len(columns))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err = rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		encodedRows = append(encodedRows, string(encoded))
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(encodedRows)
	encoded, _ := json.Marshal(encodedRows)
	return stage5TableSnapshot{table, columns, len(encodedRows), fmt.Sprintf("%x", sha256.Sum256(encoded))}
}

func stage5SnapshotTables(t *testing.T, db *sql.DB, tables ...string) []stage5TableSnapshot {
	t.Helper()
	var snapshots []stage5TableSnapshot
	for _, table := range tables {
		var exists bool
		if err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)", table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			snapshots = append(snapshots, stage5TableHash(t, db, table, nil))
		}
	}
	return snapshots
}

func stage5AssertSnapshots(t *testing.T, db *sql.DB, before []stage5TableSnapshot) {
	t.Helper()
	for _, old := range before {
		current := stage5TableHash(t, db, old.Table, old.Columns)
		if current.Rows != old.Rows || current.SHA256 != old.SHA256 {
			t.Fatalf("migration changed historical table %s: count %d -> %d, sha %s -> %s", old.Table, old.Rows, current.Rows, old.SHA256, current.SHA256)
		}
	}
}

var stage5HistoricalTables = []string{"memory_scopes", "memory_facts", "memory_fact_revisions", "memory_slots", "memory_operations", "memory_inputs", "memory_operation_sources", "memory_scope_barriers", "memory_extraction_results", "memory_extraction_items", "memory_channel_bindings", "memory_channel_preferences", "memory_vectors", "memory_migrations"}

func TestMemoryStage5MigrationAtomicAndHistoricalJobs(t *testing.T) {
	s, scope := personalStore(t)
	in := change("legacy-operation", "city", "用户最近告知所在地是北京。", 0)
	old, err := s.Mutate(t.Context(), scope, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Create(t.Context(), scope, Candidate{Content: "legacy unslotted", Category: "profile", Confidence: 1, Source: Source{ID: "unslotted", Kind: "operator"}}); err != nil {
		t.Fatal(err)
	}
	stage5Delete(t, s, scope, "city")
	// A legacy partial batch already committed its first operation. Recovery
	// must retain it but cannot replay the unresolved remainder under v2/v5.
	if _, err = s.Mutate(t.Context(), scope, change("legacy-partial-op-0", "name", "用户昵称是旧记录。", 0)); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"queued", "running", "completed", "saved", "partial"} {
		state, result := status, ""
		if status == "saved" || status == "partial" {
			state, result = "queued", `{"facts":[]}`
		}
		if _, err = s.db.Exec(`INSERT INTO memory_jobs(id,run_id,input_id,attempt_id,payload,result,status,attempts,available_at,updated_at) VALUES(?,?,?,?,?,?,?,2,'old','old')`, status, "run-"+status, "input-"+status, "attempt", []byte(`{"version":1}`), result, state); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.db.Exec("DROP TABLE memory_value_identities; DELETE FROM memory_schema_migrations WHERE version=5; CREATE TRIGGER fail_relearn BEFORE INSERT ON memory_schema_migrations WHEN NEW.version=5 BEGIN SELECT RAISE(ABORT,'relearn_migration'); END"); err != nil {
		t.Fatal(err)
	}
	before := stage5SnapshotTables(t, s.db, stage5HistoricalTables...)
	jobs := stage5SnapshotTables(t, s.db, "memory_jobs")
	if _, err = New(t.Context(), s.db); err == nil || !strings.Contains(err.Error(), "relearn_migration") {
		t.Fatalf("missing migration failure: %v", err)
	}
	var n int
	if err = s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='memory_value_identities'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("partial migration: %d %v", n, err)
	}
	stage5AssertSnapshots(t, s.db, before)
	stage5AssertSnapshots(t, s.db, jobs)
	if _, err = s.db.Exec("DROP TRIGGER fail_relearn"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		s, err = New(t.Context(), s.db)
		if err != nil {
			t.Fatal(err)
		}
		stage5AssertSnapshots(t, s.db, before)
		stage5AssertSnapshots(t, s.db, jobs)
	}
	for range 2 {
		if err = s.RecoverJobs(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	stage5AssertSnapshots(t, s.db, before)
	if err = s.db.QueryRow("SELECT count(*) FROM memory_job_dispositions WHERE reason='legacy_unverifiable' AND usage_state='legacy_usage_unavailable'").Scan(&n); err != nil || n != 4 {
		t.Fatalf("legacy jobs lack honest terminal dispositions: %d %v", n, err)
	}
	if err = s.db.QueryRow("SELECT count(*) FROM memory_jobs WHERE id='completed' AND status='completed'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("completed legacy job changed: %d %v", n, err)
	}
	again, err := s.Mutate(t.Context(), scope, in)
	if err != nil || old != again {
		t.Fatalf("historical receipt replay changed: %+v %+v %v", old, again, err)
	}
	if err = s.db.QueryRow("SELECT count(*) FROM memory_value_identities").Scan(&n); err != nil || n != 0 {
		t.Fatalf("legacy identity was guessed: %d %v", n, err)
	}
}

func stage5RunSchema4(t *testing.T, probe, path string, refused bool) {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), probe, path).CombinedOutput()
	if refused {
		exit, ok := err.(*exec.ExitError)
		if !ok || exit.ExitCode() != 2 || !strings.Contains(string(out), "unsupported memory schema version 5") {
			t.Fatalf("schema4 must refuse v5: %v %s", err, out)
		}
	} else if err != nil || !strings.Contains(string(out), "schema4_opened") {
		t.Fatalf("schema4 baseline could not open v4: %v %s", err, out)
	}
}

func TestMemoryStage5Schema4ReaderRefusesDowngrade(t *testing.T) {
	probe := os.Getenv("MEMORY_STAGE5_SCHEMA4_PROBE")
	if probe == "" {
		t.Skip("requires the schema4 probe built from the pre-stage5 workspace")
	}
	path := filepath.Join(t.TempDir(), "rollback.db")
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := New(t.Context(), db.DB())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("DROP TABLE memory_value_identities; DELETE FROM memory_schema_migrations WHERE version=5"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	stage5RunSchema4(t, probe, path, false)
	db, err = sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err = New(t.Context(), db.DB())
	if err != nil {
		t.Fatal(err)
	}
	scope, err := NewScope(GlobalScope, "", "", "rehearsal-profile", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Mutate(t.Context(), scope, change("after-upgrade", "name", "post-upgrade data must survive", 0)); err != nil {
		t.Fatal(err)
	}
	before := stage5SnapshotTables(t, s.db, append(stage5HistoricalTables, "memory_schema_migrations", "memory_value_identities", "memory_jobs")...)
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	stage5RunSchema4(t, probe, path, true)
	db, err = sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	stage5AssertSnapshots(t, db.DB(), before)
}

func TestMemoryStage5InstalledCopyMigration(t *testing.T) {
	path := os.Getenv("MEMORY_STAGE5_DATABASE_COPY")
	if path == "" {
		t.Skip("requires an offline consistent database copy; never opens the supplied path for writing")
	}
	backup, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(t.TempDir(), "rehearsal.db")
	if err = os.WriteFile(copyPath, backup, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	before := stage5SnapshotTables(t, db.DB(), append(stage5HistoricalTables, "memory_jobs")...)
	var hasIdentities bool
	if err = db.DB().QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='memory_value_identities')").Scan(&hasIdentities); err != nil {
		t.Fatal(err)
	}
	expectedIdentities := 0
	if hasIdentities {
		identitySnapshot := stage5TableHash(t, db.DB(), "memory_value_identities", nil)
		expectedIdentities = identitySnapshot.Rows
		before = append(before, identitySnapshot)
	}
	for range 2 {
		if _, err = New(t.Context(), db.DB()); err != nil {
			t.Fatal(err)
		}
		stage5AssertSnapshots(t, db.DB(), before)
	}
	s, err := New(t.Context(), db.DB())
	if err != nil {
		t.Fatal(err)
	}
	immutable := stage5SnapshotTables(t, db.DB(), "memory_facts", "memory_fact_revisions", "memory_operations", "memory_slots")
	// B1 may repair a decision, but never the accepted source manifest.
	immutable = append(immutable, stage5TableHash(t, db.DB(), "memory_inputs", []string{"input_id", "run_id", "session_id", "manifest"}))
	if err = s.RecoverJobs(t.Context()); err != nil {
		t.Fatal(err)
	}
	stage5AssertSnapshots(t, db.DB(), immutable)
	var version, identities int
	if err = db.DB().QueryRow("SELECT max(version) FROM memory_schema_migrations").Scan(&version); err != nil || version != 5 {
		t.Fatalf("migration version: %d %v", version, err)
	}
	if err = db.DB().QueryRow("SELECT count(*) FROM memory_value_identities").Scan(&identities); err != nil || identities != expectedIdentities {
		t.Fatalf("historical values reclassified: %d %v", identities, err)
	}
	if probe := os.Getenv("MEMORY_STAGE5_SCHEMA4_PROBE"); probe != "" {
		after := stage5SnapshotTables(t, db.DB(), append(stage5HistoricalTables, "memory_jobs", "memory_schema_migrations")...)
		if err = db.Close(); err != nil {
			t.Fatal(err)
		}
		stage5RunSchema4(t, probe, copyPath, true)
		db, err = sqlite.Open(copyPath)
		if err != nil {
			t.Fatal(err)
		}
		stage5AssertSnapshots(t, db.DB(), after)
	}
	encoded, _ := json.Marshal(before)
	t.Logf("installed_copy_sha256=%x; schema=%d; old_column_snapshots=%s", sha256.Sum256(backup), version, encoded)
}
