package zernio

import "fmt"

// Settings keys used by the integration. All keys live under the
// "zernio." prefix so the metadata cache (Phase 4) can detect them with
// a single prefix check on writes.
const (
	SettingProfileID        = "zernio.profile_id"
	SettingProfileName      = "zernio.profile_name"
	SettingProfileCreatedAt = "zernio.profile_created_at"
	SettingProfileMeta      = "zernio.profile_meta"
	SettingLastSyncAt       = "zernio.last_sync_at"
	SettingLastSyncStatus   = "zernio.last_sync_status"
	// SettingConnectInitiatedAt (RFC3339) records the first time a tenant
	// issued a Zernio connect-link. Eager profile provisioning means
	// every tenant has a zernio.profile_id from signup, so profile presence no
	// longer marks "this tenant uses Zernio". The sync worker enumerates tenants
	// to sweep by THIS key instead, so freshly-signed-up tenants that never
	// connected aren't swept.
	SettingConnectInitiatedAt = "zernio.connect_initiated_at"
	// Analytics refresh health. These live in the Zernio
	// adapter namespace, so they stay `zernio.`-prefixed even though the
	// post-side columns are publisher-agnostic.
	SettingAnalyticsLastRefreshAt     = "zernio.analytics.last_refresh_at"
	SettingAnalyticsLastRefreshStatus = "zernio.analytics.last_refresh_status"
	// Follower-stats refresh health, mirroring the analytics
	// keys above for the daily follower snapshot sweep.
	SettingFollowersLastRefreshAt     = "zernio.followers.last_refresh_at"
	SettingFollowersLastRefreshStatus = "zernio.followers.last_refresh_status"
)

// SettingPrefix is the namespace shared by every Zernio-managed key.
// Used by the cache invalidator to recognise Zernio writes.
const SettingPrefix = "zernio."

// ManagedProfileName is the canonical name written to Zernio when Ogen
// creates the profile, and the fallback lookup key when no profile ID
// is stored in settings. An admin renaming the profile on the Zernio
// dashboard breaks the name-based fallback — see the bootstrap
// concurrent-boot note for the mitigation.
const (
	ManagedProfileName        = "Ogen integration"
	ManagedProfileDescription = "Auto-managed by Ogen — do not rename or delete"
)

// defaultProfileEnv is the ZERNIO_ENV fallback baked into a profile name when no
// environment is configured (mirrors config's default). Keeps a misconfigured
// empty env from producing a malformed "Ogen--<tenant_id>" name.
const defaultProfileEnv = "dev"

// ManagedProfileNameFor returns a tenant's Zernio profile name,
// "Ogen-<env>-<tenant_id>", where env is ZERNIO_ENV (e.g. "dev",
// "prod"). Profiles created under an older naming scheme are resolved by their
// stored zernio.profile_id and are deliberately left unrenamed.
func ManagedProfileNameFor(env, tenantID string) string {
	if env == "" {
		env = defaultProfileEnv
	}
	return fmt.Sprintf("Ogen-%s-%s", env, tenantID)
}
