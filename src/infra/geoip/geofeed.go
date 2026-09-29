package geoip

import (
	"cmp"
	"encoding/csv"
	"errors"
	"io"
	"net/netip"
	"os"
	"slices"
	"strings"

	"golang.org/x/text/language"
	"golang.org/x/text/language/display"
)

// feedTable is a longest-prefix-match table over RFC 8805 geofeeds
// (ip_prefix,alpha2code,region,city,postal_code). Anonymizing relays such as
// Cloudflare WARP and iCloud Private Relay publish one to place each egress
// address in the city of the users behind it, at a granularity the Lite City
// database does not carry. IPv4 prefixes are stored IPv4-mapped so one table
// serves both families. Keys hold no pointers, so the ~400k entries stay off
// the GC's scan path.
type feedTable struct {
	levels []feedLevel // longest prefix first
	labels []string
}

// feedLevel holds every prefix of one length, sorted by masked address.
type feedLevel struct {
	bits   int // in IPv6 terms: an IPv4 /32 is a /128
	keys   []feedKey
	labels []uint32 // index into feedTable.labels, parallel to keys
}

type feedKey struct{ hi, lo uint64 }

func (k feedKey) compare(o feedKey) int {
	if c := cmp.Compare(k.hi, o.hi); c != 0 {
		return c
	}
	return cmp.Compare(k.lo, o.lo)
}

func keyOf(a netip.Addr) feedKey {
	b := a.As16()
	var k feedKey
	for i := range 8 {
		k.hi = k.hi<<8 | uint64(b[i])
		k.lo = k.lo<<8 | uint64(b[8+i])
	}
	return k
}

// lookup returns the label of the longest prefix containing addr, which must
// already be unmapped. ok is false when no prefix matches; a matching entry
// may still carry an empty label when its feed publishes no country.
func (t *feedTable) lookup(addr netip.Addr) (string, bool) {
	a16 := netip.AddrFrom16(addr.As16())
	for i := range t.levels {
		lv := &t.levels[i]
		p, err := a16.Prefix(lv.bits)
		if err != nil {
			continue
		}
		if j, found := slices.BinarySearchFunc(lv.keys, keyOf(p.Addr()), feedKey.compare); found {
			return t.labels[lv.labels[j]], true
		}
	}
	return "", false
}

// feedStats reports one feed file's load for the startup log.
type feedStats struct {
	rows, skipped int
}

type feedEntry struct {
	bits  int
	key   feedKey
	label uint32
}

// feedBuilder accumulates entries from one or more feeds. When feeds repeat a
// prefix, the first one added wins.
type feedBuilder struct {
	entries []feedEntry
	labels  []string
	index   map[string]uint32
	regions display.Namer
}

func newFeedBuilder() *feedBuilder {
	return &feedBuilder{index: map[string]uint32{}, regions: display.English.Regions()}
}

func (b *feedBuilder) addFile(path string) (feedStats, error) {
	f, err := os.Open(path)
	if err != nil {
		return feedStats{}, err
	}
	defer func() { _ = f.Close() }()
	return b.add(f)
}

// add reads one feed. Rows that do not parse are counted and skipped rather
// than failing the whole feed.
func (b *feedBuilder) add(r io.Reader) (feedStats, error) {
	cr := csv.NewReader(r)
	cr.Comment = '#'
	cr.FieldsPerRecord = -1
	cr.ReuseRecord = true
	var st feedStats
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return st, nil
		}
		if _, ok := errors.AsType[*csv.ParseError](err); ok {
			st.skipped++
			continue
		}
		if err != nil {
			return st, err
		}
		if !b.addRecord(rec) {
			st.skipped++
			continue
		}
		st.rows++
	}
}

func (b *feedBuilder) addRecord(rec []string) bool {
	if len(rec) < 2 {
		return false
	}
	p, err := netip.ParsePrefix(strings.TrimSpace(rec[0]))
	if err != nil {
		return false
	}
	p = p.Masked()
	bits := p.Bits()
	if p.Addr().Is4() {
		bits += 96
	}
	city := ""
	if len(rec) > 3 {
		city = strings.TrimSpace(rec[3])
	}
	b.entries = append(b.entries, feedEntry{
		bits:  bits,
		key:   keyOf(netip.AddrFrom16(p.Addr().As16())),
		label: b.intern(label(city, b.countryName(rec[1]))),
	})
	return true
}

// countryName maps an ISO 3166-1 alpha-2 code to its English name, or "" when
// the code is blank or unknown.
func (b *feedBuilder) countryName(code string) string {
	code = strings.TrimSpace(code)
	if code == "" {
		return ""
	}
	r, err := language.ParseRegion(code)
	if err != nil || r.String() == "ZZ" {
		return ""
	}
	return b.regions.Name(r)
}

func (b *feedBuilder) intern(s string) uint32 {
	if i, ok := b.index[s]; ok {
		return i
	}
	i := uint32(len(b.labels))
	b.labels = append(b.labels, s)
	b.index[s] = i
	return i
}

// build returns nil when no entries were added.
func (b *feedBuilder) build() *feedTable {
	if len(b.entries) == 0 {
		return nil
	}
	// Stable, so a prefix repeated across feeds keeps the first feed's entry.
	slices.SortStableFunc(b.entries, func(x, y feedEntry) int {
		if c := cmp.Compare(y.bits, x.bits); c != 0 {
			return c
		}
		return x.key.compare(y.key)
	})
	t := &feedTable{labels: b.labels}
	for i, e := range b.entries {
		if i > 0 && e.bits == b.entries[i-1].bits && e.key == b.entries[i-1].key {
			continue
		}
		if n := len(t.levels); n == 0 || t.levels[n-1].bits != e.bits {
			t.levels = append(t.levels, feedLevel{bits: e.bits})
		}
		lv := &t.levels[len(t.levels)-1]
		lv.keys = append(lv.keys, e.key)
		lv.labels = append(lv.labels, e.label)
	}
	for i := range t.levels {
		lv := &t.levels[i]
		lv.keys = slices.Clip(lv.keys)
		lv.labels = slices.Clip(lv.labels)
	}
	b.entries = nil
	return t
}
