package credentials

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLockWaitCanBeCancelled(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "tokens.json")}
	held, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		finished <- store.Locked(context.Background(), func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := store.Locked(ctx, func() error { t.Error("entered held lock"); return nil })
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting lock: %v", err)
	}
}

func TestMalformedStoreIsNotOverwritten(t *testing.T) {
	for _, original := range []string{"not JSON", "null", "[]"} {
		t.Run(original, func(t *testing.T) {
			store := Store{Path: filepath.Join(t.TempDir(), "tokens.json")}
			if err := os.WriteFile(store.Path, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			if err := store.SetGrant("api", &Grant{AccessToken: "new"}); err == nil {
				t.Fatal("replaced malformed credentials")
			}
			raw, err := os.ReadFile(store.Path)
			if err != nil || string(raw) != original {
				t.Fatalf("store changed: %s, %v", raw, err)
			}
		})
	}
}
