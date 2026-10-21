package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
)

type TokenVault struct {
	masterKey []byte // Debe ser exactamente de 32 bytes (AES-256)
}

func NewTokenVault(masterKey32Bytes string) (*TokenVault, error) {
	key := []byte(masterKey32Bytes)
	if len(key) != 32 {
		return nil, errors.New("MASTER_KEY debe tener exactamente 32 bytes para AES-256-GCM")
	}
	return &TokenVault{masterKey: key}, nil
}

func (v *TokenVault) Encrypt(plainText string) (string, error) {
	block, err := aes.NewCipher(v.masterKey)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	cipherText := gcm.Seal(nonce, nonce, []byte(plainText), nil)
	return base64.StdEncoding.EncodeToString(cipherText), nil
}

func (v *TokenVault) Decrypt(encodedCipherText string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(encodedCipherText)
	if err != nil {
		return "", err
	}

	block, err := aes.NewCipher(v.masterKey)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", errors.New("ciphertext demasiado corto")
	}

	nonce, cipherText := data[:nonceSize], data[nonceSize:]
	plainBytes, err := gcm.Open(nil, nonce, cipherText, nil)
	if err != nil {
		return "", err
	}

	return string(plainBytes), nil
}
