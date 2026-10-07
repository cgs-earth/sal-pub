package httpsig

import (
	"bytes"
	"context"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"testing"
)

const (
	testOwner = "https://example.org/users/alice"
	testKeyID = testOwner + "#main-key"
)

// signedRequest signs a POST for https://remote.example/users/bob/inbox and
// returns it as the receiving server would see it, plus a fetcher for the key.
func signedRequest(t *testing.T, body []byte) (*http.Request, KeyFetcher) {
	t.Helper()
	privatePEM, publicPEM, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	priv, err := ParsePrivateKey(privatePEM)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ParsePublicKey(publicPEM)
	if err != nil {
		t.Fatal(err)
	}

	out, _ := http.NewRequest(http.MethodPost, "https://remote.example/users/bob/inbox?x=1", bytes.NewReader(body))
	if err := Sign(out, testKeyID, priv, body); err != nil {
		t.Fatal(err)
	}

	in := httptest.NewRequest(http.MethodPost, "/users/bob/inbox?x=1", bytes.NewReader(body))
	in.Host = "remote.example"
	in.Header = out.Header.Clone()
	in.Header.Del("Host") // Go servers move Host out of the header map into r.Host
	fetch := func(ctx context.Context, keyID string) (string, *rsa.PublicKey, error) {
		return testOwner, pub, nil
	}
	return in, fetch
}

func TestVerifyAcceptsSignedRequest(t *testing.T) {
	body := []byte(`{"type":"Follow"}`)
	r, fetch := signedRequest(t, body)

	owner, err := Verify(r, body, fetch)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if owner != testOwner {
		t.Fatalf("owner = %q, want %q", owner, testOwner)
	}
}

func TestVerifyRejectsTamperedBody(t *testing.T) {
	r, fetch := signedRequest(t, []byte(`{"type":"Follow"}`))

	if _, err := Verify(r, []byte(`{"type":"Block"}`), fetch); err == nil {
		t.Fatal("Verify accepted a tampered body")
	}
}

func TestVerifyRejectsOtherHost(t *testing.T) {
	body := []byte(`{"type":"Follow"}`)
	r, fetch := signedRequest(t, body)
	r.Host = "other.example"

	if _, err := Verify(r, body, fetch); err == nil {
		t.Fatal("Verify accepted a request signed for another host")
	}
}

func TestVerifyRejectsUnsignedRequest(t *testing.T) {
	body := []byte(`{"type":"Follow"}`)
	r, fetch := signedRequest(t, body)
	r.Header.Del("Signature")

	if _, err := Verify(r, body, fetch); err == nil {
		t.Fatal("Verify accepted an unsigned request")
	}
}
