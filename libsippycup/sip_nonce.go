package libsippycup

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"net"
	"strings"
	"time"
)

// nonceMAC binds a challenge to its issuer, realm, algorithm, and signaling IP.
// Issuing a challenge stores no state, so unauthenticated traffic cannot fill
// the replay cache. Only successfully authenticated nonces enter that cache.
func (t *sipTransport) nonceMAC(payload []byte, realm, algorithm, remote string) []byte {
	t.nonceOnce.Do(func() { _, _ = rand.Read(t.nonceKey[:]) })
	if host, _, err := net.SplitHostPort(remote); err == nil {
		remote = host
	}

	mac := hmac.New(sha256.New, t.nonceKey[:])
	mac.Write(payload)
	for _, value := range []string{realm, strings.ToUpper(algorithm), remote} {
		mac.Write([]byte{0})
		mac.Write([]byte(value))
	}

	return mac.Sum(nil)
}

func (t *sipTransport) issueNonce(realm, algorithm, remote string, now time.Time) string {
	var payload [24]byte
	binary.BigEndian.PutUint64(payload[:8], uint64(now.Add(time.Minute).Unix()))
	_, _ = rand.Read(payload[8:])
	encoded := append(payload[:], t.nonceMAC(payload[:], realm, algorithm, remote)...)

	return base64.RawURLEncoding.EncodeToString(encoded)
}

func (t *sipTransport) verifyNonce(value, realm, algorithm, remote string, now time.Time) (sipNonce, bool) {
	if len(value) != base64.RawURLEncoding.EncodedLen(24+sha256.Size) {
		return sipNonce{}, false
	}

	b, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(b) != 24+sha256.Size ||
		!hmac.Equal(b[24:], t.nonceMAC(b[:24], realm, algorithm, remote)) {
		return sipNonce{}, false
	}

	expires := time.Unix(int64(binary.BigEndian.Uint64(b[:8])), 0)
	if !expires.After(now) || expires.After(now.Add(time.Minute)) {
		return sipNonce{}, false
	}

	return sipNonce{realm: realm, algorithm: strings.ToUpper(algorithm), expires: expires}, true
}

func (t *sipTransport) consumeNonce(user, value string, expires, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	used := t.usedNonces[user]
	for nonce, until := range used {
		if !until.After(now) {
			delete(used, nonce)
		}
	}

	if _, replay := used[value]; replay || len(used) >= 4096 || !expires.After(now) {
		return false
	}

	if t.usedNonces == nil {
		t.usedNonces = make(map[string]map[string]time.Time)
	}

	if used == nil {
		used = make(map[string]time.Time)
		t.usedNonces[user] = used
	}

	used[value] = expires

	return true
}
