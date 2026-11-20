package desktopconfig

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
	"github.com/omengye/deerflow-acp/go-harness/internal/websearch"
	"go.yaml.in/yaml/v3"
)

func (s service) searchManager() (*websearch.Manager, error) {
	body, err := os.ReadFile(s.config)
	if err != nil {
		return nil, err
	}
	config, err := websearch.SeedYAML(body)
	if err != nil {
		return nil, err
	}
	manager, err := websearch.New(filepath.Join(filepath.Dir(s.config), "search-providers.json"), config)
	if err != nil {
		return nil, err
	}
	attachSearchSecretCheck(s.config, manager)
	return manager, nil
}

func attachSearchSecretCheck(configPath string, manager *websearch.Manager) {
	if manager == nil {
		return
	}
	manager.UseSecretCheck(func(_ context.Context, name string) bool {
		ref := secrets.ReferenceTo(name)
		values := launch.ReadConfigValues(configPath, "", ref)
		resolved, err := values.Resolve(ref)
		return err == nil && resolved != "" && len(resolved) <= 16384
	})
}

// Called under the configuration lock. Persist the search store first, then
// remove only the superseded web_search fields, preserving unrelated YAML and
// an exact backup. A failure leaves the old credentials available for retry.
func (s service) initializeSearchPlugins() error {
	manager, err := s.searchManager()
	if err != nil {
		return err
	}
	if err := manager.Initialize(context.Background()); err != nil {
		return err
	}
	body, err := os.ReadFile(s.config)
	if err != nil {
		return err
	}
	var document yaml.Node
	if err := yaml.Unmarshal(body, &document); err != nil {
		return err
	}
	if len(document.Content) == 0 {
		return nil
	}
	root := document.Content[0]
	changed := false
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "tools" {
			continue
		}
		for _, tool := range root.Content[i+1].Content {
			var identity struct {
				Name string `yaml:"name"`
			}
			if tool.Decode(&identity) != nil || identity.Name != "web_search" {
				continue
			}
			kept := tool.Content[:0]
			for j := 0; j+1 < len(tool.Content); j += 2 {
				switch tool.Content[j].Value {
				case "api_key", "https_proxy", "timeout":
					changed = true
				default:
					kept = append(kept, tool.Content[j], tool.Content[j+1])
				}
			}
			tool.Content = kept
		}
	}
	if !changed {
		return nil
	}
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		return err
	}
	if err := encoder.Close(); err != nil {
		return err
	}
	backup := s.config + ".search-migration.bak"
	file, err := os.OpenFile(backup, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err == nil {
		_, err = file.Write(body)
		if err == nil {
			err = file.Sync()
		}
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(backup)
			return err
		}
	} else if !errors.Is(err, os.ErrExist) {
		return err
	}
	return writeAtomic(s.config, output.Bytes())
}
