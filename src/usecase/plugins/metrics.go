package plugins

import "expvar"

// Counters exposed at /debug/vars.
var (
	PairingsStarted    = expvar.NewInt("ogen_plugin_pairings_started")
	PairingsApproved   = expvar.NewInt("ogen_plugin_pairings_approved")
	PairingsDenied     = expvar.NewInt("ogen_plugin_pairings_denied")
	PairingsCollected  = expvar.NewInt("ogen_plugin_pairings_collected")
	ConnectionsRevoked = expvar.NewInt("ogen_plugin_connections_revoked")
	ImagesSent         = expvar.NewInt("ogen_plugin_images_sent")
	ImagesDeduplicated = expvar.NewInt("ogen_plugin_images_deduplicated")
	VideosSent         = expvar.NewInt("ogen_plugin_videos_sent")
	VideosRejected     = expvar.NewInt("ogen_plugin_videos_rejected")
)
