package clients

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/rs/zerolog/log"
	"math/big"
	"mev_bot/contracts"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

var (
	PoolFactories = map[string]common.Address{
		"v2": common.HexToAddress("0x5C69bEe701ef814a2B6a3EDD4B1652CB9cc5aA6f"),
		"v3": common.HexToAddress("0x1F98431c8aD98523631AE4a59f267346ea31F984"),
	}
	InitialDeploymentBlock = 10000835
	TxFormat               = "Url: https://etherscan.io/tx/%s"
	WETHAddress            = common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2")
	AddressZero            = common.Address{}
	UniswapV3Quoter        = common.HexToAddress("0x61fFE014bA17989E743c5F6cB21bF9697530B21e")
	BribePercent           = big.NewInt(5)
)

type PoolSummaryFile struct {
	Pools         map[common.Address]Pool `json:"pools"`
	LastSeenBlock uint64                  `json:"lastSeenBlock"`
}

type Pool struct {
	Token0   common.Address
	Token1   common.Address
	Address  common.Address
	Reserve0 *big.Int
	Reserve1 *big.Int
	Fee      *big.Int
	Type     string
	Enabled  bool
}

type UniswapClient struct {
	BotContract       *contracts.UniswapBotV2
	client            *ethclient.Client
	wsClient          *ethclient.Client
	historyClient     *ethclient.Client
	arbitrageContract *contracts.UniswapBotV2
	botToken          string
	chatId            string
	privKey           *ecdsa.PrivateKey
	chainId           *big.Int
	mevAddress        common.Address
	address           common.Address
	ctx               context.Context
	prices            map[string]string
	Pools             map[common.Address]Pool
	FactoryAddresses  []common.Address
	LastSeenBlock     uint64
	statePath         string
	factoryAddressMap map[common.Address]bool
	tokensPath        string
	tokenMap          map[common.Address]bool
}

func NewUniswapClient(rpcURL string, wsURL string, historyURL string, botToken string, chatId string, mevAddress string, privKey string, ctx context.Context) (*UniswapClient, error) {
	client, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return nil, err
	}
	wsClient, err := ethclient.DialContext(ctx, wsURL)
	if err != nil {
		return nil, err
	}
	historyClient, err := ethclient.DialContext(ctx, historyURL)
	if err != nil {
		return nil, err
	}
	arbitrageClient, err := ethclient.DialContext(ctx, "https://rpc.flashbots.net/fast")
	if err != nil {
		return nil, err
	}
	signingKey, err := crypto.HexToECDSA(privKey)
	if err != nil {
		return nil, err
	}
	publicKey := signingKey.Public()
	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("public key error")
	}
	address := crypto.PubkeyToAddress(*publicKeyECDSA)
	chainId, err := client.ChainID(ctx)
	if err != nil {
		return nil, err
	}
	var botContract *contracts.UniswapBotV2
	var botAddress common.Address
	if mevAddress == "" {
		opts, err := bind.NewKeyedTransactorWithChainID(signingKey, chainId)
		if err != nil {
			return nil, err
		}
		gas, err := client.SuggestGasPrice(ctx)
		if err != nil {
			return nil, err
		}
		opts.GasPrice = gas
		deployedAddress, _, deployedContract, err := contracts.DeployUniswapBotV2(
			opts,
			client,
		)
		if err != nil {
			return nil, err
		}
		botContract = deployedContract
		log.Info().Str("Contract Address", deployedAddress.String()).Msg("Deployed")
		botAddress = deployedAddress
	} else {
		botContract, err = contracts.NewUniswapBotV2(common.HexToAddress(mevAddress), client)
		botAddress = common.HexToAddress(mevAddress)
		if err != nil {
			return nil, err
		}
	}
	arbitrageContract, err := contracts.NewUniswapBotV2(common.HexToAddress(mevAddress), arbitrageClient)
	if err != nil {
		return nil, err
	}
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	filePath := wd + "/data/pools.json"
	tokensPath := wd + "/data/tokens.json"
	return &UniswapClient{
		client:        client,
		wsClient:      wsClient,
		historyClient: historyClient,
		botToken:      botToken,
		chatId:        chatId,
		BotContract:   botContract,
		privKey:       signingKey,
		chainId:       chainId,
		ctx:           ctx,
		address:       address,
		mevAddress:    botAddress,
		prices:        make(map[string]string),
		Pools:         make(map[common.Address]Pool),
		LastSeenBlock: uint64(InitialDeploymentBlock),
		FactoryAddresses: []common.Address{
			PoolFactories["v2"],
			PoolFactories["v3"],
		},
		factoryAddressMap: map[common.Address]bool{
			PoolFactories["v2"]: true,
			PoolFactories["v3"]: true,
		},
		statePath:         filePath,
		tokensPath:        tokensPath,
		tokenMap:          make(map[common.Address]bool),
		arbitrageContract: arbitrageContract,
	}, nil
}

