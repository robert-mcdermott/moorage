package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/ids"
)

func addUser(t *testing.T, db *DB, email, role string) User {
	t.Helper()
	u := User{ID: ids.New(ids.User), Email: email, Name: email, PasswordHash: "hash", Role: role, CreatedAt: time.Now()}
	if err := db.CreateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestClaimServerOnlyOnce(t *testing.T) {
	db := migrated(t)
	ctx := context.Background()
	first := User{ID: ids.New(ids.User), Email: "a@example.com", Name: "A", PasswordHash: "h", Role: RoleAdmin, CreatedAt: time.Now()}
	if err := db.ClaimServer(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.ID, second.Email = ids.New(ids.User), "b@example.com"
	if err := db.ClaimServer(ctx, second); !errors.Is(err, ErrAlreadyClaimed) {
		t.Errorf("a second claim: %v", err)
	}
	if n, _ := db.CountUsers(ctx); n != 1 {
		t.Errorf("%d users", n)
	}
	if err := db.CreateUser(ctx, User{ID: ids.New(ids.User), Email: "a@example.com", Name: "A", Role: RoleMember, CreatedAt: time.Now()}); !errors.Is(err, ErrEmailTaken) {
		t.Errorf("a taken email: %v", err)
	}
}

func TestLastAdminStays(t *testing.T) {
	db := migrated(t)
	ctx := context.Background()
	admin := addUser(t, db, "admin@example.com", RoleAdmin)
	if err := db.SetUserRole(ctx, admin.ID, RoleMember); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("demoting the last admin: %v", err)
	}
	if err := db.SetUserDisabled(ctx, admin.ID, true); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("disabling the last admin: %v", err)
	}
	other := addUser(t, db, "other@example.com", RoleMember)
	if err := db.SetUserRole(ctx, other.ID, RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := db.SetUserDisabled(ctx, admin.ID, true); err != nil {
		t.Fatal(err)
	}
	// A disabled admin does not count: other is now the last one.
	if err := db.SetUserRole(ctx, other.ID, RoleMember); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("demoting the last enabled admin: %v", err)
	}
	if u, _ := db.UserByID(ctx, admin.ID); !u.Disabled() {
		t.Error("not disabled")
	}
	if err := db.SetUserDisabled(ctx, admin.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := db.SetUserRole(ctx, "usr_missing", RoleMember); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unknown user: %v", err)
	}
}

func TestSessions(t *testing.T) {
	db := migrated(t)
	ctx := context.Background()
	u := addUser(t, db, "a@example.com", RoleAdmin)
	now := time.Now()
	s := Session{ID: ids.New(ids.Session), IDHash: "hash1", UserID: u.ID, CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour), IP: "10.0.0.1", UserAgent: "test"}
	if err := db.CreateSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	got, err := db.SessionByHash(ctx, "hash1")
	if err != nil || got.UserID != u.ID || got.IP != "10.0.0.1" {
		t.Fatalf("session: %+v %v", got, err)
	}
	expired := Session{ID: ids.New(ids.Session), IDHash: "hash2", UserID: u.ID, CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(-time.Minute)}
	if err := db.CreateSession(ctx, expired); err != nil {
		t.Fatal(err)
	}
	if list, _ := db.UserSessions(ctx, u.ID); len(list) != 1 {
		t.Errorf("sessions: %+v", list)
	}
	other := addUser(t, db, "b@example.com", RoleMember)
	if err := db.DeleteSession(ctx, other.ID, s.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("ending someone else's session: %v", err)
	}
	if err := db.DeleteSession(ctx, u.ID, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SessionByHash(ctx, "hash1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after ending it: %v", err)
	}

	// Disabling a user ends their sessions.
	s.ID, s.IDHash = ids.New(ids.Session), "hash3"
	db.CreateSession(ctx, s)
	addUser(t, db, "admin2@example.com", RoleAdmin)
	if err := db.SetUserDisabled(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SessionByHash(ctx, "hash3"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the session of a disabled user: %v", err)
	}
}

