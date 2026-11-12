package app

import (
	"context"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/pricing"
	"github.com/Mag1cFall/AIStudio2API/internal/requestdb"
)

// startPriceList attaches a per-instance catalog; errors affect only reference estimates.
func startPriceList(ctx context.Context, ledger *requestdb.Store, path string, log func(string, string, string)) {
	prices := pricing.NewCatalog()
	changed := func(status pricing.Status) {
		if log == nil {
			return
		}
		if status.LastError != "" {
			log("service", "WARN", "参考价格单未加载，保留有效价格 | "+status.LastError)
			return
		}
		log("service", "INFO", "参考价格单已加载 | 版本="+status.Version+" 摘要="+status.SHA256)
	}
	_ = prices.Reload(path)
	changed(prices.PricingStatus())
	ledger.SetPricing(prices)
	go prices.Watch(ctx, path, 2*time.Second, changed)
}
