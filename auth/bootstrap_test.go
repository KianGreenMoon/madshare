package auth

import (
	"context"
	"errors"
	"testing"
)

// fakeBootstrapStore is an in-memory BootstrapStore for testing Bootstrap's
// branching without a database.
type fakeBootstrapStore struct {
	userCount int
	created   []string // usernames passed to CreateUser
	roles     map[int64]string
	nextID    int64
}

func (f *fakeBootstrapStore) CountUsers(context.Context) (int, error) { return f.userCount, nil }

func (f *fakeBootstrapStore) CreateUser(_ context.Context, username, _ string, _ bool) (int64, error) {
	f.nextID++
	f.created = append(f.created, username)
	f.userCount++
	return f.nextID, nil
}

func (f *fakeBootstrapStore) AssignRoleByName(_ context.Context, userID int64, role string) error {
	if f.roles == nil {
		f.roles = map[int64]string{}
	}
	f.roles[userID] = role
	return nil
}

func TestBootstrap_EmptyWithPassword_CreatesAdmin(t *testing.T) {
	f := &fakeBootstrapStore{}
	created, err := Bootstrap(context.Background(), f, "kian", "hunter2hunter")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if !created {
		t.Fatal("created = false, want true")
	}
	if len(f.created) != 1 || f.created[0] != "kian" {
		t.Errorf("created users = %v, want [kian]", f.created)
	}
	if f.roles[1] != RoleAdmin {
		t.Errorf("role = %q, want %q", f.roles[1], RoleAdmin)
	}
}

func TestBootstrap_EmptyNoPassword_Errors(t *testing.T) {
	f := &fakeBootstrapStore{}
	_, err := Bootstrap(context.Background(), f, "admin", "")
	if !errors.Is(err, ErrNoAdminCredential) {
		t.Fatalf("err = %v, want ErrNoAdminCredential", err)
	}
}

func TestBootstrap_UsersExist_NoOp(t *testing.T) {
	f := &fakeBootstrapStore{userCount: 3}
	created, err := Bootstrap(context.Background(), f, "admin", "ignored-password")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if created {
		t.Error("created = true, want false when users already exist")
	}
	if len(f.created) != 0 {
		t.Errorf("created users = %v, want none", f.created)
	}
}
