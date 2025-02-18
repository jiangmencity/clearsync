package quotes

import (
	"context"
	"fmt"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ipfs/go-log/v2"

	"github.com/layer-3/clearsync/pkg/abi/isecta_v3_factory"
	"github.com/layer-3/clearsync/pkg/abi/isecta_v3_pool"
	"github.com/layer-3/clearsync/pkg/debounce"
	"github.com/layer-3/clearsync/pkg/safe"
)

var (
	loggerSectaV3 = log.Logger("secta_v3")
	// Secta v3 uses 0.01%, 0.05%, 0.25%, and 1% fee tiers.
	sectaV3FeeTiers = []uint{100, 500, 2500, 10000}
)

type sectaV3 struct {
	factoryAddress common.Address
	factory        *isecta_v3_factory.ISectaV3Factory

	assets *safe.Map[string, poolToken]
	client *ethclient.Client
}

func newSectaV3(rpcUrl string, config SectaV3Config, outbox chan<- TradeEvent, history HistoricalData) (Driver, error) {
	hooks := &sectaV3{
		factoryAddress: common.HexToAddress(config.FactoryAddress),
	}

	params := baseDexConfig[
		isecta_v3_pool.ISectaV3PoolSwap,
		isecta_v3_pool.ISectaV3Pool,
		*isecta_v3_pool.ISectaV3PoolSwapIterator,
	]{
		// Params
		DriverType: DriverSectaV3,
		RPC:        rpcUrl,
		AssetsURL:  config.AssetsURL,
		MappingURL: config.MappingURL,
		MarketsURL: config.MarketsURL,
		IdlePeriod: config.IdlePeriod,
		// Hooks
		PostStartHook: hooks.postStart,
		PoolGetter:    hooks.getPool,
		EventParser:   hooks.parseSwap,
		IterDeref:     hooks.derefIter,
		// State
		Outbox:  outbox,
		Logger:  loggerSectaV3,
		Filter:  config.Filter,
		History: history,
	}
	return newBaseDEX(params)
}

func (s *sectaV3) postStart(driver *baseDEX[
	isecta_v3_pool.ISectaV3PoolSwap,
	isecta_v3_pool.ISectaV3Pool,
	*isecta_v3_pool.ISectaV3PoolSwapIterator,
]) (err error) {
	s.client = driver.Client()
	s.assets = driver.Assets()

	s.factory, err = isecta_v3_factory.NewISectaV3Factory(s.factoryAddress, s.client)
	if err != nil {
		return fmt.Errorf("failed to instantiate a Secta v3 pool factory contract: %w", err)
	}
	return nil
}

func (s *sectaV3) getPool(ctx context.Context, market Market) ([]*dexPool[isecta_v3_pool.ISectaV3PoolSwap, *isecta_v3_pool.ISectaV3PoolSwapIterator], error) {
	baseToken, quoteToken, err := getTokens(s.assets, market, loggerSectaV3)
	if err != nil {
		return nil, fmt.Errorf("failed to get tokens: %w", err)
	}

	if strings.ToLower(baseToken.Symbol) == "eth" {
		baseToken.Address = wethContract
	}

	poolAddresses := make([]common.Address, 0, len(sectaV3FeeTiers))
	zeroAddress := common.HexToAddress("0x0")
	for _, feeTier := range sectaV3FeeTiers {
		var poolAddress common.Address
		err = debounce.Debounce(ctx, loggerSectaV3, func(ctx context.Context) error {
			poolAddress, err = s.factory.GetPool(&bind.CallOpts{Context: ctx}, baseToken.Address, quoteToken.Address, big.NewInt(int64(feeTier)))
			return err
		})
		if err != nil {
			return nil, err
		}

		if poolAddress != zeroAddress {
			loggerSectaV3.Infow("found pool",
				"market", market,
				"selected fee tier", fmt.Sprintf("%.2f%%", float64(feeTier)/10000),
				"address", poolAddress)
			poolAddresses = append(poolAddresses, poolAddress)
		}
	}

	pools := make([]*dexPool[isecta_v3_pool.ISectaV3PoolSwap, *isecta_v3_pool.ISectaV3PoolSwapIterator], 0, len(poolAddresses))
	for _, poolAddress := range poolAddresses {
		poolContract, err := isecta_v3_pool.NewISectaV3Pool(poolAddress, s.client)
		if err != nil {
			return nil, fmt.Errorf("failed to build Secta v3 pool contract: %w", err)
		}

		var basePoolToken common.Address
		err = debounce.Debounce(ctx, loggerSectaV3, func(ctx context.Context) error {
			basePoolToken, err = poolContract.Token0(&bind.CallOpts{Context: ctx})
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("failed to get base token address for Secta v3 pool: %w", err)
		}

		var quotePoolToken common.Address
		err = debounce.Debounce(ctx, loggerSectaV3, func(ctx context.Context) error {
			quotePoolToken, err = poolContract.Token1(&bind.CallOpts{Context: ctx})
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("failed to get quote token address for Secta v3 pool: %w", err)
		}

		isDirect := baseToken.Address == basePoolToken && quoteToken.Address == quotePoolToken
		isReversed := quoteToken.Address == basePoolToken && baseToken.Address == quotePoolToken
		pool := &dexPool[isecta_v3_pool.ISectaV3PoolSwap, *isecta_v3_pool.ISectaV3PoolSwapIterator]{
			Contract:   poolContract,
			Address:    poolAddress,
			BaseToken:  baseToken,
			QuoteToken: quoteToken,
			Market:     market,
			Reversed:   isReversed,
		}

		// Append pool only if the token addresses
		// match direct or reversed configurations
		if isDirect || isReversed {
			pools = append(pools, pool)
		}
	}

	return pools, nil
}

func (s *sectaV3) parseSwap(
	swap *isecta_v3_pool.ISectaV3PoolSwap,
	pool *dexPool[isecta_v3_pool.ISectaV3PoolSwap, *isecta_v3_pool.ISectaV3PoolSwapIterator],
) (trade TradeEvent, err error) {
	opts := v3TradeOpts[isecta_v3_pool.ISectaV3PoolSwap, *isecta_v3_pool.ISectaV3PoolSwapIterator]{
		Driver:          DriverSectaV3,
		RawAmount0:      swap.Amount0,
		RawAmount1:      swap.Amount1,
		RawSqrtPriceX96: swap.SqrtPriceX96,
		Pool:            pool,
		Swap:            swap,
		Logger:          loggerSectaV3,
	}
	return buildV3Trade(opts)
}

func (s *sectaV3) derefIter(
	iter *isecta_v3_pool.ISectaV3PoolSwapIterator,
) *isecta_v3_pool.ISectaV3PoolSwap {
	return iter.Event
}