func (c *UniswapClient) InitializePools() ([]Pool, error) {
	err := c.ReadState()
	if err != nil {
		return nil, err
	}
	//err = c.ReadTokens()
	//if err != nil {
	//	return nil, err
	//}
	currentBlockNumber, err := c.client.BlockNumber(c.ctx)
	if err != nil {
		return nil, err
	}
	ch := make(chan []Pool)
	log.Info().Msg("Getting pools")
	for name, address := range PoolFactories {
		if name == "v3" {
			contract, err := contracts.NewIEvents(address, c.historyClient)
			if err != nil {
				return nil, err
			}
			go GetV3Pools(ch, contract, currentBlockNumber, c.LastSeenBlock)
		} else {
			contract, err := contracts.NewIEvents(address, c.historyClient)
			if err != nil {
				return nil, err
			}
			go GetV2Pools(ch, contract, currentBlockNumber, c.LastSeenBlock)
		}
	}
	newPools := []Pool{}
	for i := 0; i < len(PoolFactories); i++ {
		collectedPools := <-ch
		for _, pool := range collectedPools {
			//_, ok1 := c.tokenMap[pool.Token1]
			//_, ok2 := c.tokenMap[pool.Token0]
			//if ok1 && ok2 {
			//	newPools = append(newPools, pool)
			//}
			newPools = append(newPools, pool)
		}
	}
	c.LastSeenBlock = currentBlockNumber
	log.Info().Msg("Finished getting pools")
	return newPools, nil
}

func (c *UniswapClient) SetReserves(newPools []Pool) error {
	wethReserveLimit, ok := new(big.Int).SetString("10000000000000000000", 10)
	if !ok {
		return errors.New("can not set weth reserve limit")
	}
	log.Info().Msg("Starting getting reserves")
	zero := big.NewInt(0)
	oldReserveParams := []contracts.UniswapBotV2ReserveParams{}
	newReserveParams := []contracts.UniswapBotV2ReserveParams{}
	for _, pool := range c.Pools {
		oldReserveParams = append(oldReserveParams, contracts.UniswapBotV2ReserveParams{
			Token0: pool.Token0,
			Token1: pool.Token1,
			Pool:   pool.Address,
		})
	}
	for _, pool := range newPools {
		newReserveParams = append(newReserveParams, contracts.UniswapBotV2ReserveParams{
			Token0: pool.Token0,
			Token1: pool.Token1,
			Pool:   pool.Address,
		})
	}
	rawContract := contracts.UniswapBotV2Raw{Contract: c.BotContract}
	opts := bind.CallOpts{
		From: c.address,
	}
	batchSize := 2000
	for i := 0; i < len(oldReserveParams); i += batchSize {
		var oldReservesRaw []interface{}
		var paramSlice []contracts.UniswapBotV2ReserveParams
		if i+batchSize < len(oldReserveParams) {
			paramSlice = oldReserveParams[i : i+batchSize]
		} else {
			paramSlice = oldReserveParams[i:]
		}
		err := rawContract.Call(&opts, &oldReservesRaw, "multiGetReserves", paramSlice)
		if err != nil {
			return err
		}
		res := oldReservesRaw[0].([][]*big.Int)
		for j, oldParam := range paramSlice {
			pool := c.Pools[oldParam.Pool]
			reserves := res[j]
			pool.Reserve1 = reserves[1]
			pool.Reserve0 = reserves[0]
			if pool.Reserve0.Cmp(zero) == 0 || pool.Reserve1.Cmp(zero) == 0 {
				pool.Enabled = false
			}
			if pool.Token0.String() == WETHAddress.String() && pool.Reserve0.Cmp(wethReserveLimit) == -1 {
				pool.Enabled = false
			}
			if pool.Token1.String() == WETHAddress.String() && pool.Reserve1.Cmp(wethReserveLimit) == -1 {
				pool.Enabled = false
			}
			c.Pools[oldParam.Pool] = pool
		}
	}

	for i, pool := range newPools {
		log.Info().Interface("index", i).Int("total", len(newPools)).Msg("reserve progress")
		var reservesRaw []interface{}
		err := rawContract.Call(&opts, &reservesRaw, "getReserves", newReserveParams[i])
		if err != nil {
			continue
		}
		reserves := reservesRaw[0].([]*big.Int)
		pool.Reserve0 = reserves[0]
		pool.Reserve1 = reserves[1]
		if pool.Reserve0.Cmp(zero) == 0 || pool.Reserve1.Cmp(zero) == 0 {
			pool.Enabled = false
		}
		if pool.Token0.String() == WETHAddress.String() && pool.Reserve0.Cmp(wethReserveLimit) == -1 {
			pool.Enabled = false
		}
		if pool.Token1.String() == WETHAddress.String() && pool.Reserve1.Cmp(wethReserveLimit) == -1 {
			pool.Enabled = false
		}
		c.Pools[pool.Address] = pool
	}
	err := c.SaveState()
	log.Info().Msg("Saving reserves")
	return err
}

