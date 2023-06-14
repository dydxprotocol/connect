package utils

import (
	"sort"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/skip-mev/slinky/oracle/types"
)

// ComputeMedian inputs the aggregated prices from all providers and computes
// the median price for each asset.
func ComputeMedian(providers types.AggregatedProviderPrices) map[string]sdk.Dec {
	// Iterate through all providers and their assets to get weighted prices
	pricesByAsset := make(map[string][]types.TickerPrice)
	for _, providerPrices := range providers {
		for asset, price := range providerPrices {
			// Initialize the asset array if it doesn't exist
			if _, ok := pricesByAsset[asset]; !ok {
				pricesByAsset[asset] = make([]types.TickerPrice, 0)
			}

			pricesByAsset[asset] = append(pricesByAsset[asset], price)
		}
	}

	medianPrices := make(map[string]sdk.Dec)

	// Iterate through all assets and compute the median price
	for asset, prices := range pricesByAsset {
		// Sort the prices and grab the median
		sort.SliceStable(prices, func(i, j int) bool {
			return prices[i].Price.LT(prices[j].Price)
		})

		medianPrice := prices[len(prices)/2].Price
		medianPrices[asset] = medianPrice
	}

	return medianPrices
}
