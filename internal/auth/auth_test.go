package auth

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/watsons/rms/internal/config"
	"github.com/watsons/rms/internal/namespace"
)

// writeHtpasswd creates an htpasswd file with bcrypt hashes.
func writeHtpasswd(t *testing.T, users map[string]string) string {
	t.Helper()

	var sb strings.Builder
	sb.WriteString("# managed by rms\n")
	for user, pass := range users {
		hash, err := HashPassword(pass)
		if err != nil {
			t.Fatalf("hash %s: %v", user, err)
		}
		fmt.Fprintf(&sb, "%s:%s\n", user, hash)
	}

	path := filepath.Join(t.TempDir(), "htpasswd")
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatalf("write htpasswd: %v", err)
	}
	return path
}

func newTestAuth(t *testing.T, opts Options) *Authenticator {
	t.Helper()
	if opts.Realm == "" {
		opts.Realm = "rms"
	}
	a, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// standardAuth mirrors the documented default: proxy namespaces are pull-only
// but anonymously readable, local namespaces are private, alice and bob own
// their namespaces, and "shared" is writable by any authenticated user.
func standardAuth(t *testing.T) *Authenticator {
	t.Helper()

	path := writeHtpasswd(t, map[string]string{
		"alice": "alicepw",
		"bob":   "bobpw",
		"root":  "rootpw",
	})
	return newTestAuth(t, Options{
		HtpasswdFile:       path,
		Users:              []string{"alice", "bob", "root"},
		SharedNamespaces:   []string{"me", "shared"},
		Admins:             map[string]bool{"root": true},
		AnonymousPullProxy: true,
		AnonymousPullLocal: false,
	})
}

func req(t *testing.T, method, path, user, pass string, withCreds bool) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	if withCreds {
		r.SetBasicAuth(user, pass)
	}
	return r
}

func res(ns string, mode config.Mode, kind namespace.Kind) namespace.Resolution {
	return namespace.Resolution{Namespace: ns, Mode: mode, Kind: kind, EnginePath: "/v2/x", Repo: "x"}
}

func TestAuthenticate(t *testing.T) {
	a := standardAuth(t)

	t.Run("valid credentials", func(t *testing.T) {
		id, ok, present := a.Authenticate(req(t, http.MethodGet, "/v2/", "alice", "alicepw", true))
		if !ok || !present {
			t.Fatalf("ok=%v present=%v, want both true", ok, present)
		}
		if id.Username != "alice" {
			t.Errorf("Username = %q, want alice", id.Username)
		}
		if id.Admin {
			t.Error("alice should not be an admin")
		}
	})

	t.Run("admin flag", func(t *testing.T) {
		id, ok, _ := a.Authenticate(req(t, http.MethodGet, "/v2/", "root", "rootpw", true))
		if !ok || !id.Admin {
			t.Errorf("root should be a valid admin, got ok=%v admin=%v", ok, id.Admin)
		}
	})

	t.Run("wrong password is present but not ok", func(t *testing.T) {
		_, ok, present := a.Authenticate(req(t, http.MethodGet, "/v2/", "alice", "nope", true))
		if ok {
			t.Error("wrong password must not authenticate")
		}
		if !present {
			t.Error("credentials were presented and must be reported as such")
		}
	})

	t.Run("unknown user", func(t *testing.T) {
		_, ok, present := a.Authenticate(req(t, http.MethodGet, "/v2/", "mallory", "x", true))
		if ok || !present {
			t.Errorf("ok=%v present=%v, want false/true", ok, present)
		}
	})

	t.Run("no credentials", func(t *testing.T) {
		_, ok, present := a.Authenticate(req(t, http.MethodGet, "/v2/", "", "", false))
		if ok || present {
			t.Errorf("ok=%v present=%v, want false/false", ok, present)
		}
	})
}

