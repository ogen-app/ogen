package envelope

import (
	"encoding/json"
	"fmt"
)

// Seal encrypts plaintext into one self-contained blob (the JSON-encoded
// Record), for short-lived secrets stored in a single bytea column.
func (c *Cipher) Seal(plaintext []byte) ([]byte, error) {
	rec, err := c.Encrypt(plaintext)
	if err != nil {
		return nil, err
	}
	return json.Marshal(rec)
}

// Open reverses Seal.
func (c *Cipher) Open(sealed []byte) ([]byte, error) {
	var rec Record
	if err := json.Unmarshal(sealed, &rec); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	return c.Decrypt(rec)
}
