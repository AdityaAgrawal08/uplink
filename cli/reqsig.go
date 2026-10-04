package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"net/url"
	"strconv"

	"time"

	"filippo.io/edwards25519"
)

// ─── Request signatures (identity-bound signaling auth) ────────────────────
//
// Every identity-bearing signaling call carries a signature made with the
// device identity key — the SAME X25519 static keypair whose public half is
// claimed immutably in the room roster (hsetnx at join). The server verifies
// against that rostered pubkey, so a raw X-Uplink-Username header alone no
// longer authorizes anything: impersonation requires forging an Ed25519
// signature under the victim's key.
//
// X25519 keys cannot sign directly, so we sign with the Edwards-form of the
// SAME private scalar. The Montgomery↔Edwards birational map (RFC 7748
// §4.1, u = (1+y)/(1−y)) is a group isomorphism that sends the Edwards base
// point to u = 9, so for a = k mod L (k = the X25519 private scalar):
//
//	A = a·B  ⇒  mont(A) = (1 + y_A)/(1 − y_A) = X25519(k, 9)
//
// i.e. the u-coordinate of A IS the rostered X25519 public key. The server
// derives A from the rostered pubkey (y = (u−1)/(u+1), x with sign bit 0)
// and runs a stock Ed25519 verify. We force sign bit 0 on A by negating the
// scalar when the point's x parity is odd, so both sides always recover the
// same point.
//
// Signed string:  METHOD "|" PATH "|" TIMESTAMP_MS "|" NONCE
// Headers:        X-Uplink-Timestamp (unix ms), X-Uplink-Nonce (16 random
//                 bytes hex), X-Uplink-Sig (base64 of R‖S, 64 bytes).
//
// Replay protection is the server's job (30s timestamp window + one-time
// nonce); the client just mints a fresh nonce per request.

// signWithIdentity signs msg with the device identity key (X25519 scalar,
// used as the Ed25519 scalar mod L as described above).
func signWithIdentity(id *identityKey, msg []byte) ([]byte, error) {
	if id == nil || id.priv == nil {
		return nil, fmt.Errorf("no device identity to sign with")
	}
	k := append([]byte{}, id.priv.Bytes()...)
	// Clamp the scalar exactly like the X25519 function does internally:
	// ecdh private keys are raw random bytes (GenerateKey), and the public
	// key is computed from the CLAMPED form — so signatures must be too,
	// or mont(A) never matches the rostered pubkey.
	k[0] &= 248
	k[31] &= 127
	k[31] |= 64
	// a = k mod L: SetUniformBytes reduces its 64-byte little-endian input
	// modulo L; k‖0·2^256 reduces to exactly k mod L.
	var buf [64]byte
	copy(buf[:32], k)
	a, err := edwards25519.NewScalar().SetUniformBytes(buf[:])
	if err != nil {
		return nil, err
	}
	A := edwards25519.NewGeneratorPoint().ScalarBaseMult(a)
	if A.Bytes()[31]&0x80 == 0x80 {
		// Force the sign bit to 0 (the server's decompression of u fixes
		// sign bit 0): negating keeps mont(A) == u, flips the sign bit.
		a.Negate(a)
		A.Negate(A)
	}
	// Deterministic nonce from a secret prefix of the key:
	// r = SHA512(SHA512(k)[32:64] ‖ M) mod L.
	h := sha512.Sum512(k)
	rb := sha512.Sum512(append(append([]byte{}, h[32:]...), msg...))
	r, err := edwards25519.NewScalar().SetUniformBytes(rb[:])
	if err != nil {
		return nil, err
	}
	R := edwards25519.NewGeneratorPoint().ScalarBaseMult(r)
	// hram = SHA512(R ‖ A ‖ M) mod L;  S = r + hram·a mod L.
	hr := sha512.Sum512(append(append(append([]byte{}, R.Bytes()...), A.Bytes()...), msg...))
	hram, err := edwards25519.NewScalar().SetUniformBytes(hr[:])
	if err != nil {
		return nil, err
	}
	S := edwards25519.NewScalar().Multiply(hram, a)
	S.Add(S, r)
	return append(R.Bytes(), S.Bytes()...), nil
}

// --- Montgomery → Edwards conversion (shared by the Go verifier) -----------

var (
	edwardsP      = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
	edwardsD      = new(big.Int)
	edwardsSqrtM1 = new(big.Int)
)

func init() {
	// d = -121665 / 121666 mod p
	edwardsD.Mod(new(big.Int).Mul(big.NewInt(-121665), new(big.Int).ModInverse(big.NewInt(121666), edwardsP)), edwardsP)
	// sqrt(-1) = 2^((p-1)/4)
	edwardsSqrtM1.Exp(big.NewInt(2), new(big.Int).Rsh(new(big.Int).Sub(edwardsP, big.NewInt(1)), 2), edwardsP)
}

func leDecode(b []byte) *big.Int {
	rev := append([]byte{}, b...)
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return new(big.Int).SetBytes(rev)
}

