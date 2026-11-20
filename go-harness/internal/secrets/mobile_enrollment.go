package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"strings"
)

// One ephemeral key per pending ACP approval. Only the public form can be replayed
// to a reconnecting client. Neither ciphertext nor plaintext is a tool argument.
const enrollmentProtocol = "DeerFlow service credential v1"
const enrollmentProtocolV2 = "DeerFlow service credential v2"

var errEnrollment = errors.New("invalid or expired credential reply; request a new approval")

type EnrollmentForm struct {
	CredentialScope
	Version        int      `json:"version"`
	PublicKey      []byte   `json:"publicKey"`
	RequiredFields []string `json:"requiredFields,omitempty"`
}

type Enrollment struct {
	Form    EnrollmentForm
	private *ecdh.PrivateKey
	fields  CredentialFields
}

type SealedCredential struct {
	ClientKey  []byte `json:"clientKey"`
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}

func (v *Vault) NewEnrollment(ctx context.Context, scope CredentialScope, fields CredentialFields, version int) (*Enrollment, error) {
	missing, err := v.missingCredentialFields(ctx, scope, fields)
	if err != nil || missing == 0 {
		return nil, err
	}
	form := EnrollmentForm{CredentialScope: scope, Version: version}
	switch version {
	case 1:
		if missing != BothFields {
			return nil, errors.New("this credential requires a client supporting secure field-selective enrollment (v2)")
		}
	case 2:
		form.RequiredFields = missing.labels()
	default:
		return nil, errors.New("the connected client did not negotiate secure credential enrollment; use a client supporting remote credential forms and retry")
	}
	private, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKIXPublicKey(private.PublicKey())
	if err != nil {
		return nil, err
	}
	form.PublicKey = der
	return &Enrollment{Form: form, fields: missing, private: private}, nil
}

func (f EnrollmentForm) protocol() string {
	if f.Version == 2 {
		return enrollmentProtocolV2
	}
	return enrollmentProtocol
}

func (f EnrollmentForm) associatedData() []byte {
	aad := f.protocol() + "\x00" + f.Consumer + "\x00" + f.Service + "\x00" + f.Account
	if f.Version == 2 {
		aad += "\x00" + strings.Join(f.RequiredFields, ",")
	}
	return []byte(aad)
}

func (e *Enrollment) decrypt(reply *SealedCredential) (CredentialPair, error) {
	if e == nil || reply == nil || len(reply.ClientKey) > 128 || len(reply.Nonce) != 12 ||
		len(reply.Ciphertext) < 4+1+16 || len(reply.Ciphertext) > 4+2*MaxStringBytes+16 {
		return CredentialPair{}, errEnrollment
	}
	raw, err := x509.ParsePKIXPublicKey(reply.ClientKey)
	if err != nil {
		return CredentialPair{}, errEnrollment
	}
	public, ok := raw.(*ecdsa.PublicKey)
	if !ok {
		return CredentialPair{}, errEnrollment
	}
	peer, err := public.ECDH()
	if err != nil || peer.Curve() != ecdh.P256() {
		return CredentialPair{}, errEnrollment
	}
	shared, err := e.private.ECDH(peer)
	if err != nil {
		return CredentialPair{}, errEnrollment
	}
	defer clear(shared)
	key, err := hkdf.Key(sha256.New, shared, nil, e.Form.protocol(), 32)
	if err != nil {
		return CredentialPair{}, errEnrollment
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return CredentialPair{}, errEnrollment
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return CredentialPair{}, errEnrollment
	}
	data, err := gcm.Open(nil, reply.Nonce, reply.Ciphertext, e.Form.associatedData())
	if err != nil {
		return CredentialPair{}, errEnrollment
	}
	defer clear(data)
	n := uint64(binary.BigEndian.Uint32(data[:4]))
	if n > uint64(len(data)-4) {
		return CredentialPair{}, errEnrollment
	}
	user, secret := data[4:4+n], data[4+n:]
	valid := func(field CredentialFields, value []byte) bool {
		if e.fields&field == 0 {
			return len(value) == 0
		}
		return validateValue(KindString, value) == nil
	}
	if !valid(UsernameField, user) || !valid(KeyField, secret) {
		return CredentialPair{}, errEnrollment
	}
	return CredentialPair{append([]byte(nil), user...), append([]byte(nil), secret...)}, nil
}

func (e *Enrollment) Save(ctx context.Context, v *Vault, reply *SealedCredential) error {
	pair, err := e.decrypt(reply)
	if err != nil {
		return err
	}
	defer pair.Clear()
	return v.saveCredential(ctx, e.Form.CredentialScope, e.fields, pair, "mobile_credential")
}
