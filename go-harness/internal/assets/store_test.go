package assets_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/assets"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

type assetFixture struct {
	db      *sqlite.Store
	runtime *hr.Store
	assets  *assets.Store
	session harness.Session
	dir     string
}

func newAssetFixture(t *testing.T) *assetFixture {
	t.Helper()
	root := t.TempDir()
	cwd := filepath.Join(root, "workspace")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	rt, err := hr.NewStore(context.Background(), db.DB())
	if err != nil {
		t.Fatal(err)
	}
	x, err := rt.CreateSession(context.Background(), cwd, "vision")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "assets")
	store, err := assets.NewStore(context.Background(), dir, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return &assetFixture{db: db, runtime: rt, assets: store, session: x, dir: dir}
}
func pngBytes(n int) []byte {
	if n < 32 {
		n = 32
	}
	b := make([]byte, n)
	copy(b, []byte("\x89PNG\r\n\x1a\n"))
	return b
}
func imageInput(n int) harness.Content {
	return harness.Content{Type: "image", MimeType: "image/png", Data: base64.StdEncoding.EncodeToString(pngBytes(n))}
}
func fileURI(path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}
func (f *assetFixture) accept(t *testing.T, input []harness.Content) []harness.Content {
	t.Helper()
	p, err := f.assets.Prepare(context.Background(), f.session, input)
	if err != nil {
		t.Fatal(err)
	}
	err = f.runtime.BeginRun(context.Background(), harness.RunRequest{Session: f.session, RunID: hr.NewID(), InputID: hr.NewID(), Input: p.Input}, p)
	finishErr := p.Finish(err == nil)
	if err != nil || finishErr != nil {
		t.Fatal(errors.Join(err, finishErr))
	}
	return p.Input
}
func (f *assetFixture) scalar(t *testing.T, q string) int {
	t.Helper()
	var n int
	if err := f.db.DB().QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestInputsAreImmutableAuthorizedReferences(t *testing.T) {
	f := newAssetFixture(t)
	ctx := context.Background()
	path := filepath.Join(f.session.CWD, "notes.txt")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	in := f.accept(t, []harness.Content{imageInput(32), {Type: "resource_link", URI: fileURI(path), Name: "notes.txt"}})
	if len(in) != 2 || in[0].Data != "" || in[0].Asset == nil || in[1].Asset == nil {
		t.Fatalf("noncanonical input: %+v", in)
	}
	if err := os.WriteFile(path, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := f.assets.Resolve(ctx, f.session.ID, *in[1].Asset)
	if err != nil || string(data) != "original" {
		t.Fatalf("snapshot=%q err=%v", data, err)
	}
	var saved string
	if err := f.db.DB().QueryRow(`SELECT CAST(content AS TEXT) FROM harness_inputs LIMIT 1`).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(saved, "\"data\"") || !strings.Contains(saved, "deerflow-asset://") {
		t.Fatal("input persisted inline bytes")
	}
	x, err := f.runtime.CreateSession(ctx, f.session.CWD, "vision")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.assets.Resolve(ctx, x.ID, *in[0].Asset); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("cross-session resolve: %v", err)
	}
	forged := *in[0].Asset
	forged.Name = "forged.png"
	if _, err = f.assets.Resolve(ctx, f.session.ID, forged); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("metadata tamper: %v", err)
	}
	forged = *in[0].Asset
	forged.SessionID = x.ID
	if _, err = f.assets.Resolve(ctx, x.ID, forged); err == nil {
		t.Fatal("forged session accepted")
	}
	reused := f.accept(t, []harness.Content{in[0]})
	if *reused[0].Asset != *in[0].Asset || f.scalar(t, `SELECT count(*) FROM harness_assets`) != 2 {
		t.Fatal("reference reuse wrote another snapshot")
	}
	if err := os.WriteFile(filepath.Join(f.dir, in[0].Asset.ID+".blob"), pngBytes(33), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = f.assets.Resolve(ctx, f.session.ID, *in[0].Asset); err == nil {
		t.Fatal("changed snapshot accepted")
	}
}

func TestInputValidationAndRemoteReferencesNeverFetch(t *testing.T) {
	f := newAssetFixture(t)
	ctx := context.Background()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	cases := []harness.Content{
		{Type: "image", MimeType: "image/png", Data: "not-base64"},
		{Type: "image", MimeType: "image/jpeg", Data: imageInput(32).Data},
		{Type: "image", MimeType: "image/png", Data: imageInput(32).Data[:4] + "\n" + imageInput(32).Data[4:]},
		{Type: "image", MimeType: "image/png", URI: server.URL + "/image"},
		{Type: "resource_link", URI: server.URL + "/image.png", Name: "image"},
		{Type: "resource_link", URI: server.URL + "/image.svg", Name: "image"},
		{Type: "resource_link", URI: server.URL + "/image", Name: "image.avif"},
		{Type: "resource_link", URI: server.URL + "/image", MimeType: "image/png", Name: "image"},
		{Type: "text", Text: "text", Data: "hidden"},
		{Type: "resource_link", URI: "deerflow-asset://unknown/id", Name: "forged"},
		{Type: "text", Text: " "},
	}
	for _, c := range cases {
		if p, err := f.assets.Prepare(ctx, f.session, []harness.Content{c}); err == nil {
			p.Finish(false)
			t.Fatalf("accepted %+v", c)
		}
	}
	out := f.accept(t, []harness.Content{{Type: "resource_link", URI: server.URL + "/manual.pdf", Name: "manual.pdf", MimeType: "application/pdf"}})
	if out[0].Asset != nil || out[0].URI != server.URL+"/manual.pdf" || requests.Load() != 0 {
		t.Fatal("remote reference was fetched or snapshotted")
	}
}

func TestImageAndFileSizeLimits(t *testing.T) {
	f := newAssetFixture(t)
	ctx := context.Background()
	tooMany := make([]harness.Content, 9)
	for i := range tooMany {
		tooMany[i] = imageInput(32)
	}
	if p, err := f.assets.Prepare(ctx, f.session, tooMany); err == nil {
		p.Finish(false)
		t.Fatal("nine images accepted")
	}
	twenty := imageInput(int(harness.MaxInputImageBytes))
	p, err := f.assets.Prepare(ctx, f.session, []harness.Content{twenty, twenty})
	if err != nil {
		t.Fatal("40 MiB boundary:", err)
	}
	if err = p.Finish(false); err != nil {
		t.Fatal(err)
	}
	if p, err = f.assets.Prepare(ctx, f.session, []harness.Content{twenty, twenty, imageInput(32)}); err == nil {
		p.Finish(false)
		t.Fatal("over 40 MiB accepted")
	}
	if p, err = f.assets.Prepare(ctx, f.session, []harness.Content{imageInput(int(harness.MaxInputImageBytes + 1))}); err == nil {
		p.Finish(false)
		t.Fatal("over 20 MiB accepted")
	}
	path := filepath.Join(f.session.CWD, "large.bin")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(harness.MaxInputFileBytes); err != nil {
		t.Fatal(err)
	}
	file.Close()
	p, err = f.assets.Prepare(ctx, f.session, []harness.Content{{Type: "resource_link", URI: fileURI(path), Name: "large.bin"}})
	if err != nil {
		t.Fatal("25 MiB boundary:", err)
	}
	p.Finish(false)
	if err = os.Truncate(path, harness.MaxInputFileBytes+1); err != nil {
		t.Fatal(err)
	}
	if p, err = f.assets.Prepare(ctx, f.session, []harness.Content{{Type: "resource_link", URI: fileURI(path), Name: "large.bin"}}); err == nil {
		p.Finish(false)
		t.Fatal("over 25 MiB file accepted")
	}
}

func TestWorkspaceBoundaryAndSymlinkEscape(t *testing.T) {
	f := newAssetFixture(t)
	ctx := context.Background()
	outside := filepath.Join(t.TempDir(), "private.txt")
	if err := os.WriteFile(outside, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if p, err := f.assets.Prepare(ctx, f.session, []harness.Content{{Type: "resource_link", URI: fileURI(outside), Name: "private"}}); err == nil {
		p.Finish(false)
		t.Fatal("outside file accepted")
	}
	link := filepath.Join(f.session.CWD, "escape.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Log("symlink unavailable:", err)
		return
	}
	if p, err := f.assets.Prepare(ctx, f.session, []harness.Content{{Type: "resource_link", URI: fileURI(link), Name: "private"}}); err == nil {
		p.Finish(false)
		t.Fatal("symlink escape accepted")
	}
}

func TestAcceptedInputAssetRowsAndEventRollbackTogether(t *testing.T) {
	f := newAssetFixture(t)
	ctx := context.Background()
	if _, err := f.db.DB().Exec(`CREATE TRIGGER fail_input_event BEFORE INSERT ON harness_events BEGIN SELECT RAISE(ABORT,'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	p, err := f.assets.Prepare(ctx, f.session, []harness.Content{imageInput(32)})
	if err != nil {
		t.Fatal(err)
	}
	ref := *p.Input[0].Asset
	err = f.runtime.BeginRun(ctx, harness.RunRequest{Session: f.session, RunID: hr.NewID(), InputID: hr.NewID(), Input: p.Input}, p)
	if err == nil {
		t.Fatal("trigger did not fail")
	}
	if err = p.Finish(false); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"harness_runs", "harness_inputs", "harness_events", "harness_assets", "harness_input_assets"} {
		if f.scalar(t, `SELECT count(*) FROM `+table) != 0 {
			t.Fatalf("partial commit in %s", table)
		}
	}
	if _, err = os.Stat(filepath.Join(f.dir, ref.ID+".blob")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback snapshot retained: %v", err)
	}
}

func TestCleanupPinsActivePreparationAndRetainsCommittedSnapshots(t *testing.T) {
	f := newAssetFixture(t)
	ctx := context.Background()
	keep := f.accept(t, []harness.Content{imageInput(32)})[0]
	if other, err := assets.NewStore(ctx, f.dir, f.db.DB()); !errors.Is(err, harness.ErrBusy) {
		if other != nil {
			other.Close()
		}
		t.Fatalf("second store lock: %v", err)
	}
	p, err := f.assets.Prepare(ctx, f.session, []harness.Content{imageInput(32)})
	if err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(f.dir, p.Input[0].Asset.ID+".blob")
	cleaned := make(chan error, 1)
	go func() { cleaned <- f.assets.CleanupOrphans(ctx) }()
	select {
	case err := <-cleaned:
		t.Fatalf("GC crossed active preparation: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	// Model a crash after the atomic file rename but before its transaction.
	if err = p.Finish(true); err != nil {
		t.Fatal(err)
	}
	if err = <-cleaned; err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("orphan retained")
	}
	if _, err = f.assets.Resolve(ctx, f.session.ID, *keep.Asset); err != nil {
		t.Fatal("committed snapshot deleted:", err)
	}
	if err = f.assets.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := assets.NewStore(ctx, f.dir, f.db.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err = reopened.Resolve(ctx, f.session.ID, *keep.Asset); err != nil {
		t.Fatal(err)
	}
}

func (f *assetFixture) startTool(t *testing.T, name string) (string, string) {
	t.Helper()
	ctx := context.Background()
	run, call := hr.NewID(), hr.NewID()
	if err := f.runtime.BeginRun(ctx, harness.RunRequest{Session: f.session, RunID: run, InputID: hr.NewID(), Input: []harness.Content{{Type: "text", Text: "run"}}}); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []struct{ kind, status string }{{"tool_start", "pending"}, {"tool_execute", "in_progress"}} {
		if _, err := f.runtime.Append(ctx, harness.RunEvent{SessionID: f.session.ID, RunID: run, ToolCallID: call, ToolName: name, Kind: phase.kind, Status: phase.status, Arguments: json.RawMessage(`{}`)}, f.assets); err != nil {
			t.Fatal(err)
		}
	}
	return run, call
}
func TestArtifactTerminalReceiptRegistryAndAuditAreAtomic(t *testing.T) {
	f := newAssetFixture(t)
	ctx := context.Background()
	dir := filepath.Join(f.session.CWD, ".deerflow", "outputs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "report.txt")
	if err := os.WriteFile(path, []byte("original report"), 0600); err != nil {
		t.Fatal(err)
	}
	run, call := f.startTool(t, "present_files")
	input := []string{".deerflow/outputs/report.txt", "report.txt", "/mnt/user-data/outputs/report.txt", path}
	staged, err := f.assets.StageArtifacts(ctx, f.session, run, call, input)
	if err != nil || len(staged) != 1 {
		t.Fatalf("stage=%v err=%v", staged, err)
	}
	if _, err = f.assets.Resolve(ctx, f.session.ID, *staged[0].Asset); err == nil {
		t.Fatal("uncommitted artifact resolvable")
	}
	if _, err = f.db.DB().Exec(`CREATE TRIGGER fail_artifact_audit BEFORE INSERT ON harness_events WHEN json_extract(NEW.event,'$.kind')='artifact_presented' BEGIN SELECT RAISE(ABORT,'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	end := harness.RunEvent{SessionID: f.session.ID, RunID: run, ToolCallID: call, ToolName: "present_files", Kind: "tool_end", Status: "completed"}
	if _, err = f.runtime.Append(ctx, end, f.assets); err == nil {
		t.Fatal("audit failure ignored")
	}
	if f.scalar(t, `SELECT count(*) FROM harness_artifacts`) != 0 || f.scalar(t, `SELECT count(*) FROM harness_assets`) != 0 || f.scalar(t, `SELECT count(*) FROM harness_tool_receipts WHERE state='completed'`) != 0 {
		t.Fatal("partial artifact commit")
	}
	if _, err = os.Stat(filepath.Join(f.dir, staged[0].Asset.ID+".blob")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed artifact snapshot retained")
	}
	if _, err = f.db.DB().Exec(`DROP TRIGGER fail_artifact_audit`); err != nil {
		t.Fatal(err)
	}
	staged, err = f.assets.StageArtifacts(ctx, f.session, run, call, input)
	if err != nil {
		t.Fatal(err)
	}
	published, err := f.runtime.Append(ctx, end, f.assets)
	if err != nil {
		t.Fatal(err)
	}
	if len(published.Content) != 1 || published.Receipt.State != harness.ReceiptCompleted || f.scalar(t, `SELECT count(*) FROM harness_artifacts`) != 1 || f.scalar(t, `SELECT count(*) FROM harness_events WHERE json_extract(event,'$.kind')='artifact_presented'`) != 1 {
		t.Fatal("terminal registration missing")
	}
	if err = os.WriteFile(path, []byte("replaced"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := f.assets.Resolve(ctx, f.session.ID, *staged[0].Asset)
	if err != nil || !bytes.Equal(data, []byte("original report")) {
		t.Fatalf("artifact lost immutability: %q %v", data, err)
	}
	if _, err = f.runtime.Append(ctx, end, f.assets); !errors.Is(err, harness.ErrReceiptConflict) {
		t.Fatal("terminal event accepted twice")
	}
}

func TestViewImageCommitsMediaWithoutRegisteringArtifactAndCloseDrainsStaged(t *testing.T) {
	f := newAssetFixture(t)
	ctx := context.Background()
	path := filepath.Join(f.session.CWD, "photo.png")
	if err := os.WriteFile(path, pngBytes(32), 0600); err != nil {
		t.Fatal(err)
	}
	run, call := f.startTool(t, "view_image")
	c, err := f.assets.StageImage(ctx, f.session, run, call, "photo.png")
	if err != nil {
		t.Fatal(err)
	}
	end := harness.RunEvent{SessionID: f.session.ID, RunID: run, ToolCallID: call, ToolName: "view_image", Kind: "tool_end", Status: "completed"}
	out, err := f.runtime.Append(ctx, end, f.assets)
	if err != nil {
		t.Fatal(err)
	}
	if out.Content[0].Asset == nil || f.scalar(t, `SELECT count(*) FROM harness_artifacts`) != 0 || f.scalar(t, `SELECT count(*) FROM harness_events WHERE json_extract(event,'$.kind')='artifact_presented'`) != 0 {
		t.Fatal("view_image incorrectly presented artifact")
	}
	if _, err = f.assets.Resolve(ctx, f.session.ID, *c.Asset); err != nil {
		t.Fatal(err)
	}
	run, call = f.startTool(t, "view_image")
	c, err = f.assets.StageImage(ctx, f.session, run, call, path)
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { closed <- f.assets.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked on abandoned staged media")
	}
	if _, err = os.Stat(filepath.Join(f.dir, c.Asset.ID+".blob")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("abandoned tool snapshot retained")
	}
}

func TestFailedToolTerminalRemovesStagedSnapshot(t *testing.T) {
	f := newAssetFixture(t)
	ctx := context.Background()
	path := filepath.Join(f.session.CWD, "photo.png")
	if err := os.WriteFile(path, pngBytes(32), 0600); err != nil {
		t.Fatal(err)
	}
	run, call := f.startTool(t, "view_image")
	c, err := f.assets.StageImage(ctx, f.session, run, call, path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.runtime.Append(ctx, harness.RunEvent{SessionID: f.session.ID, RunID: run, ToolCallID: call, ToolName: "view_image", Kind: "tool_end", Status: "failed", Text: "cancelled before publication"}, f.assets)
	if err != nil {
		t.Fatal(err)
	}
	if f.scalar(t, `SELECT count(*) FROM harness_assets`) != 0 {
		t.Fatal("failed output registered")
	}
	if _, err = os.Stat(filepath.Join(f.dir, c.Asset.ID+".blob")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed output retained on disk")
	}
}

func TestInlineToolImagesCommitWithReceiptAndRollBackOnEventFailure(t *testing.T) {
	f := newAssetFixture(t)
	ctx := context.Background()
	image := imageInput(64)
	run, call := f.startTool(t, "mcp_image")
	contents, err := f.assets.StageToolImages(ctx, f.session, run, call, []harness.Content{image})
	if err != nil || len(contents) != 1 || contents[0].Asset == nil {
		t.Fatalf("stage: %+v %v", contents, err)
	}
	if _, err := f.assets.Resolve(ctx, f.session.ID, *contents[0].Asset); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("staged image published early: %v", err)
	}
	end := harness.RunEvent{SessionID: f.session.ID, RunID: run, ToolCallID: call, ToolName: "mcp_image", Kind: "tool_end", Status: "completed", Content: []harness.Content{{Type: "text", Text: "caption"}, contents[0]}}
	if _, err := f.db.DB().Exec(`CREATE TRIGGER fail_tool_image_event BEFORE INSERT ON harness_events WHEN json_extract(NEW.event,'$.kind')='tool_end' BEGIN SELECT RAISE(ABORT,'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.runtime.Append(ctx, end, f.assets); err == nil {
		t.Fatal("event failure did not roll back tool image")
	}
	if f.scalar(t, `SELECT count(*) FROM harness_assets`) != 0 {
		t.Fatal("failed transaction published image")
	}
	if _, err := os.Stat(filepath.Join(f.dir, contents[0].Asset.ID+".blob")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed transaction retained blob: %v", err)
	}
	if _, err := f.db.DB().Exec(`DROP TRIGGER fail_tool_image_event`); err != nil {
		t.Fatal(err)
	}
	// The rejected terminal event consumed the staging handle, so a new
	// snapshot is required before retrying the receipt.
	contents, err = f.assets.StageToolImages(ctx, f.session, run, call, []harness.Content{image})
	if err != nil {
		t.Fatal(err)
	}
	end.Content[1] = contents[0]
	got, err := f.runtime.Append(ctx, end, f.assets)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Content) != 2 || got.Content[0].Text != "caption" || got.Receipt == nil || got.Receipt.State != harness.ReceiptCompleted || f.scalar(t, `SELECT count(*) FROM harness_artifacts`) != 0 {
		t.Fatalf("tool result changed or registered artifact: %+v", got)
	}
	data, err := f.assets.Resolve(ctx, f.session.ID, *contents[0].Asset)
	if err != nil || !bytes.Equal(data, pngBytes(64)) {
		t.Fatalf("committed image: %v", err)
	}
	var eventData string
	if err := f.db.DB().QueryRow(`SELECT CAST(event AS TEXT) FROM harness_events WHERE json_extract(event,'$.kind')='tool_end' AND json_extract(event,'$.runId')=?`, run).Scan(&eventData); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(eventData, image.Data) {
		t.Fatal("inline bytes persisted in tool event")
	}
}
