// Package auth authenticates registry clients and authorises pushes.
//
// Authentication is HTTP Basic against an htpasswd file. Basic auth is used
// because it is the one scheme both `docker login` and containerd handle
// without an extra token service.
//
// An important consequence is that a request carrying credentials must never
// fall back to anonymous access: `docker login` verifies a password by sending
// it to GET /v2/ and treating a 200 as success, so accepting anonymous on the
// same path would report "Login Succeeded" for a wrong password.
package auth

import (
	"bufio"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	htpasswd "github.com/tg123/go-htpasswd"
	"golang.org/x/crypto/bcrypt"

	"github.com/watsons/rms/internal/config"
	"github.com/watsons/rms/internal/namespace"
)

// DefaultRealm is advertised in WWW-Authenticate.
const DefaultRealm = "rms"

// Identity is an authenticated caller.
type Identity struct {
	Username string
	Admin    bool
}

// Anonym is the identity of an unauthenticated caller.
var Anonym = Identity{}

// Denied describes a refused request.
type Denied struct {
	// Status is 401 (provide credentials) or 403 (known but not allowed).
	Status  int
	Code    string
	Message string
	// Challenge asks the client to authenticate with HTTP Basic.
	Challenge bool
}

func (d *Denied) Error() string { return fmt.Sprintf("%d %s: %s", d.Status, d.Code, d.Message) }

func unauthorized(msg string) *Denied {
	return &Denied{Status: http.StatusUnauthorized, Code: "UNAUTHORIZED", Message: msg, Challenge: true}
}

func forbidden(msg string) *Denied {
	return &Denied{Status: http.StatusForbidden, Code: "DENIED", Message: msg}
}

// Options configures an Authenticator.
type Options struct {
	Realm string
	// HtpasswdFile is the bcrypt htpasswd path, empty when auth is disabled.
	HtpasswdFile string
	// Users are the htpasswd usernames, used to recognise personal namespaces.
	Users []string
	// SharedNamespaces may be pushed to by any authenticated user.
	SharedNamespaces []string
	// Admins may push and delete anywhere and read the merged catalog.
	Admins map[string]bool
	// AnonymousPullProxy allows unauthenticated pulls from proxy namespaces.
	AnonymousPullProxy bool
	// AnonymousPullLocal allows unauthenticated pulls from local namespaces.
	AnonymousPullLocal bool
}

// Authenticator verifies credentials and applies the push policy.
type Authenticator struct {
	realm string
	file  *htpasswd.File
	path  string

	mu     sync.RWMutex
	users  map[string]bool
	shared map[string]bool
	admins map[string]bool

	anonymousPullProxy bool
	anonymousPullLocal bool
}

// New builds an Authenticator. When opts.HtpasswdFile is empty authentication is
// disabled (auth.type: none) and every request is anonymous.
func New(opts Options) (*Authenticator, error) {
	if opts.Realm == "" {
		opts.Realm = DefaultRealm
	}

	a := &Authenticator{
		realm: opts.Realm,
		path:  opts.HtpasswdFile,

		users:  toSet(opts.Users),
		shared: toSet(opts.SharedNamespaces),
		admins: map[string]bool{},

		anonymousPullProxy: opts.AnonymousPullProxy,
		anonymousPullLocal: opts.AnonymousPullLocal,
	}
	for name, isAdmin := range opts.Admins {
		a.admins[name] = isAdmin
	}

	if opts.HtpasswdFile != "" {
		file, err := loadHtpasswd(opts.HtpasswdFile)
		if err != nil {
			return nil, err
		}
		a.file = file
	}

	return a, nil
}

// loadHtpasswd parses an htpasswd file. Plaintext is deliberately excluded from
// the accepted formats: a malformed line must not silently become a plaintext
// credential.
func loadHtpasswd(path string) (*htpasswd.File, error) {
	file, err := htpasswd.New(path,
		[]htpasswd.PasswdParser{
			htpasswd.AcceptBcrypt,
			htpasswd.AcceptMd5,
			htpasswd.AcceptSha,
			htpasswd.AcceptSsha,
			htpasswd.AcceptCryptSha,
		},
		func(err error) {
			// Reported by Reload's caller via the warning below.
		},
	)
	if err != nil {
		return nil, fmt.Errorf("load htpasswd %s: %w", path, err)
	}
	return file, nil
}

// Reload re-reads the htpasswd file, so adding a user does not require a
// restart.
func (a *Authenticator) Reload() error {
	if a.file == nil {
		return nil
	}
	if err := a.file.Reload(nil); err != nil {
		return fmt.Errorf("reload htpasswd: %w", err)
	}

	users, err := Usernames(a.path)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.users = toSet(users)
	a.mu.Unlock()
	return nil
}

// Usernames returns the htpasswd usernames.
func (a *Authenticator) Usernames() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]string, 0, len(a.users))
	for u := range a.users {
		out = append(out, u)
	}
	return out
}

// Enabled reports whether authentication is configured.
func (a *Authenticator) Enabled() bool { return a.file != nil }

// Authenticate inspects the request's credentials.
//
// It returns the identity and whether the credentials were present and valid.
// A request with present-but-invalid credentials reports ok=false with
// present=true, which callers must treat as a hard 401 rather than a reason to
// fall back to anonymous access.
func (a *Authenticator) Authenticate(r *http.Request) (id Identity, ok, present bool) {
	user, pass, hasAuth := basicAuth(r)
	if !hasAuth {
		return Anonym, false, false
	}

	if a.file == nil {
		// Authentication is disabled: accept the presented identity so that
		// audit logs still show who pushed.
		return Identity{Username: user, Admin: false}, true, true
	}

	if !a.file.Match(user, pass) {
		return Anonym, false, true
	}

	a.mu.RLock()
	isAdmin := a.admins[user]
	a.mu.RUnlock()

	return Identity{Username: user, Admin: isAdmin}, true, true
}

