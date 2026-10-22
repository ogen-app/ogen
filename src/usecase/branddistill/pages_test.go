package branddistill

import (
	"slices"
	"testing"

	"github.com/ogen-app/ogen/src/infra/firecrawl"
)

func links(urls ...string) []firecrawl.Link {
	out := make([]firecrawl.Link, len(urls))
	for i, u := range urls {
		out[i] = firecrawl.Link{URL: u}
	}
	return out
}

func TestSelectPagesTakesKindsInOrderWithinBudget(t *testing.T) {
	site := links(
		"https://acme.com/journal",
		"https://acme.com/journal/why-we-roast-light",
		"https://acme.com/journal/the-harvest-diary",
		"https://acme.com/journal/how-to-dial-in",
		"https://www.acme.com/about/our-story",
		"https://acme.com/about",
		"https://acme.com/products/espresso",
		"https://acme.com/products/filter",
		"https://acme.com/products/decaf",
		"https://acme.com/products/gift-card",
		"https://acme.com/pricing",
		"https://acme.com/terms-of-sale",
		"https://acme.com/privacy-policy",
		"https://acme.com/legal/disclaimer",
		"https://acme.com/legal/returns",
		"https://acme.com/wholesale",
		"https://acme.com/cart",
		"https://acme.com/account/login",
		"https://acme.com/pricing?plan=pro",
		"https://acme.com/brochure.pdf",
		"https://shop.acme.com/espresso",
		"https://other.com/about",
	)
	got := SelectPages("https://Acme.com", site, MaxWebsitePages)
	want := []string{
		"https://acme.com/",
		"https://www.acme.com/about/our-story",
		"https://acme.com/products/espresso",
		"https://acme.com/products/filter",
		"https://acme.com/products/decaf",
		"https://acme.com/pricing",
		"https://acme.com/terms-of-sale",
		"https://acme.com/legal/disclaimer",
		"https://acme.com/journal/why-we-roast-light",
		"https://acme.com/journal/the-harvest-diary",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("picked:\n%v\nwant:\n%v", got, want)
	}
}

func TestSelectPagesFillsWithTopLevelPages(t *testing.T) {
	site := links("https://acme.com/coffee", "https://acme.com/coffee/subscriptions", "https://acme.com/wholesale", "https://acme.com/about")
	got := SelectPages("https://acme.com/", site, 5)
	want := []string{
		"https://acme.com/",
		"https://acme.com/about",
		"https://acme.com/coffee",
		"https://acme.com/wholesale",
		"https://acme.com/coffee/subscriptions",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("picked %v, want %v", got, want)
	}
}

func TestSelectPagesKeepsTheGivenPage(t *testing.T) {
	got := SelectPages("https://acme.com/journal/why-we-roast-light", nil, MaxWebsitePages)
	want := []string{"https://acme.com/", "https://acme.com/journal/why-we-roast-light"}
	if !slices.Equal(got, want) {
		t.Fatalf("picked %v, want %v", got, want)
	}
}

func TestSelectPagesRejectsBadInput(t *testing.T) {
	if got := SelectPages("ftp://acme.com", nil, 10); got != nil {
		t.Fatalf("picked %v", got)
	}
	if got := SelectPages("https://acme.com", nil, 0); got != nil {
		t.Fatalf("picked %v", got)
	}
}

func TestSelectPagesUsesTitles(t *testing.T) {
	site := []firecrawl.Link{{URL: "https://acme.com/p/123", Title: "Pricing"}, {URL: "https://acme.com/x", Title: "About"}}
	got := SelectPages("https://acme.com", site, 3)
	want := []string{"https://acme.com/", "https://acme.com/x", "https://acme.com/p/123"}
	if !slices.Equal(got, want) {
		t.Fatalf("picked %v, want %v", got, want)
	}
}