func (c *UniswapClient) ReadState() error {
	file, err := os.ReadFile(c.statePath)
	var PoolSummary PoolSummaryFile
	if err == nil {
		err = json.Unmarshal(file, &PoolSummary)
		if err != nil {
			return err
		}
		c.LastSeenBlock = PoolSummary.LastSeenBlock
		c.Pools = PoolSummary.Pools
	}
	log.Info().Uint64("lastSeenBlock", c.LastSeenBlock).Int("totalPools", len(c.Pools)).Msg("state read summary")
	return nil
}

func (c *UniswapClient) ReadTokens() error {
	file, err := os.ReadFile(c.tokensPath)
	var tokens []Token
	if err == nil {
		err = json.Unmarshal(file, &tokens)
		if err != nil {
			return err
		}
	} else {
		err = RetrieveTopTokens()
		if err != nil {
			return err
		}
		return c.ReadTokens()
	}
	for _, token := range tokens {
		c.tokenMap[common.HexToAddress(token.ContractAddress)] = true
	}
	return nil
}

func (c *UniswapClient) SaveState() error {
	var PoolSummary PoolSummaryFile
	PoolSummary.Pools = c.Pools
	PoolSummary.LastSeenBlock = c.LastSeenBlock
	file, err := json.MarshalIndent(PoolSummary, "", " ")
	if err != nil {
		return err
	}
	err = os.WriteFile(c.statePath, file, 0644)
	log.Info().Msg("written")
	return err
}

