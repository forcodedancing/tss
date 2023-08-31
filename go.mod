module github.com/bnb-chain/tss

go 1.12

require (
	github.com/bgentry/speakeasy v0.1.0
	github.com/bnb-chain/tss-lib/v2 v2.0.0
	github.com/btcsuite/btcd v0.20.0-beta
	github.com/btcsuite/btcutil v0.0.0-20190425235716-9e5f4b9a998d
	github.com/ipfs/go-cid v0.0.3 // indirect
	github.com/ipfs/go-datastore v0.0.5
	github.com/ipfs/go-ds-leveldb v0.0.1
	github.com/ipfs/go-log v0.0.1
	github.com/koron/go-ssdp v0.0.0-20180514024734-4a0ed625a78b
	github.com/libp2p/go-eventbus v0.1.0 // indirect
	github.com/libp2p/go-libp2p v0.3.0
	github.com/libp2p/go-libp2p-circuit v0.1.1
	github.com/libp2p/go-libp2p-core v0.2.2
	github.com/libp2p/go-libp2p-kad-dht v0.2.0
	github.com/libp2p/go-libp2p-peerstore v0.1.3
	github.com/libp2p/go-libp2p-swarm v0.2.0
	github.com/libp2p/go-yamux v1.2.3
	github.com/mattn/go-isatty v0.0.14
	github.com/mitchellh/mapstructure v1.5.0
	github.com/multiformats/go-multiaddr v0.0.4
	github.com/multiformats/go-multiaddr-dns v0.0.3 // indirect
	github.com/multiformats/go-multihash v0.0.7 // indirect
	github.com/phayes/freeport v0.0.0-20180830031419-95f893ade6f2
	github.com/pkg/errors v0.9.1
	github.com/spf13/cobra v1.5.0
	github.com/spf13/viper v1.12.0
	github.com/tendermint/tendermint v0.35.9
	github.com/whyrusleeping/go-logging v0.0.1 // indirect
	golang.org/x/crypto v0.5.0
	google.golang.org/protobuf v1.28.1
)

replace github.com/tendermint/tendermint => github.com/bnb-chain/bnc-tendermint v0.32.3-binance.7.0.20230830064832-049f57c7d047

exclude (
	github.com/btcsuite/btcd v0.23.0
	github.com/btcsuite/btcd/chaincfg/chainhash v1.0.0
	github.com/btcsuite/btcd/chaincfg/chainhash v1.0.1
)
