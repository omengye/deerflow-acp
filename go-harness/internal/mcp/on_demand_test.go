package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func waitForMCP(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for MCP fixture")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDesktopColdCatalogConcurrentDiscoveryAndUpgrade(t *testing.T) {
	ctx := context.Background()
	profile := t.TempDir()
	db, err := sqlite.Open(filepath.Join(profile, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	m := testManager(t, harness.MCPPolicy{ConnectTimeout: 15 * time.Second})
	if err = m.SetCatalogDB(ctx, db.DB()); err != nil {
		t.Fatal(err)
	}
	m.SetDesktopProfile(profile)
	var servers []harness.MCPServer
	for i := range 8 {
		s := processConfig(t, "normal")
		s.Name = fmt.Sprintf("server-%d", i)
		s.Env["HARNESS_MCP_TRACE"] = filepath.Join(profile, s.Name+".trace")
		servers = append(servers, s)
	}
	server := servers[3]
	gate := filepath.Join(profile, "gate")
	server.Env["HARNESS_MCP_GATE"] = gate
	saveDesktopServers(t, profile, servers...)
	if err = m.Bind(ctx, "owner", "session", profile, servers); err != nil {
		t.Fatal(err)
	}
	_, g, l, err := m.AcquireForTurn(ctx, "session")
	if err != nil || g == "" || len(l.Catalog()) != 8 || len(l.Tools) != 0 {
		t.Fatalf("cold catalog: %v", err)
	}
	defer l.Close()
	if _, _, err = m.Describe(ctx, "session"); err != nil {
		t.Fatal(err)
	}
	count := func(s harness.MCPServer, event string) int {
		data, _ := os.ReadFile(s.Env["HARNESS_MCP_TRACE"])
		n := 0
		for _, e := range strings.Fields(string(data)) {
			if e == event {
				n++
			}
		}
		return n
	}
	for _, s := range servers {
		if count(s, "start") != 0 {
			t.Fatal("binding or listing started", s.Name)
		}
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	first := make(chan error, 1)
	go func() { first <- l.Discover(cancelCtx, server.Name) }()
	waitForMCP(t, func() bool { return count(server, "initialize") == 1 })
	results := make(chan error, 20)
	for range 20 {
		go func() { results <- l.Discover(ctx, server.Name) }()
	}
	d := l.binding.desktop
	waitForMCP(t, func() bool { d.mu.Lock(); defer d.mu.Unlock(); return d.find(server.Name).process.refs == 21 })
	cancel()
	if err = <-first; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err = os.WriteFile(gate, []byte("open"), 0600); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		if err = <-results; err != nil {
			t.Fatal(err)
		}
	}
	for _, event := range []string{"start", "initialize", "tools/list"} {
		if count(server, event) != 1 {
			t.Fatalf("%s count %d", event, count(server, event))
		}
	}
	for _, s := range servers {
		if s.Name != server.Name && count(s, "start") != 0 {
			t.Fatal("started sibling", s.Name)
		}
	}
	if err = m.Release(ctx, "owner", "session"); err != nil {
		t.Fatal(err)
	}
	if err = m.Bind(ctx, "owner", "next", profile, servers); err != nil {
		t.Fatal(err)
	}
	_, _, next, err := m.AcquireForTurn(ctx, "next")
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	c := next.Catalog()[3]
	if c.Ready || len(c.Tools) != 1 || c.Tools[0].HasDefinition() || c.Tools[0].Original != "echo.value" || count(server, "start") != 1 {
		t.Fatalf("unexpected disk catalog: %+v", c)
	}
	var persisted string
	if err = db.DB().QueryRow("SELECT cards FROM harness_mcp_catalog_v1").Scan(&persisted); err != nil || strings.Contains(persisted, server.Command) || strings.Contains(persisted, fixtureSecret) || strings.Contains(persisted, "inputSchema") {
		t.Fatal("unsafe or missing summary", err)
	}
	if _, err = db.DB().Exec("UPDATE harness_mcp_catalog_v1 SET cards='broken'"); err != nil {
		t.Fatal(err)
	}
	if err = m.Bind(ctx, "owner", "damaged", profile, servers); err != nil {
		t.Fatal(err)
	}
	_, _, damaged, err := m.AcquireForTurn(ctx, "damaged")
	if err != nil {
		t.Fatal(err)
	}
	defer damaged.Close()
	if len(damaged.Catalog()[3].Tools) != 0 || count(server, "start") != 1 {
		t.Fatal("damaged cache did not fall back to cold discovery")
	}
}

func TestHostIdleHonorsLeasesAndPausedApprovals(t *testing.T) {
	ctx, cwd := context.Background(), t.TempDir()
	s := processConfig(t, "host-idle")
	s.Startup, s.IdleTimeoutSeconds = "on_demand", 1
	m := testManager(t, harness.MCPPolicy{})
	if err := m.ConfigureHostRecipes([]harness.HostMCPRecipe{{ID: "recipe", Workspace: cwd, Servers: []harness.MCPServer{s}, RestoreOnRestart: true}}); err != nil {
		t.Fatal(err)
	}
	if err := m.Bind(ctx, "owner", "chat", cwd, nil); err != nil {
		t.Fatal(err)
	}
	_, g, l, err := m.AcquireForTurn(ctx, "chat")
	if err != nil {
		t.Fatal(err)
	}
	if err = l.Discover(ctx, s.Name); err != nil {
		t.Fatal(err)
	}
	tool, pin, err := l.Pin(ctx, l.Catalog()[0].Tools[0].ToolPin)
	if err != nil {
		t.Fatal(err)
	}
	proc := tool.(*leasedTool).process
	var paused atomic.Bool
	paused.Store(true)
	checked := make(chan bool, 1)
	tool.(*leasedTool).RetainPausedOperation("op", func(context.Context, string) bool {
		select {
		case checked <- true:
		default:
		}
		return paused.Load()
	})
	l.Close()
	select {
	case <-checked:
	case <-time.After(5 * time.Second):
		t.Fatal("paused approval not checked")
	}
	if proc.ctx.Err() != nil {
		t.Fatal("approval lost its process")
	}
	paused.Store(false)
	select {
	case <-proc.done:
	case <-time.After(5 * time.Second):
		t.Fatal("idle process not reaped")
	}
	_, newG, next, err := m.AcquireForTurn(ctx, "chat")
	if err != nil || g != newG {
		t.Fatal("recycling changed binding", err)
	}
	defer next.Close()
	if _, _, err = next.Pin(ctx, pin); !errors.Is(err, harness.ErrResourcesWaiting) {
		t.Fatal("expired approval reused", err)
	}
	cached := next.Catalog()[0].Tools[0]
	if !cached.HasDefinition() || cached.Connection != "" {
		t.Fatal("idle catalog discarded or still live")
	}
	_, current, err := next.Pin(ctx, cached.ToolPin)
	if err != nil || current.Connection == pin.Connection {
		t.Fatal("fresh generation missing", err)
	}
}

func TestHostRestoreStartsOnlyTheCheckpointTarget(t *testing.T) {
	ctx, cwd := context.Background(), t.TempDir()
	a, b := processConfig(t, "restore-a"), processConfig(t, "restore-b")
	a.Name, b.Name = "database", "browser"
	a.Startup, b.Startup = "on_demand", "on_demand"
	for _, s := range []harness.MCPServer{a, b} {
		s.Env["HARNESS_MCP_TRACE"] = filepath.Join(cwd, s.Name+".trace")
	}
	recipe := harness.HostMCPRecipe{ID: "workspace", Workspace: cwd, Servers: []harness.MCPServer{a, b}, RestoreOnRestart: true}
	manager := func() *Manager {
		m := testManager(t, harness.MCPPolicy{})
		if err := m.ConfigureHostRecipes([]harness.HostMCPRecipe{recipe}); err != nil {
			t.Fatal(err)
		}
		return m
	}
	first := manager()
	if err := first.Bind(ctx, "owner", "session", cwd, nil); err != nil {
		t.Fatal(err)
	}
	desc, origin, lease, err := first.AcquireForTurn(ctx, "session")
	if err != nil {
		t.Fatal(err)
	}
	if err = lease.Discover(ctx, a.Name); err != nil {
		t.Fatal(err)
	}
	var pin ToolPin
	for _, s := range lease.Catalog() {
		if s.Name == a.Name {
			pin = s.Tools[0].ToolPin
		}
	}
	if _, pin, err = lease.Pin(ctx, pin); err != nil {
		t.Fatal(err)
	}
	lease.Close()
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	restored := manager()
	ref := harness.MCPResourceRef{Version: 2, ToolExposure: "auto", SessionID: "session", HostRecipeID: recipe.ID, OriginGeneration: origin, Descriptor: desc}
	g, err := restored.RestoreHost(ctx, ref, cwd)
	if err != nil || g == origin {
		t.Fatal("restore definition", err)
	}
	current, err := restored.Acquire(ctx, "session", desc, g)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	count := func(s harness.MCPServer) int {
		data, _ := os.ReadFile(s.Env["HARNESS_MCP_TRACE"])
		return strings.Count(string(data), "start\n")
	}
	if count(a) != 1 || count(b) != 0 {
		t.Fatal("restoring the definition launched a child")
	}
	if _, _, err = current.Pin(ctx, pin); !errors.Is(err, harness.ErrResourcesWaiting) {
		t.Fatal("old connection was admitted", err)
	}
	if _, next, err := current.RestorePin(ctx, origin, pin); err != nil || next.Digest != pin.Digest || next.Connection == pin.Connection {
		t.Fatal("explicit host restoration", err)
	}
	if count(a) != 2 || count(b) != 0 {
		t.Fatal("restored the wrong set of children")
	}
}