// TestDecideMatrix is the access policy table. It is the security-critical part
// of rms, so every combination is pinned explicitly.
func TestDecideMatrix(t *testing.T) {
	a := standardAuth(t)

	proxyRes := res("docker.io", config.ModeProxy, namespace.KindRepository)
	proxyResGHCR := res("ghcr.io", config.ModeProxy, namespace.KindRepository)
	ownRes := res("alice", config.ModeLocal, namespace.KindRepository)
	otherRes := res("bob", config.ModeLocal, namespace.KindRepository)
	sharedRes := res("shared", config.ModeLocal, namespace.KindRepository)
	meRes := res("me", config.ModeLocal, namespace.KindRepository)

	tests := []struct {
		name      string
		method    string
		res       namespace.Resolution
		user      string
		pass      string
		withCreds bool
		wantCode  string // "" means allowed
		wantStat  int
	}{
		// --- API root: docker login -----------------------------------------
		{name: "login with valid password", method: http.MethodGet, res: res("", "", namespace.KindBase), user: "alice", pass: "alicepw", withCreds: true},
		{name: "login with wrong password", method: http.MethodGet, res: res("", "", namespace.KindBase), user: "alice", pass: "bad", withCreds: true, wantCode: "UNAUTHORIZED", wantStat: 401},
		// The API root always challenges so the client learns the auth scheme;
		// anonymous reads of proxy namespaces are still granted later on.
		{name: "anonymous api root is challenged", method: http.MethodGet, res: res("", "", namespace.KindBase), wantCode: "UNAUTHORIZED", wantStat: 401},

		// --- proxy namespaces -------------------------------------------------
		{name: "anonymous pull from proxy namespace", method: http.MethodGet, res: proxyRes},
		{name: "anonymous HEAD from proxy namespace", method: http.MethodHead, res: proxyRes},
		{name: "authenticated pull from proxy namespace", method: http.MethodGet, res: proxyRes, user: "alice", pass: "alicepw", withCreds: true},
		{name: "bad credentials on proxy pull still 401", method: http.MethodGet, res: proxyRes, user: "alice", pass: "bad", withCreds: true, wantCode: "UNAUTHORIZED", wantStat: 401},
		{name: "push to proxy namespace is denied even when authenticated",
			method: http.MethodPost, res: proxyRes, user: "alice", pass: "alicepw", withCreds: true, wantCode: "DENIED", wantStat: 403},
		{name: "push to proxy namespace is denied when anonymous",
			method: http.MethodPost, res: proxyRes, wantCode: "DENIED", wantStat: 403},
		{name: "delete from proxy namespace is denied",
			method: http.MethodDelete, res: proxyRes, user: "alice", pass: "alicepw", withCreds: true, wantCode: "DENIED", wantStat: 403},
		{name: "push to a different proxy namespace uses that namespace name",
			method: http.MethodPost, res: proxyResGHCR, user: "alice", pass: "alicepw", withCreds: true, wantCode: "DENIED", wantStat: 403},

		// --- local personal namespaces ---------------------------------------
		{name: "anonymous pull from a local namespace is refused", method: http.MethodGet, res: ownRes, wantCode: "UNAUTHORIZED", wantStat: 401},
		{name: "owner pulls own namespace", method: http.MethodGet, res: ownRes, user: "alice", pass: "alicepw", withCreds: true},
		{name: "other user may not pull a private namespace", method: http.MethodGet, res: otherRes, user: "alice", pass: "alicepw", withCreds: true},
		{name: "owner pushes to own namespace", method: http.MethodPost, res: ownRes, user: "alice", pass: "alicepw", withCreds: true},
		{name: "owner PUTs a manifest", method: http.MethodPut, res: ownRes, user: "alice", pass: "alicepw", withCreds: true},
		{name: "owner PATCHes an upload", method: http.MethodPatch, res: ownRes, user: "alice", pass: "alicepw", withCreds: true},
		{name: "owner DELETEs in own namespace", method: http.MethodDelete, res: ownRes, user: "alice", pass: "alicepw", withCreds: true},
		{name: "pushing to another users namespace is denied",
			method: http.MethodPost, res: otherRes, user: "alice", pass: "alicepw", withCreds: true, wantCode: "DENIED", wantStat: 403},
		{name: "anonymous push to a local namespace is challenged",
			method: http.MethodPost, res: ownRes, wantCode: "UNAUTHORIZED", wantStat: 401},
		{name: "wrong password on push is challenged",
			method: http.MethodPost, res: ownRes, user: "alice", pass: "bad", withCreds: true, wantCode: "UNAUTHORIZED", wantStat: 401},

		// --- shared namespaces ------------------------------------------------
		{name: "any authenticated user may push to a shared namespace",
			method: http.MethodPost, res: sharedRes, user: "bob", pass: "bobpw", withCreds: true},
		// "me" is both an htpasswd user and a configured shared namespace: the
		// configuration wins, so any authenticated user may push to it.
		{name: "a configured shared namespace overrides a same-named user",
			method: http.MethodPost, res: meRes, user: "bob", pass: "bobpw", withCreds: true},
		{name: "anonymous push to a shared namespace is challenged",
			method: http.MethodPost, res: sharedRes, wantCode: "UNAUTHORIZED", wantStat: 401},

		// --- admin ------------------------------------------------------------
		{name: "admin pushes to any local namespace",
			method: http.MethodPost, res: otherRes, user: "root", pass: "rootpw", withCreds: true},
		{name: "admin deletes from a proxy namespace (still read-only)",
			method: http.MethodDelete, res: proxyRes, user: "root", pass: "rootpw", withCreds: true, wantCode: "DENIED", wantStat: 403},
		{name: "admin reads the catalog", method: http.MethodGet, res: res("", "", namespace.KindCatalog), user: "root", pass: "rootpw", withCreds: true},
		{name: "non-admin catalog is forbidden", method: http.MethodGet, res: res("", "", namespace.KindCatalog), user: "alice", pass: "alicepw", withCreds: true, wantCode: "DENIED", wantStat: 403},
		{name: "anonymous catalog is challenged", method: http.MethodGet, res: res("", "", namespace.KindCatalog), wantCode: "UNAUTHORIZED", wantStat: 401},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := req(t, tc.method, "/v2/", tc.user, tc.pass, tc.withCreds)
			id, ok, present := a.Authenticate(r)
			got := a.Decide(r, tc.res, id, ok, present)

			if tc.wantCode == "" {
				if got != nil {
					t.Fatalf("request was denied but should be allowed: %v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("request was allowed but should be denied with %s", tc.wantCode)
			}
			if got.Code != tc.wantCode {
				t.Errorf("Code = %q, want %q", got.Code, tc.wantCode)
			}
			if got.Status != tc.wantStat {
				t.Errorf("Status = %d, want %d", got.Status, tc.wantStat)
			}
			if tc.wantStat == 401 && !got.Challenge {
				t.Error("a 401 must carry a WWW-Authenticate challenge")
			}
			if tc.wantStat == 403 && got.Challenge {
				t.Error("a 403 must not carry a challenge")
			}
		})
	}
}