func (c *UniswapClient) ResolveLogs(logs []types.Log, blockHash common.Hash) ([]Pool, error) {
	wethReserveLimit, ok := new(big.Int).SetString("10000000000000000000", 10)
	if !ok {
		return nil, errors.New("can not set weth reserve limit")
	}
	zero := big.NewInt(0)
	contract, err := contracts.NewIEvents(c.address, c.client)
	if err != nil {
		return nil, err
	}
	logMap := make(map[common.Address]uint)
	processedPools := make(map[common.Address]Pool)
	log.Info().Str("blockHash", blockHash.String()).Msg("resolving block events")
	for _, blockLog := range logs {
		_, ok := c.factoryAddressMap[blockLog.Address]
		if ok {
			poolCreated, err := contract.ParsePoolCreated(blockLog)
			if err == nil {
				_, ok := c.Pools[poolCreated.Pool]
				pool := Pool{
					Address: poolCreated.Pool,
					Token0:  poolCreated.Token0,
					Token1:  poolCreated.Token1,
					Fee:     poolCreated.Fee,
					Type:    "v3",
					Enabled: true,
				}
				reserves, err := c.BotContract.GetReserves(nil, contracts.UniswapBotV2ReserveParams{
					Token0: pool.Token0,
					Token1: pool.Token1,
					Pool:   pool.Address,
				})
				if err != nil {
					continue
				}
				if !ok {
					pool.Reserve0 = reserves[0]
					pool.Reserve1 = reserves[1]
					if pool.Reserve0.Cmp(zero) == 0 || pool.Reserve1.Cmp(zero) == 0 {
						pool.Enabled = false
					}
					if pool.Token0.String() == WETHAddress.String() && pool.Reserve0.Cmp(wethReserveLimit) == -1 {
						pool.Enabled = false
					}
					if pool.Token1.String() == WETHAddress.String() && pool.Reserve1.Cmp(wethReserveLimit) == -1 {
						pool.Enabled = false
					}
					c.Pools[poolCreated.Pool] = pool
				}
			}
			pairCreated, err := contract.ParsePairCreated(blockLog)
			if err == nil {
				_, ok := c.Pools[pairCreated.Pair]
				pool := Pool{
					Address: pairCreated.Pair,
					Token0:  pairCreated.Token0,
					Token1:  pairCreated.Token1,
					Type:    "v2",
					Enabled: true,
				}
				reserves, err := c.BotContract.GetReserves(nil, contracts.UniswapBotV2ReserveParams{
					Token0: pool.Token0,
					Token1: pool.Token1,
					Pool:   pool.Address,
				})
				if err != nil {
					continue
				}
				if !ok {
					pool.Reserve0 = reserves[0]
					pool.Reserve1 = reserves[1]
					if pool.Reserve0.Cmp(zero) == 0 || pool.Reserve1.Cmp(zero) == 0 {
						pool.Enabled = false
					}
					if pool.Token0.String() == WETHAddress.String() && pool.Reserve0.Cmp(wethReserveLimit) == -1 {
						pool.Enabled = false
					}
					if pool.Token1.String() == WETHAddress.String() && pool.Reserve1.Cmp(wethReserveLimit) == -1 {
						pool.Enabled = false
					}
					c.Pools[pairCreated.Pair] = pool
				}
			}
		} else {
			oldIndex, ok := logMap[blockLog.Address]
			if !ok {
				logMap[blockLog.Address] = blockLog.Index
			} else {
				if blockLog.Index > oldIndex {
					logMap[blockLog.Address] = blockLog.Index
				} else {
					continue
				}
			}
			swapEvent, err := contract.ParseSwap(blockLog)
			if err == nil {
				pool, ok := c.Pools[blockLog.Address]
				if ok {
					pool.Reserve0 = new(big.Int).Add(pool.Reserve0, swapEvent.Amount0)
					pool.Reserve1 = new(big.Int).Add(pool.Reserve1, swapEvent.Amount1)
					if pool.Reserve0.Cmp(zero) != 1 || pool.Reserve1.Cmp(zero) != 1 {
						pool.Enabled = false
					}
					if pool.Token0.String() == WETHAddress.String() && pool.Reserve0.Cmp(wethReserveLimit) == -1 {
						pool.Enabled = false
					}
					if pool.Token1.String() == WETHAddress.String() && pool.Reserve1.Cmp(wethReserveLimit) == -1 {
						pool.Enabled = false
					}
					c.Pools[blockLog.Address] = pool
					_, ok := processedPools[blockLog.Address]
					if !ok {
						processedPools[blockLog.Address] = pool
					}
				}
			}
			mintEvent, err := contract.ParseMint(blockLog)
			if err == nil {
				pool, ok := c.Pools[blockLog.Address]
				if ok {
					pool.Reserve0 = new(big.Int).Add(pool.Reserve0, mintEvent.Amount0)
					pool.Reserve1 = new(big.Int).Add(pool.Reserve1, mintEvent.Amount1)
					if pool.Reserve0.Cmp(zero) != 1 || pool.Reserve1.Cmp(zero) != 1 {
						pool.Enabled = false
					}
					if pool.Token0.String() == WETHAddress.String() && pool.Reserve0.Cmp(wethReserveLimit) == -1 {
						pool.Enabled = false
					}
					if pool.Token1.String() == WETHAddress.String() && pool.Reserve1.Cmp(wethReserveLimit) == -1 {
						pool.Enabled = false
					}
					c.Pools[blockLog.Address] = pool
					_, ok := processedPools[blockLog.Address]
					if !ok {
						processedPools[blockLog.Address] = pool
					}
				}
			}
			burnEvent, err := contract.ParseBurn(blockLog)
			if err == nil {
				pool, ok := c.Pools[blockLog.Address]
				if ok {
					pool.Reserve0 = new(big.Int).Sub(pool.Reserve0, burnEvent.Amount0)
					pool.Reserve1 = new(big.Int).Sub(pool.Reserve1, burnEvent.Amount1)
					if pool.Reserve0.Cmp(zero) != 1 || pool.Reserve1.Cmp(zero) != 1 {
						pool.Enabled = false
					}
					if pool.Token0.String() == WETHAddress.String() && pool.Reserve0.Cmp(wethReserveLimit) == -1 {
						pool.Enabled = false
					}
					if pool.Token1.String() == WETHAddress.String() && pool.Reserve1.Cmp(wethReserveLimit) == -1 {
						pool.Enabled = false
					}
					c.Pools[blockLog.Address] = pool
					_, ok := processedPools[blockLog.Address]
					if !ok {
						processedPools[blockLog.Address] = pool
					}
				}
			}
			collectProtocolEvent, err := contract.ParseCollectProtocol(blockLog)
			if err == nil {
				pool, ok := c.Pools[blockLog.Address]
				if ok {
					pool.Reserve0 = new(big.Int).Sub(pool.Reserve0, collectProtocolEvent.Amount0)
					pool.Reserve1 = new(big.Int).Sub(pool.Reserve1, collectProtocolEvent.Amount1)
					if pool.Reserve0.Cmp(zero) != 1 || pool.Reserve1.Cmp(zero) != 1 {
						pool.Enabled = false
					}
					if pool.Token0.String() == WETHAddress.String() && pool.Reserve0.Cmp(wethReserveLimit) == -1 {
						pool.Enabled = false
					}
					if pool.Token1.String() == WETHAddress.String() && pool.Reserve1.Cmp(wethReserveLimit) == -1 {
						pool.Enabled = false
					}
					c.Pools[blockLog.Address] = pool
					_, ok := processedPools[blockLog.Address]
					if !ok {
						processedPools[blockLog.Address] = pool
					}
				}
			}
			syncEvent, err := contract.ParseSync(blockLog)
			if err == nil {
				pool, ok := c.Pools[blockLog.Address]
				if ok {
					pool.Reserve0 = syncEvent.Reserve0
					pool.Reserve1 = syncEvent.Reserve1
					if pool.Reserve0.Cmp(zero) != 1 || pool.Reserve1.Cmp(zero) != 1 {
						pool.Enabled = false
					}
					if pool.Token0.String() == WETHAddress.String() && pool.Reserve0.Cmp(wethReserveLimit) == -1 {
						pool.Enabled = false
					}
					if pool.Token1.String() == WETHAddress.String() && pool.Reserve1.Cmp(wethReserveLimit) == -1 {
						pool.Enabled = false
					}
					c.Pools[blockLog.Address] = pool
					_, ok := processedPools[blockLog.Address]
					if !ok {
						processedPools[blockLog.Address] = pool
					}
				}
			}
		}
	}
	processedPoolsArr := []Pool{}
	for _, pool := range processedPools {
		processedPoolsArr = append(processedPoolsArr, pool)
	}
	return processedPoolsArr, nil
}

