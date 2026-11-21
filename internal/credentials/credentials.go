// Package credentials keeps this provider's OAuth tokens in an atomic,
// private file. A cancellable file lock coordinates renewal across processes.
package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Store is the file of one provider, which holds the credentials of each
// server it reaches, by the server's address.
type Store struct {
	// Path is the file. Empty means there is nowhere to keep credentials,
	// so none are found and none can be stored.
	Path string
}

// Default returns the store of the named provider, in the user's
// configuration directory. It is empty when that directory is not known.
func Default(provider string) Store {
	dir, err := os.UserConfigDir()

	if err != nil {
		return Store{}
	}

	return Store{Path: filepath.Join(dir, "rumbo-claude", provider+".json")}
}

type server struct {
	Grant *Grant `json:"grant,omitempty"`
}

// Grant is the OAuth token set. Only this provider reads it.
type Grant struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	Scopes       []string  `json:"scopes,omitempty"`
}

// Grant returns what signing in to server gave, or nil when nobody is
// signed in.
func (s Store) Grant(address string) (*Grant, error) {
	servers, err := s.read()

	if err != nil {
		return nil, err
	}

	return servers[address].Grant, nil
}

// SetGrant keeps a token set; nil removes it locally.
func (s Store) SetGrant(address string, grant *Grant) error {
	servers, err := s.read()

	if err != nil {
		return err
	}

	kept := servers[address]
	kept.Grant = grant
	servers[address] = kept

	return s.write(servers)
}

// Locked runs do while no other process holds the store, so two processes
// of a provider do not renew one grant with the same token at once.
func (s Store) Locked(ctx context.Context, do func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.Path == "" {
		return do()
	}

	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}

	lock, err := os.OpenFile(s.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)

	if err != nil {
		return err
	}

	defer lock.Close()

	for {
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}

	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	if err := ctx.Err(); err != nil {
		return err
	}
	return do()
}

func (s Store) read() (map[string]server, error) {
	servers := map[string]server{}

	if s.Path == "" {
		return servers, nil
	}

	raw, err := os.ReadFile(s.Path)

	if errors.Is(err, os.ErrNotExist) {
		return servers, nil
	}

	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(raw, &servers); err != nil {
		return nil, fmt.Errorf("the credentials in %s cannot be read: %w", s.Path, err)
	}

	if servers == nil {
		return nil, fmt.Errorf("the credentials in %s must be a JSON object, not null", s.Path)
	}
	return servers, nil
}

// write replaces the file whole, readable only by the user, so a reader
// never finds half of it.
func (s Store) write(servers map[string]server) error {
	if s.Path == "" {
		return errors.New("there is no configuration directory to keep credentials in")
	}

	dir := filepath.Dir(s.Path)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	raw, _ := json.MarshalIndent(servers, "", "  ")

	// A temporary file is created readable only by its owner.
	file, err := os.CreateTemp(dir, ".credentials-*")

	if err != nil {
		return err
	}

	defer os.Remove(file.Name())

	if _, err := file.Write(append(raw, '\n')); err != nil {
		file.Close()
		return err
	}

	if err := file.Close(); err != nil {
		return err
	}

	return os.Rename(file.Name(), s.Path)
}