func TestTokens(t *testing.T) {
	db := migrated(t)
	ctx := context.Background()
	u := addUser(t, db, "a@example.com", RoleMember)
	tok := APIToken{ID: ids.New(ids.Token), UserID: u.ID, Name: "ci", TokenHash: "h1", Scopes: []string{"deploy"}, CreatedAt: time.Now()}
	if err := db.CreateToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	dup := tok
	dup.ID, dup.TokenHash = ids.New(ids.Token), "h2"
	if err := db.CreateToken(ctx, dup); !errors.Is(err, ErrTokenNameTaken) {
		t.Errorf("a name in use: %v", err)
	}
	// Another owner may use the name; the local administrator is an owner too.
	local := dup
	local.UserID = ""
	if err := db.CreateToken(ctx, local); err != nil {
		t.Fatal(err)
	}
	restricted := APIToken{ID: ids.New(ids.Token), UserID: u.ID, Name: "shop", TokenHash: "h3", Scopes: []string{"read"}, App: "shop", CreatedAt: time.Now()}
	if err := db.CreateToken(ctx, restricted); !errors.Is(err, ErrNotFound) {
		t.Errorf("a restriction to an unknown app: %v", err)
	}
	if _, _, err := db.EnsureApp(ctx, "shop"); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateToken(ctx, restricted); err != nil {
		t.Fatal(err)
	}
	got, err := db.TokenByHash(ctx, "h3")
	if err != nil || got.App != "shop" || got.UserID != u.ID || len(got.Scopes) != 1 || got.Scopes[0] != "read" {
		t.Fatalf("token: %+v %v", got, err)
	}
	db.TouchToken(ctx, got.ID)
	if got, _ := db.TokenByHash(ctx, "h3"); got.LastUsedAt.IsZero() {
		t.Error("last use not recorded")
	}
	if list, _ := db.Tokens(ctx, u.ID); len(list) != 2 {
		t.Errorf("a user's tokens: %+v", list)
	}
	if list, _ := db.Tokens(ctx, ""); len(list) != 1 {
		t.Errorf("the local administrator's tokens: %+v", list)
	}
	if _, err := db.DeleteToken(ctx, "", "shop"); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoking someone else's token: %v", err)
	}
	if _, err := db.DeleteToken(ctx, u.ID, "shop"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.TokenByHash(ctx, "h3"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a revoked token: %v", err)
	}
}

func TestDeviceCodes(t *testing.T) {
	db := migrated(t)
	ctx := context.Background()
	u := addUser(t, db, "a@example.com", RoleMember)
	d := DeviceCode{CodeHash: "c1", UserCode: "ABCD-EFGH", Client: "moor on laptop", IP: "10.0.0.2", ExpiresAt: time.Now().Add(time.Minute)}
	if err := db.CreateDeviceCode(ctx, d); err != nil {
		t.Fatal(err)
	}
	if got, err := db.DeviceByUserCode(ctx, "ABCD-EFGH"); err != nil || got.Client != "moor on laptop" {
		t.Fatalf("by user code: %+v %v", got, err)
	}
	tok := APIToken{ID: ids.New(ids.Token), UserID: u.ID, Name: "login", TokenHash: "t1", Scopes: []string{"admin"}, CreatedAt: time.Now()}
	if err := db.RedeemDevice(ctx, "c1", tok); !errors.Is(err, ErrNotFound) {
		t.Errorf("redeeming before approval: %v", err)
	}
	polled, err := db.PollDevice(ctx, "c1")
	if err != nil || polled.ApprovedBy != "" || !polled.LastPollAt.IsZero() {
		t.Errorf("first poll: %+v %v", polled, err)
	}
	if polled, _ := db.PollDevice(ctx, "c1"); polled.LastPollAt.IsZero() {
		t.Error("the poll was not recorded")
	}
	if err := db.DecideDevice(ctx, "ABCD-EFGH", u.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.DecideDevice(ctx, "ABCD-EFGH", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("deciding twice: %v", err)
	}
	if polled, _ := db.PollDevice(ctx, "c1"); polled.ApprovedBy != u.ID {
		t.Errorf("after approval: %+v", polled)
	}
	if err := db.RedeemDevice(ctx, "c1", tok); err != nil {
		t.Fatal(err)
	}
	tok.ID, tok.TokenHash, tok.Name = ids.New(ids.Token), "t2", "login2"
	if err := db.RedeemDevice(ctx, "c1", tok); !errors.Is(err, ErrNotFound) {
		t.Errorf("redeeming twice: %v", err)
	}
	if _, err := db.TokenByHash(ctx, "t2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a second token was made: %v", err)
	}

	gone := DeviceCode{CodeHash: "c2", UserCode: "GONE-GONE", Client: "x", ExpiresAt: time.Now().Add(-time.Minute)}
	db.CreateDeviceCode(ctx, gone)
	if _, err := db.DeviceByUserCode(ctx, "GONE-GONE"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an expired code: %v", err)
	}
	if err := db.DecideDevice(ctx, "GONE-GONE", u.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("approving an expired code: %v", err)
	}
}

func TestInvitations(t *testing.T) {
	db := migrated(t)
	ctx := context.Background()
	u := User{ID: ids.New(ids.User), Email: "new@example.com", Name: "New", Role: RoleMember, CreatedAt: time.Now()}
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	soon := time.Now().Add(time.Hour)
	if err := db.CreateInvitation(ctx, "i1", u.ID, "admin", soon); err != nil {
		t.Fatal(err)
	}
	// A new invitation replaces the old one.
	if err := db.CreateInvitation(ctx, "i2", u.ID, "admin", soon); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InvitedUser(ctx, "i1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a replaced invitation: %v", err)
	}
	if got, err := db.InvitedUser(ctx, "i2"); err != nil || got.ID != u.ID {
		t.Fatalf("invited: %+v %v", got, err)
	}
	now := time.Now()
	db.CreateSession(ctx, Session{ID: ids.New(ids.Session), IDHash: "s", UserID: u.ID, CreatedAt: now, LastSeenAt: now, ExpiresAt: soon})
	got, err := db.AcceptInvitation(ctx, "i2", "newhash")
	if err != nil || got.PasswordHash != "newhash" {
		t.Fatalf("accepted: %+v %v", got, err)
	}
	if _, err := db.AcceptInvitation(ctx, "i2", "again"); !errors.Is(err, ErrNotFound) {
		t.Errorf("accepting twice: %v", err)
	}
	if _, err := db.SessionByHash(ctx, "s"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a new password leaves old sessions: %v", err)
	}
	db.CreateInvitation(ctx, "i3", u.ID, "admin", time.Now().Add(-time.Minute))
	if _, err := db.AcceptInvitation(ctx, "i3", "late"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an expired invitation: %v", err)
	}
}