func leEncode(x *big.Int, n int) []byte {
	out := x.FillBytes(make([]byte, n))
	for i, j := 0, n-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// x25519PubkeyToEdwards converts a 32-byte little-endian Montgomery
// u-coordinate (the roster pubkey) into the compressed Ed25519 public key
// (sign bit 0) that the matching device key signs with. The signer forces
// even x-parity, so this deterministic derivation always matches.
func x25519PubkeyToEdwards(uBlob []byte) ([]byte, error) {
	if len(uBlob) != 32 {
		return nil, fmt.Errorf("x25519 pubkey must be 32 bytes")
	}
	p := edwardsP
	u := leDecode(uBlob)
	if u.Cmp(p) >= 0 || u.Sign() < 0 {
		return nil, fmt.Errorf("x25519 pubkey out of range")
	}
	// y = (u - 1) / (u + 1) mod p
	den := new(big.Int).Add(u, big.NewInt(1))
	den.Mod(den, p)
	if den.Sign() == 0 {
		return nil, fmt.Errorf("x25519 pubkey maps to the point at infinity")
	}
	y := new(big.Int).Mul(new(big.Int).Sub(u, big.NewInt(1)), new(big.Int).ModInverse(den, p))
	y.Mod(y, p)
	// x² = (y² - 1) / (d·y² + 1) mod p
	y2 := new(big.Int).Mul(y, y)
	y2.Mod(y2, p)
	num := new(big.Int).Sub(y2, big.NewInt(1))
	num.Mod(num, p)
	den2 := new(big.Int).Mul(edwardsD, y2)
	den2.Add(den2, big.NewInt(1))
	den2.Mod(den2, p)
	if den2.Sign() == 0 {
		return nil, fmt.Errorf("x25519 pubkey not on the edwards curve")
	}
	x2 := new(big.Int).Mul(num, new(big.Int).ModInverse(den2, p))
	x2.Mod(x2, p)
	// sqrt mod p (p ≡ 5 mod 8): x = x2^((p+3)/8); if that isn't a root,
	// multiply by sqrt(-1).
	x := new(big.Int).Exp(x2, new(big.Int).Rsh(new(big.Int).Add(p, big.NewInt(3)), 3), p)
	if new(big.Int).Mod(new(big.Int).Mul(x, x), p).Cmp(x2) != 0 {
		x.Mul(x, edwardsSqrtM1)
		x.Mod(x, p)
		if new(big.Int).Mod(new(big.Int).Mul(x, x), p).Cmp(x2) != 0 {
			return nil, fmt.Errorf("x25519 pubkey has no edwards square root")
		}
	}
	if x.Bit(0) == 1 {
		x.Sub(p, x) // even parity (sign bit 0)
	}
	out := leEncode(y, 32)
	if x.Bit(0) == 1 {
		out[31] |= 0x80 // x is even by construction; belt and braces
	}
	return out, nil
}

// signatureMsg builds the exact string signed over for one request.
func signatureMsg(method, path, timestamp, nonce string) []byte {
	return []byte(method + "|" + path + "|" + timestamp + "|" + nonce)
}

// verifyRequestSigAgainstX25519 verifies sig over msg against the X25519
// pubkey u (roster-anchored). Returns false on any malformation or failure.
func verifyRequestSigAgainstX25519(u []byte, msg, sig []byte) bool {
	if len(sig) != 64 || len(u) != 32 {
		return false
	}
	A, err := x25519PubkeyToEdwards(u)
	if err != nil {
		return false
	}
	return ed25519.Verify(A, msg, sig)
}

// requestSigWindowMs is the server's accepted skew for X-Uplink-Timestamp.
// The Go client mints fresh timestamps, so this only bounds retries.
const requestSigWindowMs = 30_000

// sigHeaders builds the identity headers for one signaling call: the
// username plus a fresh timestamp/nonce and the signature over
// METHOD|PATH|TIMESTAMP|NONCE. PATH is the request path WITHOUT the query
// string (the server signs req.nextUrl.pathname, which also excludes it).
// Callers pass the same full URL they hand to postJSON/getJSON.
func (c *signalClient) sigHeaders(method, rawURL string) map[string]string {
	h := map[string]string{"X-Uplink-Username": c.me}
	if c.id == nil {
		return h // no identity: call will 401 on any signature-required route
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return h
	}
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	nb := make([]byte, 16)
	if _, err := rand.Read(nb); err != nil {
		return h
	}
	nonce := hex.EncodeToString(nb)
	sig, err := signWithIdentity(c.id, signatureMsg(method, u.Path, ts, nonce))
	if err != nil {
		return h
	}
	h["X-Uplink-Timestamp"] = ts
	h["X-Uplink-Nonce"] = nonce
	h["X-Uplink-Sig"] = base64.StdEncoding.EncodeToString(sig)
	return h
}

// sigHeadersWithPubkey is sigHeaders plus the caller's advertised pubkey —
// only needed by invite-scoped calls (invites/mine, invites/decline) where
// the caller has no roster entry yet and the FIRST use claims the username
// to a device key (server-side immutable hsetnx claim, same trust pattern
// as the roster).
func (c *signalClient) sigHeadersWithPubkey(method, rawURL string) map[string]string {
	h := c.sigHeaders(method, rawURL)
	if c.id != nil {
		h["X-Uplink-Pubkey"] = pubkeyB64(c.id)
	}
	return h
}
