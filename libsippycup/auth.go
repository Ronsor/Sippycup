package libsippycup

import (
	"crypto/md5"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/sergeymakinen/go-crypt"
	cryptbcrypt "github.com/sergeymakinen/go-crypt/bcrypt"
	cryptsha256 "github.com/sergeymakinen/go-crypt/sha256"
	cryptsha512 "github.com/sergeymakinen/go-crypt/sha512"
)

var errUnauthorized = errors.New("authentication failed")

// AuthIdentity holds authentication state for one request. Password is private
// credential material and is excluded from the HCL request context.
type AuthIdentity struct {
	Username   string
	Password   string
	Verified   bool
	DigestOnly bool
}

func realm(a *AuthConfig) string {
	if a != nil && a.Realm != "" {
		return a.Realm
	}

	return "sippycup"
}

func validateCredential(c *Credential) error {
	if c.Username == "" {
		return errors.New("username is required")
	}

	count := 0
	if c.Password != nil {
		count++
	}

	if c.PasswordHash != "" {
		count++
	}

	if c.Digest != nil {
		count++
	}

	if count != 1 {
		return errors.New("specify exactly one of password, password_hash, digest")
	}

	if h := c.PasswordHash; h != "" {
		var err error
		switch {
		case strings.HasPrefix(h, "$2"):
			_, _, _, err = cryptbcrypt.Params(h)
		case strings.HasPrefix(h, "$5$"):
			_, _, err = cryptsha256.Params(h)
		case strings.HasPrefix(h, "$6$"):
			_, _, err = cryptsha512.Params(h)
		default:
			return errors.New("unsupported password hash format")
		}

		if err != nil {
			return errors.New("malformed password hash")
		}
	}

	if d := c.Digest; d != nil {
		d.HA1 = strings.ToLower(d.HA1)
		size := 0
		switch strings.ToUpper(d.Algorithm) {
		case "MD5":
			size = 16
		case "SHA-256":
			size = 32
		default:
			return errors.New("digest algorithm must be MD5 or SHA-256")
		}

		b, e := hex.DecodeString(d.HA1)
		if e != nil || len(b) != size || d.Realm == "" {
			return errors.New("invalid digest HA1 or realm")
		}
	}

	return nil
}

func digestHA1(user, realm, password, alg string) string {
	b := []byte(user + ":" + realm + ":" + password)
	if strings.EqualFold(alg, "SHA-256") {
		v := sha256.Sum256(b)

		return hex.EncodeToString(v[:])
	}

	v := md5.Sum(b)

	return hex.EncodeToString(v[:])
}

func equalSecret(a, b string) bool {
	aa := sha256.Sum256([]byte(a))
	bb := sha256.Sum256([]byte(b))

	return subtle.ConstantTimeCompare(aa[:], bb[:]) == 1
}

func (c *Config) verifyBasic(a *AuthConfig, id *AuthIdentity) error {
	if a == nil || a.Mode == "none" {
		return nil
	}

	if id == nil || id.Username == "" {
		return errUnauthorized
	}

	if a.Mode == "passthrough" {
		return nil
	}

	set := c.credentialSet(a.CredentialSet)
	if set == nil {
		return errUnauthorized
	}

	for _, name := range set.Credentials {
		v := c.credential(name)
		if v.Username != id.Username {
			continue
		}

		ok := false
		if v.Password != nil {
			ok = equalSecret(*v.Password, id.Password)
		} else if v.PasswordHash != "" {
			ok = crypt.Check(v.PasswordHash, id.Password) == nil
		} else if d := v.Digest; d != nil {
			ok = equalSecret(strings.ToLower(d.HA1), digestHA1(id.Username, d.Realm, id.Password, d.Algorithm))
		}

		if ok {
			id.Verified = true

			return nil
		}

		break
	}

	return errUnauthorized
}

// httpAuth may defer missing credentials to the AudioSocket identity packet.
func (c *Config) httpAuth(n *Node, w http.ResponseWriter, r *http.Request) (*AuthIdentity, bool) {
	a := n.Auth
	if a == nil || a.Mode == "none" {
		return nil, true
	}

	if !slices.Contains(a.Sources, "http_basic") {
		return nil, true
	}

	user, pass, ok := r.BasicAuth()
	if !ok && r.Header.Get("Authorization") == "" && slices.Contains(a.Sources, "basic_auth") {
		return nil, true
	}

	id := &AuthIdentity{Username: user, Password: pass}
	if !ok || c.verifyBasic(a, id) != nil {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf("Basic realm=%q, charset=\"UTF-8\"", realm(a)))
		http.Error(w, "authentication required", http.StatusUnauthorized)

		return nil, false
	}

	return id, true
}

func (c *Config) outboundCredential(n *Node, ctx *hcl.EvalContext, in *AuthIdentity) (*Credential, error) {
	if n.Auth == nil || n.Auth.Credential == nil {
		return nil, nil
	}

	name, e := exprString(n.Auth.Credential, ctx)
	if e != nil {
		return nil, e
	}

	if name == "@request" {
		if in == nil || in.DigestOnly {
			return nil, errUnauthorized
		}

		p := in.Password

		return &Credential{Username: in.Username, Password: &p}, nil
	}

	cr := c.credential(name)
	if cr == nil {
		return nil, fmt.Errorf("unknown credential reference")
	}
	if cr.Password == nil && !(n.Type == "sip" && cr.Digest != nil) {
		return nil, errors.New("outbound authentication requires a reusable credential")
	}

	return cr, nil
}
