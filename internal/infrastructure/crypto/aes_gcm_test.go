package crypto

import (
	"testing"
)

func TestTokenVault_EncryptDecrypt(t *testing.T) {
	// Clave inválida (menos de 32 bytes) debe fallar
	if _, err := NewTokenVault("short-key"); err == nil {
		t.Fatalf("esperaba error con clave menor a 32 bytes")
	}

	vault, err := NewTokenVault("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("error inesperado creando TokenVault: %v", err)
	}

	secretToken := "APP_USR-84920194829104-102026-abcdef1234567890-1492837465"
	enc1, err := vault.Encrypt(secretToken)
	if err != nil {
		t.Fatalf("error encriptando: %v", err)
	}
	enc2, err := vault.Encrypt(secretToken)
	if err != nil {
		t.Fatalf("error encriptando segunda vez: %v", err)
	}

	// Dos encriptaciones del mismo texto plano deben producir ciphertexts distintos gracias al nonce aleatorio
	if enc1 == enc2 {
		t.Fatalf("se esperaban ciphertexts distintos por nonce aleatorio en AES-GCM")
	}

	dec, err := vault.Decrypt(enc1)
	if err != nil {
		t.Fatalf("error desencriptando: %v", err)
	}
	if dec != secretToken {
		t.Fatalf("esperaba %q, obtuvo %q", secretToken, dec)
	}
}
