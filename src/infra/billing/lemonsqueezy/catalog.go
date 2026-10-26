package lemonsqueezy

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/ogen-app/ogen/src/domain/billing"
)

// catalogPageSize is the API's largest page.
const catalogPageSize = 100

// maxCatalogPages bounds the product walk: 100 pages of 100 products is far
// beyond any real store.
const maxCatalogPages = 100

// variantStatusPending marks a product's default variant. It is hidden when
// the product has variants of its own, but it is the variant that sells a
// product without any, so only the hidden one is skipped.
const variantStatusPending = "pending"

type variantAttrs struct {
	ProductID          flexString `json:"product_id"`
	Name               string     `json:"name"`
	Price              int64      `json:"price"`
	IsSubscription     bool       `json:"is_subscription"`
	Interval           *string    `json:"interval"`
	IntervalCount      *int       `json:"interval_count"`
	HasFreeTrial       bool       `json:"has_free_trial"`
	TrialInterval      *string    `json:"trial_interval"`
	TrialIntervalCount *int       `json:"trial_interval_count"`
	Status             string     `json:"status"`
	TestMode           bool       `json:"test_mode"`
}

// decodeVariant reads a variant resource. subscription reports whether it
// bills on an interval.
func decodeVariant(r resource) (v billing.Variant, subscription bool, err error) {
	var a variantAttrs
	if err := json.Unmarshal(r.Attributes, &a); err != nil {
		return billing.Variant{}, false, fmt.Errorf("lemonsqueezy: decode variant: %w", err)
	}
	v = billing.Variant{
		ID: string(r.ID), PriceMinor: a.Price, HasFreeTrial: a.HasFreeTrial, TestMode: a.TestMode,
		ProductID: string(a.ProductID), Name: a.Name, Status: a.Status,
	}
	if a.Interval != nil {
		v.Interval = *a.Interval
	}
	if a.IntervalCount != nil {
		v.IntervalCount = *a.IntervalCount
	}
	if a.TrialInterval != nil {
		v.TrialInterval = *a.TrialInterval
	}
	if a.TrialIntervalCount != nil {
		v.TrialIntervalCount = *a.TrialIntervalCount
	}
	return v, a.IsSubscription || v.Interval != "", nil
}

// Catalog implements billing.Provider: the store's currency, then its
// products with their variants, page by page. Only subscription variants that
// can be bought are listed.
func (c *Client) Catalog(ctx context.Context, testMode bool) (billing.Catalog, error) {
	key := c.keyFor(testMode)
	out := billing.Catalog{TestMode: testMode}

	var store document
	if err := c.doWith(ctx, key, http.MethodGet, "/stores/"+url.PathEscape(c.storeID), nil, &store); err != nil {
		return billing.Catalog{}, err
	}
	var storeAttrs struct {
		Currency string `json:"currency"`
	}
	if err := json.Unmarshal(store.Data.Attributes, &storeAttrs); err != nil {
		return billing.Catalog{}, fmt.Errorf("lemonsqueezy: decode store: %w", err)
	}
	out.Currency = storeAttrs.Currency

	for page := 1; page <= maxCatalogPages; page++ {
		q := url.Values{}
		q.Set("filter[store_id]", c.storeID)
		q.Set("include", "variants")
		q.Set("page[size]", strconv.Itoa(catalogPageSize))
		q.Set("page[number]", strconv.Itoa(page))
		var doc struct {
			Data     []resource `json:"data"`
			Included []resource `json:"included"`
			Meta     struct {
				Page struct {
					LastPage int `json:"lastPage"`
				} `json:"page"`
			} `json:"meta"`
		}
		if err := c.doWith(ctx, key, http.MethodGet, "/products?"+q.Encode(), nil, &doc); err != nil {
			return billing.Catalog{}, err
		}
		products := map[string]string{}
		for _, p := range doc.Data {
			var a struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(p.Attributes, &a); err != nil {
				return billing.Catalog{}, fmt.Errorf("lemonsqueezy: decode product: %w", err)
			}
			products[string(p.ID)] = a.Name
		}
		variants := make([]billing.Variant, 0, len(doc.Included))
		perProduct := map[string]int{}
		for _, r := range doc.Included {
			if r.Type != "variants" {
				continue
			}
			v, subscription, err := decodeVariant(r)
			if err != nil {
				return billing.Catalog{}, err
			}
			perProduct[v.ProductID]++
			if subscription {
				variants = append(variants, v)
			}
		}
		for _, v := range variants {
			if v.Status == variantStatusPending && perProduct[v.ProductID] > 1 {
				continue
			}
			v.ProductName = products[v.ProductID]
			out.Variants = append(out.Variants, v)
		}
		if page >= doc.Meta.Page.LastPage {
			break
		}
	}
	slices.SortFunc(out.Variants, func(a, b billing.Variant) int {
		return cmp.Or(cmp.Compare(a.ProductName, b.ProductName), cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}
