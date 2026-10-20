package models

import "testing"

func TestVerifyPassword(t *testing.T) {
	hash, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, password, hash string
		want                 bool
	}{
		{"match", "correct horse", hash, true},
		{"mismatch", "wrong", hash, false},
		{"no password set", "", "", false},
		{"no password set, any input", "correct horse", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := VerifyPassword(tc.password, tc.hash)
			if err != nil || ok != tc.want {
				t.Fatalf("VerifyPassword = %v, %v; want %v, nil", ok, err, tc.want)
			}
		})
	}
	if (&Account{}).HasPassword() || !(&Account{PasswordHash: hash}).HasPassword() {
		t.Fatal("HasPassword must follow the hash")
	}
}
