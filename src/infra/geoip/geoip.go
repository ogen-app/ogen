// Package geoip resolves an IP address to an approximate "City, Country" label
// from offline data: RFC 8805 geofeeds published by anonymizing relays
// (Cloudflare WARP, iCloud Private Relay), then a MaxMind-format City database
// (DB-IP City Lite or GeoLite2 City). Lookups run in-process; nothing here
// calls a network service.
package geoip

import (
	"log/slog"
	"net/netip"
	"strings"

	"github.com/oschwald/maxminddb-golang/v2"

	"github.com/ogen-app/ogen/src/kernel/logging"
)

// Locator resolves IPs to a location label. The zero value and a nil *Locator
// are disabled and return "" for every address.
type Locator struct {
	feeds  *feedTable
	reader *maxminddb.Reader
}

// cityRecord is the subset of the City schema the label needs.
type cityRecord struct {
	City struct {
		Names map[string]string `maxminddb:"names"`
	} `maxminddb:"city"`
	Country struct {
		Names map[string]string `maxminddb:"names"`
	} `maxminddb:"country"`
}

// Open loads the City database at dbPath and the geofeeds at feedPaths. Any
// source that is unset, missing or unreadable is skipped with one warning, so
// a deploy without the data still boots and alerts simply omit the location.
func Open(dbPath string, feedPaths []string) *Locator {
	return &Locator{feeds: openFeeds(feedPaths), reader: openDB(dbPath)}
}

func openDB(path string) *maxminddb.Reader {
	if strings.TrimSpace(path) == "" {
		slog.Warn("GEOIP_DB_PATH is not set: login alerts will not include a location outside geofeeds",
			logging.AttrComponent, "geoip")
		return nil
	}
	r, err := maxminddb.Open(path)
	if err != nil {
		slog.Warn("geoip database unavailable: login alerts will not include a location outside geofeeds",
			logging.AttrComponent, "geoip", "path", path, logging.AttrError, err)
		return nil
	}
	slog.Info("geoip database loaded", logging.AttrComponent, "geoip", "path", path,
		"build_epoch", r.Metadata.BuildEpoch)
	return r
}

// openFeeds loads the feeds in order; a prefix listed by several keeps the
// first feed's location.
func openFeeds(paths []string) *feedTable {
	b := newFeedBuilder()
	for _, path := range paths {
		st, err := b.addFile(path)
		if err != nil {
			slog.Warn("geofeed unavailable: its relay addresses fall back to the City database",
				logging.AttrComponent, "geoip", "path", path, logging.AttrError, err)
			continue
		}
		slog.Info("geofeed loaded", logging.AttrComponent, "geoip", "path", path,
			"rows", st.rows, "skipped", st.skipped)
	}
	return b.build()
}

// Enabled reports whether lookups can return anything.
func (l *Locator) Enabled() bool { return l != nil && (l.feeds != nil || l.reader != nil) }

// Lookup returns "City, Country", just "Country" when the city is unknown, or
// "" when the locator is disabled, the address is private/loopback/invalid, or
// no source places it. A geofeed entry wins over the City database, which
// places relay egress addresses only coarsely.
func (l *Locator) Lookup(ip string) string {
	if !l.Enabled() {
		return ""
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return ""
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return ""
	}
	if l.feeds != nil {
		if loc, ok := l.feeds.lookup(addr); ok {
			return loc
		}
	}
	if l.reader == nil {
		return ""
	}
	var rec cityRecord
	if err := l.reader.Lookup(addr).Decode(&rec); err != nil {
		return ""
	}
	return label(rec.City.Names["en"], rec.Country.Names["en"])
}

// Close releases the database.
func (l *Locator) Close() error {
	if l == nil || l.reader == nil {
		return nil
	}
	return l.reader.Close()
}

func label(city, country string) string {
	switch {
	case country == "":
		return ""
	case city == "":
		return country
	default:
		return city + ", " + country
	}
}
