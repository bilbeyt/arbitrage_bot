FROM golang:1.21-alpine3.17 as goDependencies

COPY ./src/go.mod /deps/go.mod
COPY ./src/go.sum /deps/go.sum
WORKDIR /deps
RUN go install github.com/ethereum/go-ethereum/cmd/abigen@latest
RUN go mod download

FROM node:20.9.0-alpine3.17 as npmDependencies
COPY contracts/package.json /deps/package.json
COPY contracts/package-lock.json /deps/package-lock.json
WORKDIR /deps
RUN npm i

FROM ghcr.io/foundry-rs/foundry:latest as contracts
COPY contracts /deps/contracts
COPY .git /deps/.git
COPY src/Makefile /deps/src/Makefile
WORKDIR /deps/src
RUN apk add --no-cache make
RUN make compile-contracts

FROM golang:1.21-alpine3.17 as appBuilder
COPY src /src
COPY --from=npmDependencies /deps/node_modules /contracts/node_modules
COPY --from=contracts /deps/contracts/out /contracts/out
COPY --from=goDependencies /go /go
RUN apk add --no-cache make jq
WORKDIR /src
RUN mkdir /abis
RUN make abi && make bytecode && make abigen && make build

FROM golang:1.21-alpine3.17
COPY --from=appBuilder /src/bin/arbitrage_bot /bin/arbitrage_bot
COPY --from=appBuilder /src/data /go/data
RUN chmod +x /bin/arbitrage_bot
CMD ["arbitrage_bot"]