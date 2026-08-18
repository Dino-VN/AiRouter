package app

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// randomPassword returns a URL-safe password for the generated admin account.
func randomPassword() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
