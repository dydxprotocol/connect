package oracle

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cometbft/cometbft/libs/log"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/skip-mev/slinky/oracle/types"
	"github.com/skip-mev/slinky/oracle/utils"
	ssync "github.com/skip-mev/slinky/pkg/sync"
	"golang.org/x/exp/maps"
	"golang.org/x/sync/errgroup"
)

// Oracle implements the core component responsible for fetching exchange rates
// for a given set of tickers and determining exchange rates.
type Oracle struct {
	// --------------------- General Config --------------------- //
	mtx    sync.RWMutex
	logger log.Logger
	closer *ssync.Closer

	// --------------------- Provider Config --------------------- //
	// providerTimeout is the maximum amount of time to wait for a provider to
	// respond to a price request.
	providerTimeout time.Duration

	// Providers is the set of providers that the oracle will fetch prices from.
	// Each provider is responsible for fetching prices for a given set of
	// currency pairs (base, quote). The oracle will fetch prices from each
	// provider concurrently.
	providers []types.Provider

	// --------------------- Oracle Config --------------------- //
	// oracleTicker is the interval at which the oracle will fetch prices from
	// providers.
	oracleTicker time.Duration

	// lastPriceSync is the last time the oracle successfully fetched prices from
	// providers.
	lastPriceSync time.Time

	// status is the current status of the oracle (running or not).
	status atomic.Bool

	// prices is the current set of prices fetched from providers.
	prices map[string]sdk.Dec
}

// New returns a new instance of an Oracle. The oracle inputs providers that are
// responsible for fetching prices for a given set of currency pairs (base, quote). The oracle
// will fetch new prices concurrently every oracleTicker interval. In the case where
// the oracle fails to fetch prices from a given provider, it will continue to fetch prices
// from the remaining providers. The oracle currently assumes that each provider aggregates prices
// using TWAPs, TVWAPs, or something similar. When determining the aggregated price for a
// given curreny pair, the oracle will compute the median price across all providers.
func New(logger log.Logger,
	providerTimeout, oracleTicker time.Duration,
	providers []types.Provider,
) *Oracle {
	if logger == nil {
		panic("logger cannot be nil")
	}

	if providers == nil {
		panic("price providers cannot be nil")
	}

	return &Oracle{
		logger:          logger,
		closer:          ssync.NewCloser(),
		providerTimeout: providerTimeout,
		oracleTicker:    oracleTicker,
		providers:       providers,
		prices:          make(map[string]sdk.Dec),
	}
}

// IsRunning returns true if the oracle is running.
func (o *Oracle) IsRunning() bool {
	return o.status.Load()
}

// Start starts the (blocking) oracle process. It will return when the context
// is cancelled or the oracle is stopped. The oracle will fetch prices from each
// provider concurrently every oracleTicker interval.
func (o *Oracle) Start(ctx context.Context) error {
	o.logger.Info("starting oracle")

	o.status.Store(true)
	defer o.status.Store(false)

	ticker := time.NewTicker(o.oracleTicker)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			o.Stop()
			return ctx.Err()

		case <-o.closer.Done():
			return nil

		case <-ticker.C:
			o.tick(ctx)
		}
	}
}

// Stop stops the oracle process and waits for it to gracefully exit.
func (o *Oracle) Stop() {
	o.logger.Info("stopping oracle")

	o.closer.Close()
	<-o.closer.Done()
}