func (c *UniswapClient) findDoublePathFirstEffected(effectedPools []Pool, ch chan []string, wethPools []Pool) {
	paths := []string{}
	now := time.Now()
	addressCh := make(chan []string)
	for _, wethPool := range wethPools {
		go func(pool1 Pool, res chan []string) {
			addressPaths := []string{}
			var token1 common.Address
			if pool1.Token0 == WETHAddress {
				token1 = pool1.Token1
			}
			if pool1.Token1 == WETHAddress {
				token1 = pool1.Token0
			}
			for _, pool2 := range effectedPools {
				if pool2.Address == pool1.Address {
					continue
				}
				if (pool2.Token1 == WETHAddress && pool2.Token0 == token1) || (pool2.Token0 == WETHAddress && pool2.Token1 == token1) {
					addressPaths = append(addressPaths, strings.Join([]string{pool1.Address.String(), pool2.Address.String()}, "->"))
				}
			}
			res <- addressPaths
		}(wethPool, addressCh)
	}
	for i := 0; i < len(wethPools); i++ {
		foundPaths := <-addressCh
		paths = append(paths, foundPaths...)
	}
	log.Info().Float64("findDoublePathFirstEffected", time.Since(now).Seconds()).Msg("duration")
	ch <- paths
}

func (c *UniswapClient) findDoublePathLastEffected(effectedPools []Pool, ch chan []string, wethPools []Pool) {
	paths := []string{}
	addressCh := make(chan []string)
	now := time.Now()
	for _, effectedPool := range effectedPools {
		go func(pool1 Pool, res chan []string) {
			addressPaths := []string{}
			var token1 *common.Address
			if pool1.Token0 == WETHAddress {
				token1 = &pool1.Token1
			}
			if pool1.Token1 == WETHAddress {
				token1 = &pool1.Token0
			}
			if token1 == nil {
				res <- addressPaths
				return
			}
			for _, pool2 := range wethPools {
				if pool2.Address == pool1.Address {
					continue
				}
				if (pool2.Token1 == WETHAddress && pool2.Token0 == *token1) || (pool2.Token0 == WETHAddress && pool2.Token1 == *token1) {
					addressPaths = append(addressPaths, strings.Join([]string{pool1.Address.String(), pool2.Address.String()}, "->"))
				}
			}
			res <- addressPaths
		}(effectedPool, addressCh)
	}
	for i := 0; i < len(effectedPools); i++ {
		foundPaths := <-addressCh
		paths = append(paths, foundPaths...)
	}
	log.Info().Float64("findDoublePathLastEffected", time.Since(now).Seconds()).Msg("duration")
	ch <- paths
}

func (c *UniswapClient) findTriangularPathMidEffected(effectedPools []Pool, ch chan []string, wethPools []Pool) {
	paths := []string{}
	addressCh := make(chan []string)
	now := time.Now()
	for _, wethPool := range wethPools {
		go func(pool1 Pool, res chan []string) {
			addressPaths := []string{}
			var token1 common.Address
			if pool1.Token0 == WETHAddress {
				token1 = pool1.Token1
			}
			if pool1.Token1 == WETHAddress {
				token1 = pool1.Token0
			}
			for _, pool2 := range effectedPools {
				if pool2.Address == pool1.Address {
					continue
				}
				var token2 *common.Address
				if pool2.Token0 == token1 {
					token2 = &pool2.Token1
				}
				if pool2.Token1 == token1 {
					token2 = &pool2.Token0
				}
				if token2 == nil {
					continue
				}
				for _, pool3 := range wethPools {
					if pool3.Address == pool2.Address || pool3.Address == pool1.Address {
						continue
					}
					if (pool3.Token1 == WETHAddress && pool3.Token0 == *token2) || (pool3.Token0 == WETHAddress && pool3.Token1 == *token2) {
						addressPaths = append(addressPaths, strings.Join([]string{pool1.Address.String(), pool2.Address.String(), pool3.Address.String()}, "->"))
					}
				}
			}
			addressCh <- addressPaths
		}(wethPool, addressCh)
	}
	for i := 0; i < len(wethPools); i++ {
		foundPaths := <-addressCh
		paths = append(paths, foundPaths...)
	}
	log.Info().Float64("findTriangularPathMidEffected", time.Since(now).Seconds()).Msg("duration")
	ch <- paths
}

