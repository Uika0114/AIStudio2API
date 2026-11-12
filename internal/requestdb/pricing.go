package requestdb

import "github.com/Mag1cFall/AIStudio2API/internal/pricing"

// SetPricing attaches this instance's live catalog without changing stored history.
func (store *Store) SetPricing(catalog *pricing.Catalog) { store.prices.Store(catalog) }

func (store *Store) priceSnapshot() *pricing.Snapshot {
	if store != nil {
		if catalog := store.prices.Load(); catalog != nil {
			return catalog.Snapshot()
		}
	}
	return pricing.DefaultSnapshot()
}

// PricingStatus is a management-only view; real applications attach a catalog at startup.
func (store *Store) PricingStatus() pricing.Status {
	if store != nil {
		if catalog := store.prices.Load(); catalog != nil {
			return catalog.PricingStatus()
		}
	}
	return pricing.NewCatalog().PricingStatus()
}
