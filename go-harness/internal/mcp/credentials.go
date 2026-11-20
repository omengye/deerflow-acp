package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
)

func (t *guardTool) CredentialTarget(args string) (string, error) {
	endpoint := t.endpoint.credentialEndpoint
	if endpoint == "" {
		return "", errors.New("MCP endpoint has no service identity")
	}
	var input any
	if json.Unmarshal([]byte(args), &input) != nil {
		return "", errors.New("invalid MCP arguments")
	}
	target := ""
	var visit func(any) error
	visit = func(node any) error {
		switch x := node.(type) {
		case []any:
			for _, v := range x {
				if err := visit(v); err != nil {
					return err
				}
			}
		case map[string]any:
			for _, v := range x {
				if err := visit(v); err != nil {
					return err
				}
			}
		case string:
			if strings.HasPrefix(x, "https://") || strings.HasPrefix(x, "http://") {
				origin, err := secrets.ServiceOrigin(x)
				if err != nil {
					return err
				}
				if target != "" && target != origin {
					return errors.New("credential use must identify a single destination service")
				}
				target = origin
			}
		}
		return nil
	}
	if err := visit(input); err != nil {
		return "", err
	}
	if target != "" && target != endpoint {
		return endpoint + " → " + target, nil
	}
	return endpoint, nil
}

func (t *guardTool) CredentialScope(args string) (secrets.CredentialScope, error) {
	target, err := t.CredentialTarget(args)
	if err != nil {
		return secrets.CredentialScope{}, err
	}
	info, err := t.Info(context.Background())
	if err != nil {
		return secrets.CredentialScope{}, err
	}
	return secrets.CredentialScope{Service: target, Consumer: "tool:" + info.Name + "@" + t.endpoint.credentialIdentity, Account: "default"}, nil
}
