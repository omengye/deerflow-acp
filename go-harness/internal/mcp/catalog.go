package mcp

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"unicode"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

const maxCatalogBytes = 8 << 20

// A pin is produced from the schema offered in a particular model step. It
// travels with the assistant tool-call batch through native Eino checkpoints.
type ToolPin struct {
	Server     string `json:"server"`
	Name       string `json:"name"`
	Digest     string `json:"digest"`
	Connection string `json:"connection,omitempty"`
	Binding    string `json:"binding,omitempty"`
	Program    string `json:"program,omitempty"`
}

type CatalogTool struct {
	ToolPin
	Original    string `json:"original"`
	Description string `json:"description,omitempty"`
	definition  []byte
}

func (c CatalogTool) HasDefinition() bool { return len(c.definition) > 0 }

// Give model providers a private copy of just the selected definitions.
// The immutable bytes also identify the exact adapter used for dispatch.
func (c CatalogTool) Definition() (*schema.ToolInfo, error) {
	var info schema.ToolInfo
	if err := json.Unmarshal(c.definition, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

type CatalogServer struct {
	Name, Description string
	Tools             []CatalogTool
	Ready             bool // complete schemas are in memory, even during expected idle shutdown
	CanDiscover       bool
}

func cardFor(t *guardTool) (CatalogTool, error) {
	info, err := t.Info(context.Background())
	if err != nil {
		return CatalogTool{}, err
	}
	runes := []rune(strings.Join(strings.Fields(info.Desc), " "))
	return CatalogTool{ToolPin: ToolPin{Server: t.endpoint.name, Name: info.Name, Digest: fmt.Sprintf("%x", sha256.Sum256(t.info))}, Original: t.original, Description: string(runes[:min(240, len(runes))]), definition: t.info}, nil
}

func catalogTools(items []tool.BaseTool) ([]CatalogTool, int, error) {
	cards := make([]CatalogTool, 0, len(items))
	size := 0
	for _, item := range items {
		t := item.(*guardTool)
		card, err := cardFor(t)
		if err != nil {
			return nil, 0, err
		}
		size += len(t.info)
		cards = append(cards, card)
	}
	return cards, size, nil
}

func (m *Manager) SetCatalogDB(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS harness_mcp_catalog_v1 (
		fingerprint TEXT PRIMARY KEY, cards BLOB NOT NULL, updated INTEGER NOT NULL)`)
	if err == nil {
		m.catalogDB = db
	}
	return err
}

func (s *desktopServer) catalogKey(cwd string, file os.FileInfo) string {
	identity, err := programFileID(s.target, file)
	if err != nil {
		return ""
	}
	data, _ := json.Marshal([]any{1, s.saved, pathKey(cwd), pathKey(s.target), s.config, identity, file.Size(), file.ModTime().UnixNano()})
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (s *desktopServer) loadCatalog(ctx context.Context, m *Manager, cwd string) {
	if s.check(m, cwd) != nil || m.catalogDB == nil || s.cacheKey == "" {
		return
	}
	var data []byte
	if m.catalogDB.QueryRowContext(ctx, "SELECT cards FROM harness_mcp_catalog_v1 WHERE fingerprint=? AND length(cards)<=?", s.cacheKey, 1<<20).Scan(&data) != nil {
		return
	}
	var cards []CatalogTool
	if json.Unmarshal(data, &cards) != nil || len(cards) > m.policy.MaxTools || containsCredentials(data, credentialValues(s.config)) {
		return
	}
	seen := map[string]bool{}
	for _, c := range cards {
		digest, err := hex.DecodeString(c.Digest)
		if err != nil || len(digest) != 32 || c.Binding != "" || c.Program != "" || c.Connection != "" || c.Server != s.config.Name || c.Name != toolName(c.Server, c.Original) || c.Original == "" || len(c.Original) > 256 || strings.IndexFunc(c.Original, unicode.IsControl) >= 0 || len([]rune(c.Description)) > 240 || seen[c.Name] {
			return
		}
		seen[c.Name] = true
	}
	s.cards = cards
}

func (m *Manager) saveCatalog(ctx context.Context, key string, cards []CatalogTool) {
	if m.catalogDB == nil || key == "" {
		return
	}
	data, err := json.Marshal(cards)
	if err != nil || len(data) > 1<<20 {
		return
	}
	// Cache writes are optional. SQLite publishes a complete service list in
	// one statement and readers can always fall back to targeted discovery.
	_, _ = m.catalogDB.ExecContext(ctx, "INSERT OR REPLACE INTO harness_mcp_catalog_v1(fingerprint,cards,updated) VALUES(?,?,unixepoch())", key, data)
}

func (l *Lease) FullExposure() bool { return l.manager.policy.ToolExposure == "full" }

// Catalog reads only authorized local information. It never opens a transport.
func (l *Lease) Catalog() []CatalogServer {
	if l.Check() != nil {
		return nil
	}
	out := slices.Clone(l.eagerCatalog)
	if d := l.binding.desktop; d != nil {
		d.mu.Lock()
		defer d.mu.Unlock()
		for _, s := range d.servers {
			if errors.Is(s.check(l.manager, d.cwd), harness.ErrMCPLaunchDenied) {
				continue
			}
			c := CatalogServer{Name: s.config.Name, Description: s.config.Description, Ready: s.complete, CanDiscover: true, Tools: slices.Clone(s.cards)}
			for i := range c.Tools {
				c.Tools[i].Binding = l.binding.generation
				c.Tools[i].Program = s.cacheKey
			}
			if p := s.process; p != nil && p.endpoint != nil && p.ctx.Err() == nil && p.endpoint.ctx.Err() == nil {
				for i := range c.Tools {
					c.Tools[i].Connection = p.endpoint.desktopID
				}
			}
			out = append(out, c)
		}
	}
	return out
}

func (l *Lease) Discover(ctx context.Context, name string) error {
	if err := l.Check(); err != nil {
		return err
	}
	d := l.binding.desktop
	if d == nil || d.find(name) == nil {
		return harness.ErrPermissionDenied
	}
	s := d.find(name)
	op, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(l.ctx, cancel)
	defer func() { stop(); cancel() }()
	p, err := d.acquire(op, l.manager, l.binding, s, "")
	if err == nil {
		l.holdMu.Lock()
		if l.fresh == nil {
			l.fresh = map[*desktopProcess]bool{}
		}
		l.fresh[p] = true
		l.holdMu.Unlock()
		d.release(s, p)
	}
	return err
}

// Pin obtains the actual adapter before approval. Its reference prevents idle
// recycle during the attempt. Dispatch checks this same adapter again.
func (l *Lease) Pin(ctx context.Context, pin ToolPin) (tool.BaseTool, ToolPin, error) {
	if err := l.Check(); err != nil {
		return nil, pin, err
	}
	if pin.Binding != l.binding.generation {
		return nil, pin, harness.ErrResourcesWaiting
	}
	d := l.binding.desktop
	if d == nil || d.find(pin.Server) == nil {
		for _, item := range l.Tools {
			t := item.(*leasedTool)
			if t.raw.endpoint.name != pin.Server {
				continue
			}
			card, err := cardFor(t.raw.guardTool)
			if err == nil && card.Name == pin.Name && card.Digest == pin.Digest && pin.Program == t.raw.endpoint.programKey && pin.Connection == l.binding.generation {
				return t, pin, nil
			}
		}
		return nil, pin, harness.ErrResourcesIncompatible
	}
	s := d.find(pin.Server)
	d.mu.Lock()
	checkErr := s.check(l.manager, d.cwd)
	sameDefinition := s.cacheKey == pin.Program
	d.mu.Unlock()
	if checkErr != nil {
		return nil, pin, checkErr
	}
	if !sameDefinition {
		return nil, pin, harness.ErrResourcesIncompatible
	}
	op, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(l.ctx, cancel)
	defer func() { stop(); cancel() }()
	p, err := d.acquire(op, l.manager, l.binding, s, pin.Connection)
	if err != nil {
		return nil, pin, err
	}
	e := p.endpoint
	d.mu.Lock()
	sameDefinition = s.cacheKey == pin.Program && s.process == p
	d.mu.Unlock()
	if !sameDefinition {
		d.release(s, p)
		return nil, pin, harness.ErrResourcesIncompatible
	}
	l.holdMu.Lock()
	fresh := l.fresh[p]
	delete(l.fresh, p)
	l.holdMu.Unlock()
	caps := e.client.InitializeResult().Capabilities
	if !fresh && pin.Connection != "" && (caps == nil || caps.Tools == nil || !caps.Tools.ListChanged) {
		if err := d.relist(op, l.manager, l.binding, s, p, e); err != nil {
			d.release(s, p)
			return nil, pin, err
		}
	}
	for _, item := range *e.desktopTools.Load() {
		t := item.(*guardTool)
		if toolName(e.name, t.original) != pin.Name || fmt.Sprintf("%x", sha256.Sum256(t.info)) != pin.Digest {
			continue
		}
		pin.Connection = e.desktopID
		l.holdMu.Lock()
		if l.ctx.Err() != nil {
			l.holdMu.Unlock()
			d.release(s, p)
			return nil, pin, ErrClosed
		}
		if l.holds == nil {
			l.holds = make(map[*desktopProcess]func())
		}
		if l.holds[p] == nil {
			stopWatching := func() bool { return true }
			if l.pinned {
				stopWatching = context.AfterFunc(e.ctx, l.cancel)
			}
			l.holds[p] = func() { stopWatching(); d.release(s, p) }
		} else {
			d.release(s, p)
		}
		l.holdMu.Unlock()
		return &leasedTool{lease: l, raw: &enhancedGuardTool{t}, server: s, process: p}, pin, nil
	}
	d.release(s, p)
	return nil, pin, harness.ErrResourcesIncompatible
}

// The caller holds an explicit restorable host recipe grant. A crashed child
// in the same binding cannot inherit approval using an equal schema.
func (l *Lease) RestorePin(ctx context.Context, origin string, pin ToolPin) (tool.BaseTool, ToolPin, error) {
	if !l.manager.hostOnly || origin == l.binding.generation || pin.Binding == l.binding.generation || pin.Binding == "" {
		return nil, pin, harness.ErrResourcesWaiting
	}
	pin.Binding = l.binding.generation
	pin.Connection = l.binding.generation
	if d := l.binding.desktop; d != nil && d.find(pin.Server) != nil {
		pin.Connection = ""
	}
	return l.Pin(ctx, pin)
}
