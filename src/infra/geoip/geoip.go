// Package geoip resolves an IP address to an approximate "City, Country" label
// from an offline MaxMind-format City database (DB-IP City Lite or GeoLite2
// City). Lookups run in-process; nothing here calls a network service.
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

// Open loads the database at path. An empty path, or a file that is missing or
// unreadable, yields a disabled Locator and logs one warning, so a deploy
// without the database still boots and alerts simply omit the location.
func Open(path string) *Locator {
	if strings.TrimSpace(path) == "" {
		slog.Warn("GEOIP_DB_PATH is not set: login alerts will not include a location", logging.AttrComponent, "geoip")
		return &Locator{}
	}
	r, err := maxminddb.Open(path)
	if err != nil {
		slog.Warn("geoip database unavailable: login alerts will not include a location",
			logging.AttrComponent, "geoip", "path", path, logging.AttrError, err)
		return &Locator{}
	}
	slog.Info("geoip database loaded", logging.AttrComponent, "geoip", "path", path,
		"build_epoch", r.Metadata.BuildEpoch)
	return &Locator{reader: r}
}

// Enabled reports whether lookups can return anything.
func (l *Locator) Enabled() bool { return l != nil && l.reader != nil }

// Lookup returns "City, Country", just "Country" when the city is unknown, or
// "" when the database is disabled, the address is private/loopback/invalid, or
// it is not in the database.
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
	var rec cityRecord
	if err := l.reader.Lookup(addr).Decode(&rec); err != nil {
		return ""
	}
	return label(rec.City.Names["en"], rec.Country.Names["en"])
}

// Close releases the database.
func (l *Locator) Close() error {
	if !l.Enabled() {
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