func (c *UniswapClient) findTriangularPathFirstEffected(effectedPools []Pool, ch chan []string, wethPools []Pool, allPools []Pool) {
	paths := []string{}
	now := time.Now()
	addressCh := make(chan []string)
	for _, effectedPool := range effectedPools {
		go func(pool1 Pool, res chan []string) {
			addressPaths := []string{}
			var token1 *common.Address
			if pool1.Token0 == WETHAddress {
				token1 = &pool1.Token1
			}
			if pool1.Token1 == WETHAddress {
				token1 = &pool1.Token0
			}
			if token1 == nil {
				res <- addressPaths
				return
			}
			for _, pool2 := range allPools {
				if pool2.Address == pool1.Address {
					continue
				}
				var token2 *common.Address
				if pool2.Token0 == *token1 {
					token2 = &pool2.Token1
				}
				if pool2.Token1 == *token1 {
					token2 = &pool2.Token0
				}
				if token2 == nil {
					continue
				}
				for _, pool3 := range wethPools {
					if pool1.Address == pool3.Address || pool2.Address == pool3.Address {
						continue
					}
					if (pool3.Token0 == WETHAddress && pool3.Token1 == *token2) || (pool3.Token1 == WETHAddress && pool3.Token0 == *token2) {
						addressPaths = append(addressPaths, strings.Join([]string{pool1.Address.String(), pool2.Address.String(), pool3.Address.String()}, "->"))
					}
				}
			}
			res <- addressPaths
		}(effectedPool, addressCh)
	}
	for i := 0; i < len(effectedPools); i++ {
		foundPaths := <-addressCh
		paths = append(paths, foundPaths...)
	}
	log.Info().Float64("findTriangularPathFirstEffected", time.Since(now).Seconds()).Msg("duration")
	ch <- paths
}

func (c *UniswapClient) findTriangularPathLastEffected(effectedPools []Pool, ch chan []string, wethPools []Pool, allPools []Pool) {
	now := time.Now()
	paths := []string{}
	addressCh := make(chan []string)
	for _, wethPool := range wethPools {
		go func(pool1 Pool, res chan []string) {
			addressPaths := []string{}
			var token1 common.Address
			if pool1.Token0 == WETHAddress {
				token1 = pool1.Token1
			}
			if pool1.Token1 == WETHAddress {
				token1 = pool1.Token0
			}
			for _, pool2 := range allPools {
				if pool2.Address == pool1.Address {
					continue
				}
				var token2 *common.Address
				if pool2.Token0 == token1 {
					token2 = &pool2.Token1
				}
				if pool2.Token1 == token1 {
					token2 = &pool2.Token0
				}
				if token2 == nil {
					continue
				}
				for _, pool3 := range effectedPools {
					if pool3.Address == pool2.Address || pool3.Address == pool1.Address {
						continue
					}
					if (pool3.Token1 == WETHAddress && pool3.Token0 == *token2) || (pool3.Token0 == WETHAddress && pool3.Token1 == *token2) {
						addressPaths = append(addressPaths, strings.Join([]string{pool1.Address.String(), pool2.Address.String(), pool3.Address.String()}, "->"))
					}
				}
			}
			res <- addressPaths
		}(wethPool, addressCh)
	}
	for i := 0; i < len(wethPools); i++ {
		foundPaths := <-addressCh
		paths = append(paths, foundPaths...)
	}
	log.Info().Float64("findTriangularPathLastEffected", time.Since(now).Seconds()).Msg("duration")
	ch <- paths
}

func (c *UniswapClient) CalculateWethAndAllPools() ([]Pool, []Pool) {
	allPools := []Pool{}
	wethPools := []Pool{}
	for _, pool := range c.Pools {
		if !pool.Enabled {
			continue
		}
		if pool.Token1 == WETHAddress || pool.Token0 == WETHAddress {
			wethPools = append(wethPools, pool)
		}
		allPools = append(allPools, pool)
	}
	return allPools, wethPools
}

func (c *UniswapClient) CalculateActivePoolAddresses() []common.Address {
	addresses := c.FactoryAddresses
	for _, pool := range c.Pools {
		if !pool.Enabled {
			continue
		}
		addresses = append(addresses, pool.Address)
	}
	return addresses
}

func (c *UniswapClient) FindPaths(effectedPools []Pool) []string {
	paths := []string{}
	addedMap := make(map[string]bool)
	ch := make(chan []string)
	allPools, wethPools := c.CalculateWethAndAllPools()
	go c.findDoublePathFirstEffected(effectedPools, ch, wethPools)
	go c.findDoublePathLastEffected(effectedPools, ch, wethPools)
	go c.findTriangularPathFirstEffected(effectedPools, ch, wethPools, allPools)
	go c.findTriangularPathMidEffected(effectedPools, ch, wethPools)
	go c.findTriangularPathLastEffected(effectedPools, ch, wethPools, allPools)
	for i := 0; i < 5; i++ {
		foundPaths := <-ch
		for _, path := range foundPaths {
			_, ok := addedMap[path]
			if !ok {
				addedMap[path] = true
				paths = append(paths, path)
			}
		}
	}
	return paths
}