// TestAnonymousCatalogGatedWhenAnonymousPullDisabled covers the config where
// nothing is publicly readable: the API root must then challenge.
func TestAnonymousPullDisabled(t *testing.T) {
	path := writeHtpasswd(t, map[string]string{"alice": "alicepw"})
	a := newTestAuth(t, Options{
		HtpasswdFile:       path,
		Users:              []string{"alice"},
		AnonymousPullProxy: false,
		AnonymousPullLocal: false,
	})

	r := req(t, http.MethodGet, "/v2/", "", "", false)
	id, ok, present := a.Authenticate(r)

	if got := a.Decide(r, res("", "", namespace.KindBase), id, ok, present); got == nil {
		t.Fatal("api root should challenge when anonymous pull is disabled")
	}

	proxyRes := res("docker.io", config.ModeProxy, namespace.KindRepository)
	r = req(t, http.MethodGet, "/v2/docker.io/library/alpine/manifests/latest", "", "", false)
	if got := a.Decide(r, proxyRes, id, ok, present); got == nil {
		t.Fatal("anonymous proxy pull should be refused when disabled")
	}
}

// TestAnonymousLocalPull covers enabling anonymous reads of private
// namespaces, which is useful for a read-only internal mirror.
func TestAnonymousLocalPull(t *testing.T) {
	path := writeHtpasswd(t, map[string]string{"alice": "alicepw"})
	a := newTestAuth(t, Options{
		HtpasswdFile:       path,
		Users:              []string{"alice"},
		AnonymousPullLocal: true,
	})

	r := req(t, http.MethodGet, "/v2/alice/app/manifests/v1", "", "", false)
	id, ok, present := a.Authenticate(r)

	if got := a.Decide(r, res("alice", config.ModeLocal, namespace.KindRepository), id, ok, present); got != nil {
		t.Fatalf("anonymous local pull should be allowed: %v", got)
	}

	// Writes still require authentication.
	r = req(t, http.MethodPost, "/v2/alice/app/blobs/uploads/", "", "", false)
	if got := a.Decide(r, res("alice", config.ModeLocal, namespace.KindRepository), id, ok, present); got == nil {
		t.Fatal("anonymous local push must still be refused")
	}
}

