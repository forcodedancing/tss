package common

import (
	"github.com/binance-chain/tss-lib/tss"
)

// Transportation layer of TssClient provide Broadcast and Send method over p2p network
// ReceiveCh() provides msgs this client received
// TODO: consider a ControlCh() to expose ready&err msgs to application?
type Transporter interface {
	NodeKey() []byte // return party's p2p private key, encryption it together with keygen secret so that when move party to other machine, we only copy encrypted file
	Broadcast(msg tss.Message) error
	Send(msg tss.Message, to TssClientId) error
	ReceiveCh() <-chan P2pMessageWithFrom // messages have received !consumer of this channel should not taking too long!
	Shutdown() error
}