func (c *UniswapClient) calculateOutcomeForPath(path string, ch chan ArbitrageTx) {
	addressesAsStr := strings.Split(path, "->")
	poolAddresses := []common.Address{}
	pools := []Pool{}
	types := []*big.Int{}
	quoters := []common.Address{}
	quoteParams := []contracts.UniswapBotV2QuoteParams{}
	rawContract := contracts.UniswapBotV2Raw{Contract: c.BotContract}
	for _, addressStr := range addressesAsStr {
		pool := c.Pools[common.HexToAddress(addressStr)]
		if pool.Type == "v2" {
			quoters = append(quoters, AddressZero)
		} else {
			quoters = append(quoters, UniswapV3Quoter)
		}
		pools = append(pools, pool)
		poolAddresses = append(poolAddresses, pool.Address)
		if pool.Type == "v2" {
			types = append(types, big.NewInt(0))
		} else {
			types = append(types, big.NewInt(1))
		}
	}
	tx := ArbitrageTx{
		Path:               path,
		BorrowTokenAddress: WETHAddress,
		Pools:              poolAddresses,
		Types:              types,
		Profit:             big.NewInt(0),
		Ratio:              big.NewFloat(0),
	}
	for i := 1; i < 21; i++ {
		var reserve *big.Int
		if pools[0].Token1 == WETHAddress {
			reserve = pools[0].Reserve1
		} else {
			reserve = pools[0].Reserve0
		}
		amount1 := new(big.Int).Mul(reserve, big.NewInt(int64(i)))
		amount := new(big.Int).Div(amount1, big.NewInt(int64(100)))
		quoteParams = append(quoteParams, contracts.UniswapBotV2QuoteParams{
			Pools:   poolAddresses,
			Quoters: quoters,
			Amount:  amount,
			TokenIn: WETHAddress,
		})
	}
	var out []interface{}
	err := rawContract.Call(nil, &out, "multiQuote", quoteParams)
	if err != nil {
		ch <- tx
		return
	}
	outcomes := out[0].([][]*big.Int)

	for i, param := range quoteParams {
		outcome := outcomes[i]
		lastOut := outcome[len(outcome)-1]
		profit := new(big.Int).Sub(lastOut, param.Amount)
		if profit.Cmp(tx.Profit) == 1 {
			tx.Profit = profit
			tx.AmountOut = lastOut
			tx.BorrowAmount = param.Amount
			tx.Profit = profit
			tx.Valid = true
			profitFloat := new(big.Float).SetInt(tx.Profit)
			amountFloat := new(big.Float).SetInt(tx.BorrowAmount)
			ratio1 := new(big.Float).Mul(profitFloat, big.NewFloat(100))
			ratio := new(big.Float).Quo(ratio1, amountFloat)
			tx.Ratio = ratio
		}
	}
	ch <- tx
}

func (c *UniswapClient) calculateOutcomes(paths []string) []ArbitrageTx {
	txs := []ArbitrageTx{}
	ch := make(chan ArbitrageTx)
	for _, path := range paths {
		go c.calculateOutcomeForPath(path, ch)
	}
	for i := 0; i < len(paths); i++ {
		tx := <-ch
		txs = append(txs, tx)
	}
	return txs
}

