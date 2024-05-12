package clients

import (
	"encoding/json"
	"fmt"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/rs/zerolog/log"
	"math/big"
	"mev_bot/contracts"
	"net/http"
	"os"
	"strings"
	"time"
)

func GetV2Pools(res chan<- []Pool, contract *contracts.IEvents, currentBlockNumber uint64, latestSyncBlock uint64) {
	log.Info().Msg("Started getting pools for v2")
	pools := []Pool{}
	ch := make(chan []Pool)
	callCount := 0
	for i := latestSyncBlock; i < currentBlockNumber; i += 10000 {
		callCount += 1
		i := i
		go func(ch chan<- []Pool) {
			if i+10000 < currentBlockNumber {
				getV2Pool(contract, i, i+9999, ch)
			} else {
				getV2Pool(contract, i, currentBlockNumber, ch)
			}
		}(ch)
	}
	for i := 0; i < callCount; i++ {
		foundPairs := <-ch
		if len(foundPairs) > 0 {
			pools = append(pools, foundPairs...)
		}
	}
	log.Info().Msg("Finished getting pools for v2")
	res <- pools
}

func GetV3Pools(res chan<- []Pool, contract *contracts.IEvents, currentBlockNumber uint64, latestSyncBlock uint64) {
	log.Info().Msg("Started getting pools for v3")

	pairs := []Pool{}
	ch := make(chan []Pool)
	callCount := 0

	for i := latestSyncBlock; i < currentBlockNumber; i += 10000 {
		callCount += 1
		i := i
		go func(ch chan<- []Pool) {
			if i+10000 < currentBlockNumber {
				getV3Pool(contract, i, i+9999, ch)
			} else {
				getV3Pool(contract, i, currentBlockNumber, ch)
			}
		}(ch)
	}
	for i := 0; i < callCount; i++ {
		foundPairs := <-ch
		if len(foundPairs) > 0 {
			pairs = append(pairs, foundPairs...)
		}
	}
	log.Info().Msg("Finished getting pools for v3")
	res <- pairs
}

func getV3Pool(contract *contracts.IEvents, startIndex uint64, endIndex uint64, ch chan<- []Pool) {
	pools := []Pool{}
	filter := bind.FilterOpts{
		Start: startIndex,
		End:   &endIndex,
	}
	logs, err := contract.FilterPoolCreated(&filter, nil, nil, nil)
	if err != nil {
		log.Fatal().Err(err).Msg("can not get v3 pools")
	}
	for {
		if logs.Event != nil {
			pool := Pool{
				Token0:  logs.Event.Token0,
				Token1:  logs.Event.Token1,
				Address: logs.Event.Pool,
				Fee:     logs.Event.Fee,
				Type:    "v3",
				Enabled: true,
			}
			pools = append(pools, pool)
		}
		ok := logs.Next()
		if !ok {
			break
		}
	}
	ch <- pools
}

func getV2Pool(contract *contracts.IEvents, startIndex uint64, endIndex uint64, ch chan<- []Pool) {
	pools := []Pool{}
	filter := bind.FilterOpts{
		Start: startIndex,
		End:   &endIndex,
	}
	logs, err := contract.FilterPairCreated(&filter, nil, nil)
	if err != nil {
		log.Fatal().Err(err).Msg("can not get v2 pools")
	}
	for {
		if logs.Event != nil {
			pool := Pool{
				Token0:  logs.Event.Token0,
				Token1:  logs.Event.Token1,
				Address: logs.Event.Pair,
				Type:    "v2",
				Enabled: true,
			}
			pools = append(pools, pool)
		}
		ok := logs.Next()
		if !ok {
			break
		}
	}
	ch <- pools
}

type ArbitrageTx struct {
	Path               string
	BorrowTokenAddress common.Address
	BorrowAmount       *big.Int
	Pools              []common.Address
	Types              []*big.Int
	AmountOut          *big.Int
	Ratio              *big.Float
	Profit             *big.Int
	Valid              bool
}

type Token struct {
	Symbol          string `json:"symbol"`
	ContractAddress string `json:"contract_address"`
	Rank            int    `json:"rank"`
}

type TokenPreview struct {
	ID     int    `json:"id"`
	Symbol string `json:"symbol"`
	Rank   int    `json:"cmc_rank"`
}

type TokenContractPlatform struct {
	Name string `json:"name"`
}

type TokenContractDetail struct {
	ContractAddress string                `json:"contract_address"`
	Platform        TokenContractPlatform `json:"platform"`
}

type TokenDetail struct {
	ID                int                   `json:"id"`
	ContractAddresses []TokenContractDetail `json:"contract_address"`
}

type TokenListRes struct {
	Status map[string]interface{} `json:"status"`
	Data   []TokenPreview         `json:"data"`
}

type TokenDetailRes struct {
	Status map[string]interface{}   `json:"status"`
	Data   map[string][]TokenDetail `json:"data"`
}

func RetrieveTopTokens() error {
	tokens := []Token{}
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	filePath := wd + "/data/tokens.json"
	url := "https://pro-api.coinmarketcap.com/v1/cryptocurrency/listings/latest?limit=500&sort=volume_24h&sort_dir=desc&aux=cmc_rank&CMC_PRO_API_KEY=bcb382b5-95b2-4efd-87a3-1e6fc80f2d13"
	resp, err := http.Get(url)
	var tokenListRes TokenListRes
	err = json.NewDecoder(resp.Body).Decode(&tokenListRes)
	if err != nil {
		return err
	}
	for index, preview := range tokenListRes.Data {
		if strings.Contains(preview.Symbol, ".") {
			continue
		}
		if preview.Symbol == "ETH" {
			continue
		}
		log.Info().Interface("preview", preview).Int("index", index).Msg("preview info")
		secondUrl := fmt.Sprintf("https://pro-api.coinmarketcap.com/v2/cryptocurrency/info?symbol=%s&CMC_PRO_API_KEY=bcb382b5-95b2-4efd-87a3-1e6fc80f2d13", preview.Symbol)
		var tokenDetailRes TokenDetailRes
		for {
			resp, err := http.Get(secondUrl)
			err = json.NewDecoder(resp.Body).Decode(&tokenDetailRes)
			if err != nil {
				return err
			}
			if tokenDetailRes.Status["error_code"].(float64) == float64(0) {
				break
			} else {
				time.Sleep(1 * time.Minute)
			}
		}
		info := tokenDetailRes.Data[preview.Symbol]
		log.Info().Interface("detail", tokenDetailRes).Msg("detail info")
		for _, detail := range info {
			if detail.ID != preview.ID {
				continue
			}
			for _, address := range detail.ContractAddresses {
				log.Info().Interface("address", address).Msg("address info")
				if address.Platform.Name != "Ethereum" {
					continue
				}
				tokens = append(tokens, Token{
					Symbol:          preview.Symbol,
					Rank:            preview.Rank,
					ContractAddress: address.ContractAddress,
				})
			}
		}
	}
	file, err := json.MarshalIndent(tokens, "", " ")
	if err != nil {
		return err
	}
	err = os.WriteFile(filePath, file, 0644)
	return nil
}
