package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
)

func TestSignedIdentityAndVersions(t *testing.T) {
	i, err := ParseMagnet("magnet:?xs=urn:btpk:8543d3e6115f0f98c944077a4493dcd543e49c739fd998550a1f614ab36ed63e")
	if err != nil || i.ID() != "cc3f9d90b572172053626f9980ce261a850d050b" {
		t.Fatal("BEP46 vector", err)
	}
	i.Salt = "6e"
	if i.ID() != "59ee7c2cb9b4f7eb1986ee2d18fd2fdb8a56554f" {
		t.Fatal("salted vector")
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewSigner(key)
	if err != nil {
		t.Fatal(err)
	}
	clear(key)
	first, err := p.Sign(1, strings.Repeat("a", 40))
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.Sign(2, strings.Repeat("b", 40))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Identity().Verify(second, first); err != nil {
		t.Fatal(err)
	}
	if p.Identity().Verify(first, second) == nil {
		t.Fatal("accepted rollback")
	}
	conflict, _ := p.Sign(2, strings.Repeat("c", 40))
	if p.Identity().Verify(conflict, second) == nil {
		t.Fatal("accepted conflict")
	}
	second.Signature[0] ^= 1
	if p.Identity().Verify(second, first) == nil {
		t.Fatal("accepted forgery")
	}
	for _, raw := range []string{"https://example.com", "magnet:?xs=urn:btpk:abc", "magnet:?xt=" + strings.Repeat("a", 40), "magnet:?xt=urn:btih:../bad"} {
		if _, err := ParseMagnet(raw); err == nil {
			t.Fatal("accepted", raw)
		}
	}
	fixed, err := ParseMagnet("magnet:?xt=urn:btih:" + strings.Repeat("a", 40))
	if err != nil {
		t.Fatal(err)
	}
	if fixed.Verify(first, Record{}) == nil {
		t.Fatal("unsigned identity claimed authentication")
	}
}