func TestSSHKeyOwners(t *testing.T) {
	db := migrated(t)
	ctx := context.Background()
	a, b := addUser(t, db, "a@example.com", RoleMember), addUser(t, db, "b@example.com", RoleMember)
	key := func(owner, name, fp string) SSHKey {
		return SSHKey{ID: ids.New(ids.SSHKey), UserID: owner, Name: name, Fingerprint: fp, PublicKey: "k", CreatedAt: time.Now()}
	}
	if err := db.AddSSHKey(ctx, key(a.ID, "laptop", "fp1")); err != nil {
		t.Fatal(err)
	}
	// One key has one owner; a name is unique per owner.
	var exists *KeyExistsError
	err := db.AddSSHKey(ctx, key(b.ID, "mine", "fp1"))
	if !errors.As(err, &exists) || exists.Name != "" {
		t.Errorf("another's key: %v", err)
	}
	if err := db.AddSSHKey(ctx, key(a.ID, "laptop", "fp2")); !errors.As(err, &exists) || exists.Name != "laptop" {
		t.Errorf("a name in use: %v", err)
	}
	if err := db.AddSSHKey(ctx, key(b.ID, "laptop", "fp2")); err != nil {
		t.Errorf("another owner's name: %v", err)
	}
	if err := db.AddSSHKey(ctx, key("", "server", "fp3")); err != nil {
		t.Fatal(err)
	}
	if list, _ := db.SSHKeys(ctx, a.ID); len(list) != 1 || list[0].Fingerprint != "fp1" {
		t.Errorf("a's keys: %+v", list)
	}
	if list, _ := db.SSHKeys(ctx, ""); len(list) != 1 || list[0].UserID != "" {
		t.Errorf("the local administrator's keys: %+v", list)
	}
	if got, _ := db.SSHKeyByFingerprint(ctx, "fp2"); got.UserID != b.ID {
		t.Errorf("by fingerprint: %+v", got)
	}
	if _, err := db.RemoveSSHKey(ctx, a.ID, "fp2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("removing another's key: %v", err)
	}
	if _, err := db.RemoveSSHKey(ctx, b.ID, "laptop"); err != nil {
		t.Errorf("removing one's own: %v", err)
	}
}

func TestPaging(t *testing.T) {
	db := migrated(t)
	ctx := context.Background()
	for _, name := range []string{"app-a", "app-b", "app-c"} {
		if _, _, err := db.EnsureApp(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	apps, _ := db.AppsAfter(ctx, "", 2)
	if len(apps) != 2 || apps[1].Name != "app-b" {
		t.Errorf("first page: %+v", apps)
	}
	if apps, _ := db.AppsAfter(ctx, "app-b", 2); len(apps) != 1 || apps[0].Name != "app-c" {
		t.Errorf("second page: %+v", apps)
	}

	appID, _, _ := db.EnsureApp(ctx, "app-a")
	b := addBuild(t, db, appID)
	base := time.Now().Add(-time.Hour)
	var ids4 []string
	for i := range 4 {
		id := fmt.Sprintf("dep_%d", i)
		ids4 = append(ids4, id)
		// Two share a creation time: the ID breaks the tie.
		at := base.Add(time.Duration(i/2) * time.Minute)
		if err := db.CreateDeployment(ctx, Deployment{ID: id, App: "app-a", Env: "production", SourceType: "x", Status: Queued, BuildID: b, CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	var seen []string
	var created time.Time
	id := ""
	for range 5 {
		page, err := db.DeploymentsBefore(ctx, "app-a", "production", created, id, 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, d := range page {
			seen = append(seen, d.ID)
		}
		last := page[len(page)-1]
		created, id = last.CreatedAt, last.ID
	}
	if fmt.Sprint(seen) != fmt.Sprint([]string{ids4[3], ids4[2], ids4[1], ids4[0]}) {
		t.Errorf("deployments, page by page: %v", seen)
	}

	for range 3 {
		if _, err := db.CreateRelease(ctx, "app-a", "production", b, "deploy", "admin"); err != nil {
			t.Fatal(err)
		}
	}
	if rels, _ := db.ReleasesBefore(ctx, "app-a", "production", 0, 2); len(rels) != 2 || rels[0].Version != 3 {
		t.Errorf("first releases: %+v", rels)
	}
	if rels, _ := db.ReleasesBefore(ctx, "app-a", "production", 2, 2); len(rels) != 1 || rels[0].Version != 1 {
		t.Errorf("older releases: %+v", rels)
	}
}
