// Package httpsig wraps github.com/go-fed/httpsig with the policy ActivityPub
// servers (Mastodon and friends) expect: rsa-sha256 draft-cavage signatures
// over (request-target), host, date and, for requests with a body, digest.
package httpsig

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	gofed "github.com/go-fed/httpsig"
)

// MaxClockSkew is how far a request's Date header may be from now.
const MaxClockSkew = time.Hour

// KeyFetcher resolves a keyId to the public key and the URI of the actor that owns it.
type KeyFetcher func(ctx context.Context, keyID string) (owner string, key *rsa.PublicKey, err error)

// GenerateKey returns a new 2048-bit RSA key pair as PEM (PKCS#8 private, PKIX public).
func GenerateKey() (privatePEM, publicPEM string, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", fmt.Errorf("generate RSA key: %w", err)
	}

	privateDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", fmt.Errorf("marshal private key: %w", err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", "", fmt.Errorf("marshal public key: %w", err)
	}

	privatePEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}))
	publicPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}))
	return privatePEM, publicPEM, nil
}

func ParsePrivateKey(privatePEM string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(privatePEM))
	if block == nil {
		return nil, errors.New("parse private key: no PEM block")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("parse private key: not an RSA key")
	}
	return rsaKey, nil
}

// ParsePublicKey accepts PKIX ("PUBLIC KEY") and PKCS#1 ("RSA PUBLIC KEY") PEM.
func ParsePublicKey(publicPEM string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(publicPEM))
	if block == nil {
		return nil, errors.New("parse public key: no PEM block")
	}
	if block.Type == "RSA PUBLIC KEY" {
		key, err := x509.ParsePKCS1PublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse public key: %w", err)
		}
		return key, nil
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}
	rsaKey, ok := key.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("parse public key: not an RSA key")
	}
	return rsaKey, nil
}

// Sign sets the Date (and, when body is non-nil, Digest) headers on an
// outgoing request and signs it. body must be the exact bytes being sent.
func Sign(r *http.Request, keyID string, key *rsa.PrivateKey, body []byte) error {
	r.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	// go-fed signs the Host header; Go's client sends r.Host (or the URL host) instead.
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	r.Header.Set("Host", host)
	headers := []string{gofed.RequestTarget, "host", "date"}
	if body != nil {
		headers = append(headers, "digest")
	}

	// Signers aren't safe for concurrent use, so make one per request.
	signer, _, err := gofed.NewSigner([]gofed.Algorithm{gofed.RSA_SHA256}, gofed.DigestSha256, headers, gofed.Signature, 0)
	if err != nil {
		return fmt.Errorf("create signer: %w", err)
	}
	if err := signer.SignRequest(key, keyID, r, body); err != nil {
		return fmt.Errorf("sign request: %w", err)
	}
	return nil
}

var signedHeadersParam = regexp.MustCompile(`headers="([^"]*)"`)

// Verify checks the Signature header on an incoming request whose body has
// already been read into body, and returns the URI of the actor that owns the
// signing key. Beyond the signature itself it requires that the request
// target, host, date and (with a body) digest are signed, that Date is
// recent, and that Digest matches body.
func Verify(r *http.Request, body []byte, fetch KeyFetcher) (string, error) {
	verifier, err := gofed.NewVerifier(r)
	if err != nil {
		return "", fmt.Errorf("signature: %w", err)
	}

	var signed []string
	if m := signedHeadersParam.FindStringSubmatch(r.Header.Get("Signature")); m != nil {
		signed = strings.Fields(strings.ToLower(m[1]))
	}
	required := []string{gofed.RequestTarget, "host", "date"}
	if len(body) > 0 {
		required = append(required, "digest")
	}
	for _, h := range required {
		if !slices.Contains(signed, h) {
			return "", fmt.Errorf("signature: %q must be signed", h)
		}
	}

	date, err := http.ParseTime(r.Header.Get("Date"))
	if err != nil {
		return "", fmt.Errorf("signature: bad Date header: %w", err)
	}
	if skew := time.Since(date); skew > MaxClockSkew || skew < -MaxClockSkew {
		return "", errors.New("signature: Date outside allowed clock skew")
	}

	// go-fed only checks that Digest is signed, not that it matches the body.
	if len(body) > 0 {
		sum := sha256.Sum256(body)
		want := base64.StdEncoding.EncodeToString(sum[:])
		matched := false
		for _, d := range strings.Split(r.Header.Get("Digest"), ",") {
			algo, value, _ := strings.Cut(strings.TrimSpace(d), "=")
			matched = matched || (strings.EqualFold(algo, "SHA-256") && value == want)
		}
		if !matched {
			return "", errors.New("signature: Digest does not match body")
		}
	}

	owner, key, err := fetch(r.Context(), verifier.KeyId())
	if err != nil {
		return "", fmt.Errorf("signature: fetch key %s: %w", verifier.KeyId(), err)
	}
	if err := verifier.Verify(key, gofed.RSA_SHA256); err != nil {
		return "", fmt.Errorf("signature: verification failed: %w", err)
	}
	return owner, nil
}
