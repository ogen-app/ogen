package models

import (
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

// PluginPairingTTL is how long a plugin has to be approved in the browser and
// to collect its token.
const PluginPairingTTL = 10 * time.Minute

// PluginPairingStatus is where a pairing is in the handshake. Expiry is not a
// status: a row past ExpiresAt is treated as gone whatever its status.
type PluginPairingStatus = string

const (
	PluginPairingPending  PluginPairingStatus = "pending"
	PluginPairingApproved PluginPairingStatus = "approved"
	PluginPairingDenied   PluginPairingStatus = "denied"
)

// PluginPairing is the short-lived handshake that hands a plugin its token.
// The plugin keeps the read key and polls with it; the write key goes to the
// browser approval page. Approval creates the PluginToken and seals its
// plaintext into SealedToken until the plugin collects it, which deletes the
// row. Not tenant-scoped: it exists before a workspace is chosen.
type PluginPairing struct {
	bun.BaseModel `bun:"table:plugin_pairings,alias:pp" swaggerignore:"true"`

	ID           string    `bun:"id,pk"`
	Client       string    `bun:"client,notnull"`
	ClientLabel  string    `bun:"client_label,notnull"`
	ReadKeyHash  string    `bun:"read_key_hash,notnull"`
	WriteKeyHash string    `bun:"write_key_hash,notnull"`
	Status       string    `bun:"status,notnull,default:'pending'"`
	TenantID     *string   `bun:"tenant_id"`
	TokenID      *string   `bun:"token_id"`
	SealedToken  []byte    `bun:"sealed_token,nullzero"`
	CreatedIP    string    `bun:"created_ip,notnull,default:''"`
	CreatedAt    time.Time `bun:"created_at,notnull,default:current_timestamp"`
	ExpiresAt    time.Time `bun:"expires_at,notnull"`
}

// Expired reports whether the pairing is past its TTL at now.
func (p *PluginPairing) Expired(now time.Time) bool {
	return !now.Before(p.ExpiresAt)
}

// PluginPairingKeys is a fresh read/write key pair with the hashes to store.
type PluginPairingKeys struct {
	ReadKey, ReadKeyHash   string
	WriteKey, WriteKeyHash string
}

// NewPluginPairingKeys mints the two independent 32-byte keys of a pairing.
func NewPluginPairingKeys() (PluginPairingKeys, error) {
	read, err := newSecret()
	if err != nil {
		return PluginPairingKeys{}, fmt.Errorf("generate pairing read key: %w", err)
	}
	write, err := newSecret()
	if err != nil {
		return PluginPairingKeys{}, fmt.Errorf("generate pairing write key: %w", err)
	}
	return PluginPairingKeys{
		ReadKey: read, ReadKeyHash: HashPluginSecret(read),
		WriteKey: write, WriteKeyHash: HashPluginSecret(write),
	}, nil
}
