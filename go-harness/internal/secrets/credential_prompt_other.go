//go:build !windows

package secrets

import (
	"context"
	"errors"
)

func DesktopCredentialPrompt(context.Context, CredentialScope, CredentialFields) (CredentialPair, error) {
	return CredentialPair{}, errors.New("this client has no native credential form")
}
