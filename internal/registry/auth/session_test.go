package auth

import "testing"

func TestNewSessionTokenStoresOnlyHash(t *testing.T) {
	token, hash, err := NewSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != 64 || len(hash) != 64 || token == hash {
		t.Fatalf("token/hash lengths or equality: %q %q", token, hash)
	}
	if HashToken(token) != hash {
		t.Fatalf("HashToken did not reproduce hash")
	}
}
