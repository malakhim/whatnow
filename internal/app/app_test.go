package app_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/jameynakama/whatnow/internal/app"
	"github.com/jameynakama/whatnow/internal/domain"
	"github.com/jameynakama/whatnow/internal/integrations"
	"github.com/jameynakama/whatnow/internal/llm"
	"github.com/jameynakama/whatnow/internal/llm/fake"
	"github.com/jameynakama/whatnow/internal/service"
	"github.com/jameynakama/whatnow/internal/settings"
	"github.com/jameynakama/whatnow/internal/store"
)

func fakeClient(context.Context, settings.Clients, settings.Settings) (llm.Client, settings.ProviderConfig, error) {
	return fake.Client{}, settings.ProviderConfig{}, nil
}

func open(t *testing.T, path string) *app.App {
	t.Helper()
	a, err := app.Open(app.Options{DBPath: path, NewClient: fakeClient, Now: time.Now, Loc: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

func TestOpenPutsTheDatabaseInTheUserConfigDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	a := open(t, "")
	want := filepath.Join(dir, "whatnow", "whatnow.db")
	if a.DBPath != want {
		t.Errorf("DBPath = %q, want %q", a.DBPath, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Error(err)
	}
}

func TestOpenCleansUpCapturesInTheBackground(t *testing.T) {
	a := open(t, filepath.Join(t.TempDir(), "test.db"))
	ctx := context.Background()
	applied, stop := a.Hub.Subscribe("batch.applied")
	defer stop()

	batch, err := a.Service.NewBatch(ctx, domain.ActorUser)
	if err != nil {
		t.Fatal(err)
	}
	task, err := a.Service.CreateTask(ctx, batch, service.TaskFields{RawText: "buy milk and call mom"})
	if err != nil {
		t.Fatal(err)
	}
	a.Cleanup.Enqueue(task)

	select {
	case <-applied:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup never applied a batch")
	}
	tasks, err := a.Service.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks after cleanup, want 2", len(tasks))
	}
}

func TestCloseClosesTheDatabase(t *testing.T) {
	a, err := app.Open(app.Options{DBPath: filepath.Join(t.TempDir(), "test.db"), NewClient: fakeClient, Now: time.Now, Loc: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.DB.Ping(); err == nil {
		t.Error("the database still answers after Close")
	}
}

func TestOpenFailsWithNoUserConfigDir(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	if _, err := app.Open(app.Options{NewClient: fakeClient, Now: time.Now, Loc: time.UTC}); err == nil {
		t.Error("Open succeeded with no home")
	}
}

func TestOpenFailsWhenTheDatabaseCantOpen(t *testing.T) {
	if _, err := app.Open(app.Options{DBPath: t.TempDir(), NewClient: fakeClient, Now: time.Now, Loc: time.UTC}); err == nil {
		t.Error("Open succeeded on a directory")
	}
}

func TestOpenFailsWhenTheDatabaseDirCantBeMade(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Open(app.Options{DBPath: filepath.Join(file, "whatnow.db"), NewClient: fakeClient, Now: time.Now, Loc: time.UTC}); err == nil {
		t.Error("Open succeeded under a file")
	}
}

func TestACaptureReportsAFailureWhenSettingsCantBeRead(t *testing.T) {
	a := open(t, filepath.Join(t.TempDir(), "test.db"))
	ctx := context.Background()
	if _, err := a.DB.ExecContext(ctx, `INSERT INTO settings (id, body) VALUES (1, '{')`); err != nil {
		t.Fatal(err)
	}
	failed, stop := a.Hub.Subscribe("cleanup.failed")
	defer stop()

	batch, err := a.Service.NewBatch(ctx, domain.ActorUser)
	if err != nil {
		t.Fatal(err)
	}
	task, err := a.Service.CreateTask(ctx, batch, service.TaskFields{RawText: "buy milk"})
	if err != nil {
		t.Fatal(err)
	}
	a.Cleanup.Enqueue(task)

	select {
	case <-failed:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup never reported the failure")
	}
}

func openHosted(t *testing.T, path string, zone string) *app.App {
	t.Helper()
	if zone != "" {
		db, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		s := settings.Defaults()
		s.TimeZone = zone
		if err := settings.Save(context.Background(), db, s); err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
	a, err := app.Open(app.Options{DBPath: path, NewClient: fakeClient, Now: time.Now, Loc: time.UTC, Hosted: &app.Hosted{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

func TestOpenHostedUsesUserTimeZone(t *testing.T) {
	a := openHosted(t, filepath.Join(t.TempDir(), "u.db"), "America/Chicago")
	if got := a.Loc.String(); got != "America/Chicago" {
		t.Errorf("Loc = %q, want America/Chicago", got)
	}
}

func TestOpenHostedFallsBackToUTC(t *testing.T) {
	a := openHosted(t, filepath.Join(t.TempDir(), "u.db"), "")
	if got := a.Loc.String(); got != "UTC" {
		t.Errorf("Loc = %q, want UTC", got)
	}
}

func hasView(t *testing.T, a *app.App, id string) bool {
	t.Helper()
	views, err := a.Integrations.Views(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(views, func(v integrations.View) bool { return v.ID == id })
}

func TestOpenHostedLeavesOutReviewQueue(t *testing.T) {
	keyring.MockInit()
	hosted := openHosted(t, filepath.Join(t.TempDir(), "h.db"), "")
	local := open(t, filepath.Join(t.TempDir(), "l.db"))
	if hasView(t, hosted, "reviewqueue") {
		t.Error("hosted app offers reviewqueue")
	}
	if !hasView(t, local, "reviewqueue") {
		t.Error("local app lost reviewqueue")
	}
}

func TestOpenOffersTheTicketFeedLocalAndHosted(t *testing.T) {
	keyring.MockInit()
	apps := map[string]*app.App{
		"local":  open(t, filepath.Join(t.TempDir(), "l.db")),
		"hosted": openHosted(t, filepath.Join(t.TempDir(), "h.db"), ""),
	}
	for mode, a := range apps {
		v, err := a.Integrations.View(context.Background(), "ticketfeed")
		if err != nil {
			t.Errorf("%s app: %v", mode, err)
		} else if v.Pushable {
			t.Errorf("%s app's ticketfeed is pushable", mode)
		}
	}
}

func TestOpenHostedSealsKeys(t *testing.T) {
	a := openHosted(t, filepath.Join(t.TempDir(), "u.db"), "")
	ctx := context.Background()
	if err := a.Clients.Keys.Set(ctx, settings.Anthropic, "k"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := a.DB.QueryRowContext(ctx, `SELECT count(*) FROM secrets`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("secrets rows = %d, want 1", n)
	}
}
