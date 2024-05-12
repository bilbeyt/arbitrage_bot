.POSIX:

GO		?= go
BINDIR		?= ./bin
EXECUTABLE	?= ${BINDIR}/arbitrage_bot
GOPATH          ?= $(${GO} env GOPATH)

build:
	${GO} build ${GOFLAGS} -o ${EXECUTABLE}

compile-contracts:
	cd ../contracts && git submodule update --init
	sed -i'' -e 's/..\/solidity-utils\/openzeppelin\/IERC20.sol/@openzeppelin\/contracts\/token\/ERC20\/IERC20.sol/' ../contracts/lib/balancer-v2-monorepo/pkg/interfaces/contracts/vault/IVault.sol
	sed -i'' -e 's/..\/solidity-utils\/openzeppelin\/IERC20.sol/@openzeppelin\/contracts\/token\/ERC20\/IERC20.sol/' ../contracts/lib/balancer-v2-monorepo/pkg/interfaces/contracts/vault/IFlashLoanRecipient.sol
	sed -i'' -e 's/..\/solidity-utils\/openzeppelin\/IERC20.sol/@openzeppelin\/contracts\/token\/ERC20\/IERC20.sol/' ../contracts/lib/balancer-v2-monorepo/pkg/interfaces/contracts/vault/IProtocolFeesCollector.sol
	sed -i'' -e 's/..\/openzeppelin\/IERC20.sol/@openzeppelin\/contracts\/token\/ERC20\/IERC20.sol/' ../contracts/lib/balancer-v2-monorepo/pkg/interfaces/contracts/solidity-utils/misc/IWETH.sol
	cd ../contracts && forge build --force

abi:
	jq ".abi" ../contracts/out/IEvents.sol/IEvents.json > ../abis/IEvents.abi
	jq ".abi" ../contracts/out/UniswapBot.sol/UniswapBotV2.json > ../abis/UniswapBotV2.abi


bytecode:
	jq -r ".bytecode.object" ../contracts/out/UniswapBot.sol/UniswapBotV2.json > ../abis/UniswapBotV2.bytecode
	
abigen:

	abigen --abi ../abis/IEvents.abi --pkg contracts --type IEvents --out contracts/IEvents.go

	abigen --abi ../abis/UniswapBotV2.abi --bin ../abis/UniswapBotV2.bytecode --pkg contracts --type UniswapBotV2 --out contracts/UniswapBotV2.go

clean:
	rm -f ${EXECUTABLE}

.PHONY: build clean abigen abi compile-contracts