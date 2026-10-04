// Package identity implements interweb addresses and signed version records.
package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"

	"github.com/anacrolix/dht/v2/bep44"
	"github.com/anacrolix/torrent/bencode"
)

type Identity struct {
	Key  string `json:"key"`
	Salt string `json:"salt,omitempty"`
	Hash string `json:"hash,omitempty"`
}
type Record struct {
	Sequence  int64  `json:"sequence"`
	Hash      string `json:"hash"`
	Signature []byte `json:"signature"`
}
type pointer struct {
	Hash string `bencode:"ih"`
}

func ParseMagnet(raw string) (Identity, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "magnet" {
		return Identity{}, errors.New("enter a magnet link")
	}
	q := u.Query()
	if xs := q.Get("xs"); strings.HasPrefix(xs, "urn:btpk:") {
		key := strings.TrimPrefix(xs, "urn:btpk:")
		b, e := hex.DecodeString(key)
		if e != nil || len(b) != 32 {
			return Identity{}, errors.New("invalid signer key")
		}
		salt, e := hex.DecodeString(q.Get("s"))
		if e != nil || len(salt) > 64 {
			return Identity{}, errors.New("invalid salt")
		}
		return Identity{Key: hex.EncodeToString(b), Salt: hex.EncodeToString(salt)}, nil
	}
	if !strings.HasPrefix(q.Get("xt"), "urn:btih:") {
		return Identity{}, errors.New("expected a btih magnet")
	}
	h := strings.TrimPrefix(q.Get("xt"), "urn:btih:")
	b, e := hex.DecodeString(h)
	if e != nil || len(b) != 20 {
		return Identity{}, errors.New("expected a signed site magnet or hexadecimal btih magnet")
	}
	return Identity{Hash: hex.EncodeToString(b)}, nil
}
func (i Identity) ID() string {
	if i.Key == "" {
		return i.Hash
	}
	b, _ := hex.DecodeString(i.Key + i.Salt)
	h := sha1.Sum(b)
	return hex.EncodeToString(h[:])
}
func (i Identity) Magnet() string {
	if i.Key == "" {
		return "magnet:?xt=urn:btih:" + i.Hash
	}
	m := "magnet:?xs=urn:btpk:" + i.Key
	if i.Salt != "" {
		m += "&s=" + i.Salt
	}
	return m
}
func (i Identity) Target() [20]byte { b, _ := hex.DecodeString(i.Key + i.Salt); return sha1.Sum(b) }
func (i Identity) Put(r Record) bep44.Put {
	kb, _ := hex.DecodeString(i.Key)
	var k [32]byte
	copy(k[:], kb)
	salt, _ := hex.DecodeString(i.Salt)
	hb, _ := hex.DecodeString(r.Hash)
	p := bep44.Put{K: &k, Salt: salt, Seq: r.Sequence, V: pointer{string(hb)}}
	copy(p.Sig[:], r.Signature)
	return p
}
func (i Identity) sign(key ed25519.PrivateKey, seq int64, hash string) Record {
	r := Record{Sequence: seq, Hash: hash}
	p := i.Put(r)
	p.Sign(key)
	r.Signature = append([]byte(nil), p.Sig[:]...)
	return r
}
func (i Identity) Verify(r Record, previous Record) error {
	if i.Key == "" {
		return errors.New("unsigned identity cannot verify a signed record")
	}
	if err := i.Validate(); err != nil {
		return err
	}
	if r.Sequence < 1 || r.Sequence < previous.Sequence {
		return errors.New("signed version rollback")
	}
	h, e := hex.DecodeString(r.Hash)
	if e != nil || len(h) != 20 || len(r.Signature) != 64 {
		return errors.New("invalid signed pointer")
	}
	p := i.Put(r)
	if !bep44.Verify(p.K[:], p.Salt, p.Seq, bencode.MustMarshal(p.V), p.Sig[:]) {
		return errors.New("invalid site signature")
	}
	if r.Sequence == previous.Sequence && previous.Hash != "" && r.Hash != previous.Hash {
		return errors.New("conflicting signed version")
	}
	return nil
}

func (i Identity) Validate() error {
	if i.Key != "" && i.Hash != "" {
		return errors.New("identity cannot be both signed and fixed")
	}
	parsed, err := ParseMagnet(i.Magnet())
	if err != nil {
		return err
	}
	if parsed != i {
		return errors.New("identity is not canonical")
	}
	return nil
}
func (r Record) Clone() Record { r.Signature = append([]byte(nil), r.Signature...); return r }

// Signer holds signing capability, separate from storage encryption.
type Signer struct {
	key     ed25519.PrivateKey
	address Identity
}

// NewSigner copies a caller-owned key and restores its identity.
func NewSigner(key ed25519.PrivateKey, salt ...byte) (*Signer, error) {
	if len(key) != ed25519.PrivateKeySize || len(salt) > 64 {
		return nil, errors.New("invalid signer key or salt")
	}
	if !bytes.Equal(key, ed25519.NewKeyFromSeed(key[:ed25519.SeedSize])) {
		return nil, errors.New("inconsistent signer key")
	}
	pub := key.Public().(ed25519.PublicKey)
	return &Signer{key: append(ed25519.PrivateKey(nil), key...), address: Identity{Key: hex.EncodeToString(pub), Salt: hex.EncodeToString(salt)}}, nil
}
func (p *Signer) Identity() Identity { return p.address }
func (p *Signer) Sign(sequence int64, hash string) (Record, error) {
	b, err := hex.DecodeString(hash)
	if err != nil || len(b) != 20 || sequence <= 0 {
		return Record{}, errors.New("invalid version hash or sequence")
	}
	return p.address.sign(p.key, sequence, hex.EncodeToString(b)), nil
}