// tick executes a single oracle tick. It fetches prices from each provider
// concurrently and computes the aggregated price for each currency pair. The
// oracle then sets the aggregated prices. In the case where any one of the provider
// fails to provide a set of prices, the oracle will continue to aggregate prices
// from the remaining providers.
func (o *Oracle) tick(ctx context.Context) {
	o.logger.Info("starting oracle tick")

	// Create a goroutine group to fetch prices from each provider concurrently.
	g, groupCtx := errgroup.WithContext(ctx)
	g.SetLimit(len(o.providers))

	// Create a price aggregator to aggregate prices from each provider.
	priceAgg := types.NewPriceAggregator()

	// In the case where the oracle panics, we will log the error and cancel all of the
	// the goroutines. In the case where anything panics, the oracle will not update
	// the prices and will attempt to fetch prices again on the next tick.
	defer func() {
		if r := recover(); r != nil {
			o.logger.Error("oracle tick panicked", "err", r)
			groupCtx.Done()
		}
	}()

	// Fetch prices from each provider concurrently. Each provider is responsible
	// for fetching prices for the given set of (base, quote) currency pairs. In the case where
	// a provider fails to fetch prices, we will log the error and continue to
	// aggregate prices from the remaining providers.
	for _, priceProvider := range o.providers {
		g.Go(o.fetchPricesFn(priceProvider, priceAgg))
	}

	// By default, errorgroup will wait for all goroutines to finish before returning.
	if err := g.Wait(); err != nil {
		o.logger.Error("wait group failed with error", "err", err)
		return
	}

	// Compute aggregated prices and update the oracle.
	medianPrices := utils.ComputeMedian(priceAgg.GetProviderPrices())
	o.SetPrices(medianPrices)
	o.SetLastSyncTime(time.Now().UTC())

	o.logger.Info("oracle updated prices")
}

// fetchPrices returns a closure that fetches prices from the given provider. This is meant
// to be used in a goroutine. It accepts the provider and price aggregator as inputs. In the
// case where the provider fails to fetch prices, we will log the error and not update the
// price aggregator. We gracefully handle panics by recovering and logging the error. If the
// function panics, the wait group will cancel all other goroutines and skip the update for the
// oracle.
func (o *Oracle) fetchPricesFn(provider types.Provider, priceAgg *types.PriceAggregator) func() error {
	return func() (err error) {
		// Recover from any panics to graceful end the tick.
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic in fetchPricesFn %v", r)
			}
		}()

		o.logger.Info("fetching prices from provider", provider.Name())

		doneCh := make(chan bool, 1)
		errCh := make(chan error, 1)

		go func() {
			// Recover from any panics while fetching prices.
			defer func() {
				if r := recover(); r != nil {
					errCh <- fmt.Errorf("panic when fetching prices %v", r)
				}
			}()

			// Fetch and set prices from the provider.
			prices, err := provider.GetPrices()
			if err != nil {
				errCh <- err
				return
			}

			priceAgg.SetPrices(provider, prices)

			doneCh <- true
		}()

		select {
		case <-doneCh:
			o.logger.Info("fetched prices from provider", provider.Name())
			break

		case err := <-errCh:
			o.logger.Error("failed to fetch prices from provider", provider.Name(), err)
			break

		case <-time.After(o.providerTimeout):
			o.logger.Error("provider timed out", provider.Name())
			break
		}

		return nil
	}
}

// SetLastSyncTime sets the last time the oracle successfully updated prices.
func (o *Oracle) SetLastSyncTime(t time.Time) {
	o.mtx.Lock()
	defer o.mtx.Unlock()

	o.lastPriceSync = t
}

// GetLastSyncTime returns the last time the oracle successfully updated prices.
func (o *Oracle) GetLastSyncTime() time.Time {
	o.mtx.RLock()
	defer o.mtx.RUnlock()

	return o.lastPriceSync
}

// SetPrices sets the aggregate prices for the oracle.
func (o *Oracle) SetPrices(prices map[string]sdk.Dec) {
	o.mtx.Lock()
	defer o.mtx.Unlock()

	o.prices = prices
}

// GetPrices returns the aggregate prices from the oracle.
func (o *Oracle) GetPrices() map[string]sdk.Dec {
	o.mtx.RLock()
	defer o.mtx.RUnlock()

	p := make(map[string]sdk.Dec, len(o.prices))
	maps.Copy(p, o.prices)

	return p
}
