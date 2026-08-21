package passkey

import (
	"context"
	"github.com/dbehnke/opusref/internal/webapp/auth"
	"github.com/dbehnke/opusref/internal/webapp/store"
	"testing"
)

func TestLoginOptionsAreDiscoverableAndCeremonyIsOneUse(t *testing.T) {
	state, err := store.Open(context.Background(), t.TempDir()+"/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	hash, _ := auth.HashPassword("quiet marble nebula orchard", auth.DefaultParams())
	_, _ = state.CreateUser(context.Background(), store.CreateUser{Username: "alice", Role: store.RoleAdmin, PasswordHash: hash})
	manager, err := New("example.test", "OpusRef", []string{"https://example.test"}, state)
	if err != nil {
		t.Fatal(err)
	}
	id, options, err := manager.BeginLogin()
	if err != nil || id == "" || options == nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err = manager.take(id, "login", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = manager.take(id, "login", "", ""); err == nil {
		t.Fatal("ceremony replay accepted")
	}
}
