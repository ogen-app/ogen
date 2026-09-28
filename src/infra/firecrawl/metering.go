package firecrawl

import "github.com/ogen-app/ogen/src/infra/vendors"

// VendorFirecrawl is the vendor slug on Firecrawl usage events (CON-222, the
// ingest family). The process_url worker builds a vendors.MeterEvent with this
// name and hands it to the usage.Recorder, one per successful scrape.
const VendorFirecrawl = "firecrawl"

// OpScrape is the operation recorded for a URL scrape.
const OpScrape = "scrape"

// init registers the Firecrawl ingest vendor. It is count-only for now (empty
// price table → every event costs 0, treated as intentional rather than an
// unknown-model gap); a per-scrape Firecrawl price can be supplied later via
// USAGE_MODEL_PRICES.
func init() {
	vendors.Register(vendors.Descriptor{
		Name:      VendorFirecrawl,
		Family:    vendors.FamilyIngest,
		SecretKey: "firecrawl_api_key", // must match secrets.NameFirecrawlAPIKey
		Metered:   true,
		Prices:    vendors.PriceTable{Version: "firecrawl-count-only"},
	})
}