// TestAuthDisabled covers auth.type: none, which is an open registry: with no
// identity there is nothing to authorise against. Proxy namespaces stay
// read-only because that is a property of the namespace.
func TestAuthDisabled(t *testing.T) {
	a := newTestAuth(t, Options{AnonymousPullProxy: true})
	if a.Enabled() {
		t.Fatal("Enabled() should be false without an htpasswd file")
	}

	localRes := res("local", config.ModeLocal, namespace.KindRepository)

	t.Run("presented credentials are accepted and attributed", func(t *testing.T) {
		r := req(t, http.MethodPost, "/v2/local/app/blobs/uploads/", "anyone", "anything", true)
		id, ok, present := a.Authenticate(r)
		if !ok || !present {
			t.Fatalf("ok=%v present=%v, want both true", ok, present)
		}
		if id.Username != "anyone" {
			t.Errorf("Username = %q, want anyone", id.Username)
		}
		if got := a.Decide(r, localRes, id, ok, present); got != nil {
			t.Fatalf("push should be allowed when auth is disabled: %v", got)
		}
	})

	t.Run("anonymous writes are allowed", func(t *testing.T) {
		r := req(t, http.MethodPost, "/v2/local/app/blobs/uploads/", "", "", false)
		id, ok, present := a.Authenticate(r)
		if got := a.Decide(r, localRes, id, ok, present); got != nil {
			t.Fatalf("anonymous push should be allowed when auth is disabled: %v", got)
		}
	})

	t.Run("anonymous reads are allowed", func(t *testing.T) {
		r := req(t, http.MethodGet, "/v2/local/app/manifests/v1", "", "", false)
		id, ok, present := a.Authenticate(r)
		if got := a.Decide(r, localRes, id, ok, present); got != nil {
			t.Fatalf("anonymous pull should be allowed when auth is disabled: %v", got)
		}
	})

	t.Run("proxy namespaces remain read-only", func(t *testing.T) {
		proxyRes := res("docker.io", config.ModeProxy, namespace.KindRepository)
		r := req(t, http.MethodPost, "/v2/docker.io/library/alpine/blobs/uploads/", "", "", false)
		id, ok, present := a.Authenticate(r)

		got := a.Decide(r, proxyRes, id, ok, present)
		if got == nil {
			t.Fatal("push to a proxy namespace must still be refused")
		}
		if got.Status != http.StatusForbidden {
			t.Errorf("Status = %d, want 403", got.Status)
		}
	})
}

func TestUsernames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "htpasswd")
	body := strings.Join([]string{
		"# comment",
		"",
		"alice:$2y$05$abcdefghijklmnopqrstuv",
		"bob:$2y$05$abcdefghijklmnopqrstuv",
		"alice:$2y$05$duplicate",
		"malformed-line-without-colon",
		"   carol:$2y$05$abcdefghijklmnopqrstuv   ",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := Usernames(path)
	if err != nil {
		t.Fatalf("Usernames: %v", err)
	}
	want := []string{"alice", "bob", "carol"}
	if len(got) != len(want) {
		t.Fatalf("Usernames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Usernames() = %v, want %v", got, want)
		}
	}
}

func TestUsernamesErrors(t *testing.T) {
	if _, err := Usernames(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("expected an error for a missing file")
	}

	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, []byte("# only a comment\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Usernames(empty); err == nil {
		t.Error("expected an error for a file with no users")
	}
}

// TestPlaintextIsRejected guards against a malformed line silently becoming a
// plaintext credential.
func TestPlaintextIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "htpasswd")
	if err := os.WriteFile(path, []byte("alice:plaintextpassword\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	a, err := New(Options{HtpasswdFile: path, Users: []string{"alice"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	r := req(t, http.MethodGet, "/v2/", "alice", "plaintextpassword", true)
	if _, ok, _ := a.Authenticate(r); ok {
		t.Fatal("a plaintext htpasswd entry must not authenticate")
	}
}

func TestReloadPicksUpNewUsers(t *testing.T) {
	path := writeHtpasswd(t, map[string]string{"alice": "alicepw"})
	a := newTestAuth(t, Options{HtpasswdFile: path, Users: []string{"alice"}})

	r := req(t, http.MethodGet, "/v2/", "bob", "bobpw", true)
	if _, ok, _ := a.Authenticate(r); ok {
		t.Fatal("bob should not exist yet")
	}

	hash, err := HashPassword("bobpw")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := fmt.Fprintf(f, "bob:%s\n", hash); err != nil {
		t.Fatalf("append: %v", err)
	}
	_ = f.Close()

	if err := a.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	r = req(t, http.MethodGet, "/v2/", "bob", "bobpw", true)
	if _, ok, _ := a.Authenticate(r); !ok {
		t.Fatal("bob should authenticate after Reload")
	}

	found := false
	for _, u := range a.Usernames() {
		if u == "bob" {
			found = true
		}
	}
	if !found {
		t.Errorf("Usernames() = %v, want it to contain bob", a.Usernames())
	}
}

func TestChallengeHeader(t *testing.T) {
	a := standardAuth(t)
	w := httptest.NewRecorder()
	a.Challenge(w)

	if got, want := w.Header().Get("WWW-Authenticate"), `Basic realm="rms"`; got != want {
		t.Errorf("WWW-Authenticate = %q, want %q", got, want)
	}
}

func TestDeniedError(t *testing.T) {
	d := forbidden("nope")
	if !errors.Is(d, d) {
		t.Fatal("sanity")
	}
	if got := d.Error(); !strings.Contains(got, "403") || !strings.Contains(got, "nope") {
		t.Errorf("Error() = %q", got)
	}
}

func TestHashPasswordRejectsEmpty(t *testing.T) {
	if _, err := HashPassword(""); err == nil {
		t.Error("expected an error for an empty password")
	}
}