// Decide applies the access policy for a resolved request.
//
// id/ok/present come from Authenticate. A nil return means the request is
// allowed.
func (a *Authenticator) Decide(r *http.Request, res namespace.Resolution, id Identity, ok, present bool) *Denied {
	write := isWrite(r.Method)

	// Credentials were supplied and rejected: never fall through to anonymous
	// access, otherwise docker login would report success for a bad password.
	if present && !ok {
		return unauthorized("invalid username or password")
	}

	// Authentication switched off means there is no identity to authorise
	// against, so the registry is open. Proxy namespaces stay read-only
	// regardless: that is a property of the namespace, not of the caller.
	if !a.Enabled() {
		if write && res.Kind == namespace.KindRepository && res.Mode == config.ModeProxy {
			return forbidden(fmt.Sprintf(
				"namespace %q is a read-only cache of an upstream registry", res.Namespace))
		}
		return nil
	}

	switch res.Kind {
	case namespace.KindBase:
		// The API root always challenges when authentication is enabled, even
		// though anonymous content reads may be allowed further in.
		//
		// Docker's client is challenge-driven: it only learns that a host wants
		// credentials from a 401. Answering 200 here has two consequences, both
		// observed against a live daemon:
		//
		//   - `docker login` never verifies the password, because it only sends
		//     the credentials to a challenged API root. A wrong password reports
		//     "Login Succeeded".
		//   - A later push omits the stored credentials, so its blob HEAD is
		//     answered 401 and the push fails with "unexpected status 401".
		//
		// The 401 only advertises the scheme; anonymous access is still granted
		// per namespace on the content requests themselves, which is what keeps
		// anonymous pulls of public cache namespaces working.
		if ok {
			return nil
		}
		return unauthorized("authentication required")

	case namespace.KindCatalog:
		switch {
		case ok && id.Admin:
			return nil
		case ok:
			return forbidden("the catalog requires an administrator account")
		default:
			return unauthorized("authentication required")
		}

	case namespace.KindRepository:
		if !write {
			if ok {
				return nil
			}
			switch res.Mode {
			case config.ModeProxy:
				if a.anonymousPullProxy {
					return nil
				}
			case config.ModeLocal:
				if a.anonymousPullLocal {
					return nil
				}
			}
			return unauthorized("authentication required")
		}

		// Write.
		if res.Mode == config.ModeProxy {
			return forbidden(fmt.Sprintf(
				"namespace %q is a read-only cache of an upstream registry; push to your own namespace instead, for example %s/<image>",
				res.Namespace, a.personalNamespaceHint(id, res)))
		}
		if !ok {
			return unauthorized("authentication required")
		}
		if id.Admin {
			return nil
		}
		// An explicitly configured namespace wins over a same-named user:
		// listing a name in namespaces.local or auth.shared_namespaces is a
		// deliberate statement that it is shared, and a user account silently
		// making it private to that account would be surprising.
		if a.isShared(res.Namespace) {
			return nil
		}
		if a.isPersonal(res.Namespace) {
			if id.Username == res.Namespace {
				return nil
			}
			return forbidden(fmt.Sprintf("namespace %q belongs to another user; push to %s/<image> instead",
				res.Namespace, id.Username))
		}
		return forbidden(fmt.Sprintf("namespace %q is not writable", res.Namespace))
	}

	return nil
}

// personalNamespaceHint returns the namespace the caller should push to.
func (a *Authenticator) personalNamespaceHint(id Identity, res namespace.Resolution) string {
	if id.Username != "" {
		return id.Username
	}
	return "your-username"
}

func (a *Authenticator) isPersonal(ns string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.users[ns]
}

func (a *Authenticator) isShared(ns string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.shared[ns]
}

// Challenge writes the WWW-Authenticate header for a 401.
func (a *Authenticator) Challenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf("Basic realm=%q", a.realm))
}

// Realm returns the configured realm.
func (a *Authenticator) Realm() string { return a.realm }

// isWrite reports whether a method mutates registry state.
func isWrite(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func basicAuth(r *http.Request) (user, pass string, ok bool) {
	// r.BasicAuth follows RFC 7617 and rejects malformed headers.
	return r.BasicAuth()
}

func toSet(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		if n != "" {
			out[n] = true
		}
	}
	return out
}

// Usernames lists the users in an htpasswd file.
//
// The htpasswd library verifies passwords but does not enumerate users, and rms
// needs the list to derive personal namespaces. The file format is one
// "user:hash" entry per line, with blank lines and "#" comments ignored.
func Usernames(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read htpasswd %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var out []string
	seen := map[string]bool{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		user, _, found := strings.Cut(line, ":")
		user = strings.TrimSpace(user)
		if !found || user == "" || seen[user] {
			continue
		}
		seen[user] = true
		out = append(out, user)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read htpasswd %s: %w", path, err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("htpasswd %s contains no users", path)
	}
	return out, nil
}

// HashPassword produces a bcrypt htpasswd entry, as `htpasswd -B` would.
func HashPassword(password string) (string, error) {
	if len(password) == 0 {
		return "", errors.New("password must not be empty")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(hash), nil
}