func (c *UniswapClient) Run() error {
	newPools, err := c.InitializePools()
	if err != nil {
		return err
	}
	err = c.SetReserves(newPools)
	if err != nil {
		return err
	}
	headerCh := make(chan *types.Header)
	sub, err := c.wsClient.SubscribeNewHead(c.ctx, headerCh)
	if err != nil {
		return err
	}
	defer func() {
		sub.Unsubscribe()
	}()
	for {
		select {
		case blockHeader := <-headerCh:
			now := time.Now()
			hash := blockHeader.Hash()
			blockNumber, err := c.client.BlockNumber(c.ctx)
			if err != nil {
				return err
			}
			logs := []types.Log{}
			allAddresses := c.CalculateActivePoolAddresses()
			batchSize := 100000
			for i := 0; i < len(allAddresses); i += batchSize {
				var addresses []common.Address
				if i+batchSize < len(allAddresses) {
					addresses = allAddresses[i : i+batchSize]
				} else {
					addresses = allAddresses[i:]
				}
				partLogs, err := c.client.FilterLogs(c.ctx, ethereum.FilterQuery{
					FromBlock: big.NewInt(int64(c.LastSeenBlock)),
					Addresses: addresses,
				})
				if err != nil {
					return err
				}
				logs = append(logs, partLogs...)
			}
			log.Info().Float64("untilLogs", time.Since(now).Seconds()).Msg("untilLogs")
			effectedPools, err := c.ResolveLogs(logs, hash)
			if err != nil {
				return err
			}
			log.Info().Float64("untilResolve", time.Since(now).Seconds()).Msg("untilResolve duration")
			c.LastSeenBlock = blockNumber
			log.Info().Int("totalLogs", len(logs)).Msg("log summary")
			foundPaths := c.FindPaths(effectedPools)
			log.Info().Float64("untilOutcomes", time.Since(now).Seconds()).Msg("paths duration")
			log.Info().Int("totalPaths", len(foundPaths)).Msg("path summary")
			txs := c.calculateOutcomes(foundPaths)
			log.Info().Float64("untilSendTx", time.Since(now).Seconds()).Msg("outcomes duration")
			for _, tx := range txs {
				if !tx.Valid {
					continue
				}
				txhash, gasCost, bribe, err := c.SendTransaction(tx)
				log.Info().Interface("tx", tx).Interface("gasCost", gasCost).Interface("bribe", bribe).Msg("possible trade")
				if err != nil {
					log.Info().Err(err).Msg("can not send tx")
					if strings.Contains(err.Error(), "Malicious Pool: ") {
						errMsg := err.Error()
						poolAddress := errMsg[len(errMsg)-42:]
						pool := c.Pools[common.HexToAddress(poolAddress)]
						pool.Enabled = false
						c.Pools[pool.Address] = pool
					}
					continue
				}
				message := "Profitable Trade"
				message += "Pools: " + tx.Path
				message += "Profit Ratio: %" + tx.Ratio.String()
				message += "Loan Amount: " + tx.BorrowAmount.String()
				message += "Loan Payment: " + tx.BorrowAmount.String()
				message += "Amount Out: " + tx.AmountOut.String()
				message += "Gas Cost: " + gasCost.String()
				message += "Profit: " + tx.Profit.String()
				message += fmt.Sprintf(TxFormat, txhash.String())
				err = c.Notify(message)
				if err != nil {
					log.Info().Err(err).Msg("notify problem")
				}
			}
			log.Info().Float64("totalDuration", time.Since(now).Seconds()).Msg("duration")
		case err := <-sub.Err():
			if strings.Contains(err.Error(), "read: connection reset by peer") {
				log.Info().Msg("Reconnecting...")
				return c.Run()
			}
			if strings.Contains(err.Error(), "i/o timeout") {
				log.Info().Msg("Reconnecting...")
				return c.Run()
			}
			return err
		}
	}
}

func (c *UniswapClient) SendTransaction(tx ArbitrageTx) (*common.Hash, *big.Int, *big.Int, error) {
	gasCost := big.NewInt(0)
	bribe := big.NewInt(0)

	opts, err := bind.NewKeyedTransactorWithChainID(c.privKey, c.chainId)
	if err != nil {
		return nil, gasCost, bribe, err
	}
	opts.NoSend = true
	fakeTx, err := c.arbitrageContract.StartArbitrage(opts, tx.BorrowTokenAddress, tx.BorrowAmount, tx.Pools, tx.Types, tx.AmountOut, BribePercent)
	if err != nil {
		return nil, gasCost, bribe, err
	}
	bribe = new(big.Int).Mul(tx.Profit, BribePercent)
	bribe = new(big.Int).Div(bribe, big.NewInt(100))
	maxCost := new(big.Int).Mul(fakeTx.GasFeeCap(), big.NewInt(int64(fakeTx.Gas())))
	realProfit := new(big.Int).Sub(tx.Profit, maxCost)
	realProfit = new(big.Int).Sub(realProfit, bribe)
	if realProfit.Cmp(big.NewInt(0)) != 1 {
		return nil, maxCost, bribe, errors.New("gas cost is higher")
	}
	opts.NoSend = false
	realTx, err := c.arbitrageContract.StartArbitrage(opts, tx.BorrowTokenAddress, tx.BorrowAmount, tx.Pools, tx.Types, tx.AmountOut, BribePercent)
	if err != nil {
		return nil, maxCost, bribe, err
	}
	txHash := realTx.Hash()

	return &txHash, maxCost, bribe, nil
}

func (c *UniswapClient) Notify(message string) error {
	baseUrl := "https://api.telegram.org"
	resource := "/bot" + c.botToken + "/sendMessage"
	params := url.Values{}
	params.Add("chat_id", c.chatId)
	params.Add("parse_mode", "Markdown")
	params.Add("text", message)

	u, err := url.ParseRequestURI(baseUrl)
	if err != nil {
		return err
	}

	u.Path = resource

	u.RawQuery = params.Encode()
	urlStr := fmt.Sprintf("%v", u)
	_, err = http.Get(urlStr)
	if err != nil {
		return err
	}
	return nil
}
