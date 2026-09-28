package zernio

import "github.com/ogen-app/ogen/src/infra/vendors"

// VendorZernio is the vendor slug on Zernio usage events (CON-86, the
// publisher family). The Zernio River workers build vendors.MeterEvent values
// with this name and hand them to the usage.Recorder.
const VendorZernio = "zernio"

// Operations recorded for Zernio actions. status_poll/cancel/retry are
// api_call-grade and not yet wired (a follow-up); publish + account_connect +
// account_disconnect are the high-value, fully-dimensioned events.
const (
	OpPublish           = "publish"
	OpSchedule          = "schedule"
	OpAnalyticsRefresh  = "analytics_refresh"
	OpAccountConnect    = "account_connect"
	OpAccountDisconnect = "account_disconnect"
)

// init registers the Zernio publisher vendor. It is count-only for now (empty
// price table → every event costs 0, treated as intentional rather than an
// unknown-model gap); a per-tier Zernio price map can be supplied later via
// USAGE_MODEL_PRICES.
func init() {
	vendors.Register(vendors.Descriptor{
		Name:      VendorZernio,
		Family:    vendors.FamilyPublisher,
		SecretKey: "zernio_api_key", // must match secrets.NameZernioAPIKey
		Metered:   true,
		Prices:    vendors.PriceTable{Version: "zernio-count-only"},
	})
}
