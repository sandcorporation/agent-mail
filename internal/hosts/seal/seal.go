// Package seal holds the host side of secret delivery: an anonymous sealed
// box (X25519, compatible with libsodium crypto_box_seal), key files, and
// the role env file the decrypted values land in.
//
// Errors from this package never contain key material or secret values.
package seal

import (
	"crypto/rand"
	"errors"

	"golang.org/x/crypto/nacl/box"
)

// KeySize is the length of public and private keys.
const KeySize = 32

// Overhead is how much longer a sealed box is than its plaintext.
const Overhead = box.AnonymousOverhead

// ErrOpen is returned when a sealed box cannot be opened: wrong key,
// tampered or truncated ciphertext.
var ErrOpen = errors.New("seal: cannot open sealed box")

// GenerateKey returns a new X25519 key pair.
func GenerateKey() (pub, priv [KeySize]byte, err error) {
	p, s, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return pub, priv, errors.New("seal: generate key: " + err.Error())
	}
	return *p, *s, nil
}

// Seal encrypts plaintext so only the holder of pub's private key can read
// it. The sender stays anonymous; the output is crypto_box_seal compatible.
func Seal(pub [KeySize]byte, plaintext []byte) ([]byte, error) {
	ct, err := box.SealAnonymous(nil, plaintext, &pub, rand.Reader)
	if err != nil {
		return nil, errors.New("seal: seal: " + err.Error())
	}
	return ct, nil
}

// Open decrypts a sealed box made for pub.
func Open(pub, priv [KeySize]byte, ct []byte) ([]byte, error) {
	msg, ok := box.OpenAnonymous(nil, ct, &pub, &priv)
	if !ok {
		return nil, ErrOpen
	}
	return msg, nil
}
