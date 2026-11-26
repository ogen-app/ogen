package polar

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/ogen-app/ogen/src/domain/billing"
)

// productJSON is the subset of the product object the mapping reads.
type productJSON struct {
	ID                     string  `json:"id"`
	Name                   string  `json:"name"`
	IsRecurring            bool    `json:"is_recurring"`
	IsArchived             bool    `json:"is_archived"`
	RecurringInterval      *string `json:"recurring_interval"`
	RecurringIntervalCount *int    `json:"recurring_interval_count"`
	TrialInterval          *string `json:"trial_interval"`
	TrialIntervalCount     *int    `json:"trial_interval_count"`
	Prices                 []struct {
		AmountType    string `json:"amount_type"`
		PriceAmount   *int64 `json:"price_amount"`
		PriceCurrency string `json:"price_currency"`
		IsArchived    bool   `json:"is_archived"`
	} `json:"prices"`
}

func (p productJSON) toProduct(testMode bool) billing.Product {
	out := billing.Product{ID: p.ID, Name: p.Name, Archived: p.IsArchived, TestMode: testMode, IntervalCount: 1}
	if p.RecurringInterval != nil {
		out.Interval = *p.RecurringInterval
	}
	if p.RecurringIntervalCount != nil {
		out.IntervalCount = *p.RecurringIntervalCount
	}
	if p.TrialInterval != nil && *p.TrialInterval != "" {
		out.HasFreeTrial, out.TrialInterval, out.TrialIntervalCount = true, *p.TrialInterval, 1
		if p.TrialIntervalCount != nil {
			out.TrialIntervalCount = *p.TrialIntervalCount
		}
	}
	// Only fixed prices are sellable; a pay-what-you-want or free price is
	// never mapped.
	for _, pr := range p.Prices {
		if pr.AmountType != "fixed" || pr.IsArchived || pr.PriceAmount == nil {
			continue
		}
		out.Prices = append(out.Prices, billing.ProductPrice{Currency: strings.ToUpper(pr.PriceCurrency), AmountMinor: *pr.PriceAmount})
	}
	return out
}

// GetProduct implements billing.Provider.
func (c *Client) GetProduct(ctx context.Context, id string) (billing.Product, error) {
	var p productJSON
	if err := c.do(ctx, request{testMode: c.testMode, method: http.MethodGet, path: "/products/" + url.PathEscape(id)}, &p); err != nil {
		return billing.Product{}, err
	}
	if p.ID == "" {
		return billing.Product{}, errNoID
	}
	return p.toProduct(c.testMode), nil
}

// Catalog implements billing.Provider: every recurring product of the mode's
// organization, archived ones included (the mapping flags them).
func (c *Client) Catalog(ctx context.Context, testMode bool) (billing.Catalog, error) {
	_, orgID := c.credentials(testMode)
	out := billing.Catalog{TestMode: testMode}
	for pageNo := 1; ; pageNo++ {
		var pg page[productJSON]
		q := listQuery(orgID, pageNo, url.Values{"is_recurring": {"true"}})
		if err := c.do(ctx, request{testMode: testMode, method: http.MethodGet, path: "/products/", query: q}, &pg); err != nil {
			return billing.Catalog{}, err
		}
		for _, p := range pg.Items {
			if p.ID == "" || !p.IsRecurring {
				continue
			}
			out.Products = append(out.Products, p.toProduct(testMode))
		}
		if pageNo >= pg.Pagination.MaxPage || len(pg.Items) == 0 {
			return out, nil
		}
		if pageNo > 1000 {
			return billing.Catalog{}, fmt.Errorf("polar: product list did not end")
		}
	}
}
