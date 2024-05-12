package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"mev_bot/clients"

	"github.com/joho/godotenv"
	"github.com/rs/zerolog/log"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	envFile := os.Getenv("ENV_FILE")
	err := godotenv.Load(envFile)
	if err != nil {
		log.Fatal().Msg("Error loading .env file")
	}
	rpcURL := os.Getenv("RPC_URL")
	botToken := os.Getenv("BOT_TOKEN")
	chatId := os.Getenv("CHAT_ID")
	mevContractAddress := os.Getenv("MEV_ADDRESS")
	privKey := os.Getenv("PRIV_KEY")
	wsURL := os.Getenv("WS_URL")
	historyUrl := os.Getenv("HISTORY_RPC_URL")
	client, err := clients.NewUniswapClient(rpcURL, wsURL, historyUrl, botToken, chatId, mevContractAddress, privKey, ctx)
	if err != nil {
		log.Fatal().Err(err).Msg("can not get client")
	}
	err = client.Run()
	if err != nil {
		_ = client.SaveState()
		log.Info().Err(err).Msg("test")
	}
}
