package manager

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Stable across configuration changes and agent restarts; local to this guest.
// Never expose this token in status, logs, or the owner's browser session.
func guestLifecycleToken(runDir string) (string, error) {
	path := filepath.Join(runDir, "lifecycle.token")
	data, err := os.ReadFile(path)
	if err == nil {
		return string(data), nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(runDir, 0700); err != nil {
		return "", err
	}
	data = make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	token := hex.EncodeToString(data)
	if err := os.WriteFile(path, []byte(token), 0600); err != nil {
		return "", err
	}
	return token, nil
}

// New guests close Chromium before init syncs/reboots. Old templates may not
// implement this endpoint, so Driver.Stop retains its existing bounded fallback.
func prepareGuestStop(guestIP, token string) {
	req, err := http.NewRequest(http.MethodPost, "http://"+guestIP+":8080/internal/prepare-stop", nil)
	if err != nil {
		return
	}
	req.Header.Set("X-Workspace-Lifecycle-Token", token)
	response, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err == nil {
		_ = response.Body.Close()
	}
}
