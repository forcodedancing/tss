package p2p

import (
	"context"
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/binance-chain/tss-lib/protob"
	"github.com/binance-chain/tss-lib/tss"
	"github.com/golang/protobuf/proto"
	"github.com/ipfs/go-datastore"
	"github.com/libp2p/go-libp2p"
	relay "github.com/libp2p/go-libp2p-circuit"
	ifconnmgr "github.com/libp2p/go-libp2p-core/connmgr"
	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/peerstore"
	"github.com/libp2p/go-libp2p-core/protocol"
	libp2pdht "github.com/libp2p/go-libp2p-kad-dht"
	opts "github.com/libp2p/go-libp2p-kad-dht/opts"
	"github.com/libp2p/go-libp2p-peerstore/pstoremem"
	swarm "github.com/libp2p/go-libp2p-swarm"
	"github.com/libp2p/go-yamux"
	"github.com/multiformats/go-multiaddr"

	"github.com/binance-chain/tss/common"
)

const (
	partyProtocolId     = "/tss/party/0.0.1"
	bootstrapProtocolId = "/tss/bootstrap/0.0.1"
	loggerName          = "trans"
	receiveChBufSize    = 500
)

// P2P implementation of Transporter
type p2pTransporter struct {
	ifconnmgr.NullConnMgr

	nodeKey []byte
	ctx     context.Context

	// for bootstrap
	bootstrapper *common.Bootstrapper

	pathToRouteTable      string
	expectedPeers         []peer.ID
	streams               sync.Map // map[peer.ID.Pretty()]network.Stream
	encoders              sync.Map // map[common.TssClientId]*gob.Encoder
	numOfStreams          int32    // atomic int of len(streams)
	numOfBootstrapStreams int32    // atomic int of len(bootstrapStreams)
	bootstrapPeers        []multiaddr.Multiaddr
	relayPeers            []multiaddr.Multiaddr
	notifee               network.Notifiee

	// sanity check related field
	broadcastSanityCheck bool
	sanityCheckMtx       *sync.Mutex
	ioMtx                *sync.Mutex
	pendingCheckHashMsg  map[p2pMessageKey]*P2pMessageWithHash   // guarded by sanityCheckMtx
	receivedPeersHashMsg map[p2pMessageKey][]*P2pMessageWithHash // guarded by sanityCheckMtx

	receiveCh chan common.P2pMessageWithFrom
	host      host.Host

	closed chan bool
}

type p2pMessageKey string

func keyOf(m P2pMessageWithHash) p2pMessageKey {
	return p2pMessageKey(fmt.Sprintf("%s%s", m.From.ID, m.Type()))
}

var _ ifconnmgr.ConnManager = (*p2pTransporter)(nil)
var _ common.Transporter = (*p2pTransporter)(nil)

// Constructor of p2pTransporter
// signers indicate which peers within config.ExpectedPeer should be connected (non-empty for regroup and sign, empty for keygen)
// Once this is done, the transportation is ready to use
func NewP2PTransporter(
	home, vault, nodeId string,
	bootstrapper *common.Bootstrapper,
	signers map[string]int,
	config *common.P2PConfig) common.Transporter {
	t := &p2pTransporter{}

	t.ctx = context.Background()
	if bootstrapper != nil {
		t.bootstrapper = bootstrapper
	}
	t.pathToRouteTable = path.Join(home, vault, "rt/")
	ps := pstoremem.NewPeerstore()
	t.setExpectedPeers(nodeId, signers, ps, config) // t.expectedPeers will be updated in this method
	t.bootstrapPeers = config.BootstrapPeers
	// TODO: relay addr need further confirm
	// The correct address should be /p2p-circuit/p2p/<dest ID> rather than /p2p-circuit/p2p/<relay ID>
	for _, relayPeerAddr := range config.RelayPeers {
		relayPeerInfo, err := peer.AddrInfoFromP2pAddr(relayPeerAddr)
		if err != nil {
			common.Panic(err)
		}
		relayAddr, err := multiaddr.NewMultiaddr("/p2p-circuit/p2p/" + relayPeerInfo.ID.Pretty())
		if err != nil {
			common.Panic(err)
		}
		t.relayPeers = append(t.relayPeers, relayAddr)
	}

	t.notifee = &cmNotifee{t}
	t.broadcastSanityCheck = config.BroadcastSanityCheck
	if t.broadcastSanityCheck {
		t.sanityCheckMtx = &sync.Mutex{}
		t.pendingCheckHashMsg = make(map[p2pMessageKey]*P2pMessageWithHash)
		t.receivedPeersHashMsg = make(map[p2pMessageKey][]*P2pMessageWithHash)
	}
	t.ioMtx = &sync.Mutex{}

	t.receiveCh = make(chan common.P2pMessageWithFrom, receiveChBufSize)
	// load private key of node id
	var privKey crypto.PrivKey
	pathToNodeKey := path.Join(home, vault, "node_key")
	if _, err := os.Stat(pathToNodeKey); err == nil {
		bytes, err := ioutil.ReadFile(pathToNodeKey)
		if err != nil {
			common.Panic(err)
		}
		privKey, err = crypto.UnmarshalPrivateKey(bytes)
		if err != nil {
			common.Panic(err)
		}
		t.nodeKey = bytes
	}

	addr, err := multiaddr.NewMultiaddr(config.ListenAddr)
	if err != nil {
		common.Panic(err)
	}

	host, err := libp2p.New(
		t.ctx,
		libp2p.Peerstore(ps),
		libp2p.ConnectionManager(t),
		libp2p.ListenAddrs(addr),
		libp2p.Identity(privKey),
		libp2p.EnableRelay(relay.OptDiscovery),
		libp2p.NATPortMap(), // actually I cannot find a case that NATPortMap can help, but in case some edge case, created it to save relay server performance
	)
	if err != nil {
		common.Panic(err)
	}
	host.SetStreamHandler(partyProtocolId, t.handleStream)
	host.SetStreamHandler(bootstrapProtocolId, t.handleSigner)
	t.host = host
	t.closed = make(chan bool)
	logger.Debug("Host created. We are:", host.ID())
	logger.Debug("listening on:", host.Addrs())
	logger.Info("waiting peers connection...")

	dht := t.setupDHTClient()
	if bootstrapper != nil {
		t.initBootstrapConnection(dht)
	} else {
		t.initConnection(dht)
	}

	return t
}

func (t *p2pTransporter) NodeKey() []byte {
	return t.nodeKey
}

func (t *p2pTransporter) Broadcast(msg tss.Message) error {
	logger.Debug("Broadcast: ", msg)
	var err error
	t.streams.Range(func(to, stream interface{}) bool {
		shouldSend := false
		if msg.GetTo() == nil {
			shouldSend = true
		} else {
			for _, dest := range msg.GetTo() {
				if to.(string) == dest.ID {
					shouldSend = true
					break
				}
			}
		}
		if shouldSend {
			if e := t.Send(msg, common.TssClientId(to.(string))); e != nil {
				err = e
				return false
			}
		}
		return true
	})
	return err
}

func (t *p2pTransporter) Send(msg tss.Message, to common.TssClientId) error {
	t.ioMtx.Lock()
	defer t.ioMtx.Unlock()

	logger.Debugf("Sending: %s, To: %s", msg, to)
	// TODO: stream.Write should be protected by their lock?
	stream, ok := t.streams.Load(to.String())
	if ok && stream != nil {
		//enc, _ := t.encoders.Load(to)
		//if err := enc.(*gob.Encoder).Encode(&msg); err != nil {
		//	// TODO: send an signal for quit
		//	logger.Errorf("failed to encode gob message: %v, sending quit", err)
		//	return err
		//}

		if out, err := msg.WireBytes(); err != nil {
			logger.Errorf("failed to encode protobuf message: %v, sending quit", err)
			return err
		} else {
			messageLength := int32(len(out))
			if err = binary.Write(stream.(network.Stream), binary.BigEndian, &messageLength); err != nil {
				return err
			}
			if _, err = stream.(network.Stream).Write(out); err != nil {
				return err
			}
		}

		logger.Debugf("Sent: %s, To: %s, Via (memory addr of stream): %p", msg, to, stream)
	}
	return nil
}

func (t p2pTransporter) ReceiveCh() <-chan common.P2pMessageWithFrom {
	return t.receiveCh
}

func (t p2pTransporter) Shutdown() (err error) {
	logger.Info("Closing p2ptransporter")

	if err := t.host.Close(); err != nil {
		return err
	}
	close(t.closed)
	return
}

func (t p2pTransporter) closeStream(key, stream interface{}) bool {
	if stream == nil {
		return true
	}
	if e := stream.(network.Stream).Close(); e != nil {
		logger.Error("err for closing stream: %v", e)
		return false
	}
	return true
}

// implementation of ConnManager

func (t *p2pTransporter) Notifee() network.Notifiee {
	return t.notifee
}

// implementation of

func (t *p2pTransporter) handleStream(stream network.Stream) {
	pid := stream.Conn().RemotePeer().Pretty()
	logger.Infof("Connected to: %s(%s)", pid, stream.Protocol())

	if _, loaded := t.streams.LoadOrStore(pid, stream); !loaded {
		t.encoders.Store(common.TssClientId(pid), gob.NewEncoder(stream))
		atomic.AddInt32(&t.numOfStreams, 1)
	}
}

func (t *p2pTransporter) handleSigner(stream network.Stream) {
	pid := stream.Conn().RemotePeer().Pretty()
	logger.Infof("Connected to: %s(%s)", pid, stream.Protocol())

	encoder := gob.NewEncoder(stream)
	// TODO: figure out why sometimes the localaddr is 0.0.0.0
	localAddr := stream.Conn().LocalMultiaddr().String()
	logger.Infof("local addr in message: %s", localAddr)
	localAddr = strings.Replace(localAddr, "0.0.0.0", "127.0.0.1", 1)
	if msg, err := common.NewBootstrapMessage(
		t.bootstrapper.ChannelId,
		t.bootstrapper.ChannelPassword,
		localAddr,
		common.PeerParam{
			ChannelId: common.TssCfg.ChannelId,
			Moniker:   common.TssCfg.Moniker,
			Msg:       common.TssCfg.Message,
			Id:        string(common.TssCfg.Id),
			N:         common.TssCfg.Parties,
			T:         common.TssCfg.Threshold,
			NewN:      common.TssCfg.NewParties,
			NewT:      common.TssCfg.NewThreshold,
			IsOld:     common.TssCfg.IsOldCommittee,
			IsNew:     !common.TssCfg.IsOldCommittee,
		}); err == nil {
		encoder.Encode(msg)
	} else {
		logger.Errorf("failed to encrypt bootstrap message: %v", err)
	}

	decoder := gob.NewDecoder(stream)
	var peerMsg common.BootstrapMessage
	decoder.Decode(&peerMsg)
	if err := t.bootstrapper.HandleBootstrapMsg(peerMsg); err != nil {
		// peer's channel id or channel password is not correct, we can wait them fix
		logger.Error(err)
		return
	}
}

func (t *p2pTransporter) readDataRoutine(pid string, stream network.Stream) {
	var messageLength int32
	//decoder := gob.NewDecoder(stream)
	for {
		err := binary.Read(stream.(network.Stream), binary.BigEndian, &messageLength)
		if err != nil {
			logger.Error("failed to read message bytes length: ", err)
		}
		payload := make([]byte, messageLength)
		readLength, err := stream.(network.Stream).Read(payload)
		if err != nil {
			logger.Error("failed to read protobuf message: %v", err)
		}
		if readLength != int(messageLength) {
			logger.Error("failed to read protobuf message: length doesn't match prefix")
		}
		var msg protob.Message
		if err := proto.Unmarshal(payload, &msg); err == nil {
			logger.Debugf("Received message: %s from peer: %s, via (memory addr of stream): %p", msg.String(), pid, stream)

			//switch m := msg.(type) {
			//case P2pMessageWithHash:
			//	if t.broadcastSanityCheck {
			//		key := keyOf(m)
			//		t.sanityCheckMtx.Lock()
			//		t.receivedPeersHashMsg[key] = append(t.receivedPeersHashMsg[key], &m)
			//		var numOfDest int
			//		if m.To == nil {
			//			numOfDest = len(t.expectedPeers)
			//		} else {
			//			numOfDest = len(m.To)
			//		}
			//		if t.verifiedPeersBroadcastMsgGuarded(key, numOfDest) {
			//			t.receiveCh <- t.pendingCheckHashMsg[key].originMsg
			//			delete(t.pendingCheckHashMsg, key)
			//		}
			//		t.sanityCheckMtx.Unlock()
			//	} else {
			//		logger.Errorf("peer %s configuration is not consistent - sanity check is enabled", pid)
			//	}
			//case tss.ParsedMessage:
			//	if t.broadcastSanityCheck && m.IsBroadcast() {
			//		// we cannot use gob encoding here because the type spec registered relies on message sequence
			//		// in other word, it might be not deterministic https://stackoverflow.com/a/33228913/1147187
			//		if jsonBytes, err := json.Marshal(msg); err == nil {
			//			hash := sha256.Sum256(jsonBytes)
			//			logger.Debugf("Encoded message %s: %x (hash: %x)", m, jsonBytes, hash)
			//			// TODO: ToOldCommittee is blindly set to false here
			//			msgWithHash := P2pMessageWithHash{tss.MessageMetadata{From: m.GetFrom(), To: m.GetTo()}, hash, msg}
			//			t.sanityCheckMtx.Lock()
			//			t.pendingCheckHashMsg[keyOf(msgWithHash)] = &msgWithHash
			//			var numOfDest int
			//			if m.GetTo() == nil {
			//				numOfDest = len(t.expectedPeers)
			//				for _, p := range t.expectedPeers {
			//					if p.Pretty() != m.GetFrom().ID {
			//						// send our hashing of this message
			//						var msgToSend tss.ParsedMessage
			//						msgToSend = msgWithHash
			//						t.Send(msgToSend, common.TssClientId(p.Pretty()))
			//					}
			//				}
			//			} else {
			//				numOfDest = len(m.GetTo())
			//				for _, p := range m.GetTo() {
			//					if p.ID != m.GetFrom().ID {
			//						// send our hashing of this message
			//						var msgToSend tss.ParsedMessage
			//						msgToSend = msgWithHash
			//						t.Send(msgToSend, common.TssClientId(p.ID))
			//					}
			//				}
			//			}
			//			if t.verifiedPeersBroadcastMsgGuarded(keyOf(msgWithHash), numOfDest) {
			//				t.receiveCh <- m
			//				delete(t.pendingCheckHashMsg, keyOf(msgWithHash))
			//			}
			//			t.sanityCheckMtx.Unlock()
			//		} else {
			//			common.Panic(fmt.Errorf("failed to marshal message: %s to json: %v", msg, err))
			//		}
			//	} else {
			//		t.receiveCh <- msg
			//	}
			//}

			t.receiveCh <- common.P2pMessageWithFrom{From: pid, OriginMsg: payload}
		} else {
			// TODO: figure out why this doesn't work
			switch err {
			case yamux.ErrConnectionReset:
				break // connManager would handle the reconnection
			default:
				logger.Error("failed to decode message: ", err)
			}

			//if yamuxErr, ok := err.(*yamux.YamuxError); ok {
			//	if yamuxErr.Error() == yamux.ErrConnectionReset.Error() {
			//		break
			//	} else {
			//		logger.Error("failed to decode message: ", err)
			//	}
			//} else {
			//	logger.Error("failed to decode message: ", err)
			//}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// guarded by t.sanityCheckMtx
func (t *p2pTransporter) verifiedPeersBroadcastMsgGuarded(key p2pMessageKey, numOfDest int) bool {
	if t.pendingCheckHashMsg[key] == nil {
		logger.Debugf("didn't receive the main message: %s yet", key)
		return false
	} else if len(t.receivedPeersHashMsg[key])+1 != numOfDest {
		logger.Debugf("didn't receive enough peer's hash messages: %s yet", key)
		return false
	} else {
		for _, hashMsg := range t.receivedPeersHashMsg[key] {
			if hashMsg.Hash != t.pendingCheckHashMsg[key].Hash {
				common.Panic(fmt.Errorf("someone in network is malicious")) // TODO: better logging, i.e. log which one is malicious in what way
			}
		}

		delete(t.receivedPeersHashMsg, key)
		return true
	}
}

func (t *p2pTransporter) initBootstrapConnection(dht *libp2pdht.IpfsDHT) {
	logger.Debugf("initialize bootstrap connection")
	for _, pid := range t.expectedPeers {
		// we only connect parties whose id greater than us
		if strings.Compare(t.host.ID().String(), pid.String()) >= 0 {
			continue
		}
		go t.connectRoutine(dht, pid, bootstrapProtocolId)
	}

	for {
		if t.bootstrapper.IsFinished() {
			break
		} else {
			time.Sleep(time.Second)
		}
	}
}

func (t *p2pTransporter) initConnection(dht *libp2pdht.IpfsDHT) {
	for _, pid := range t.expectedPeers {
		if stream, ok := t.streams.Load(pid.Pretty()); ok && stream != nil {
			continue
		}

		// we only connect parties whose id greater than us
		if strings.Compare(t.host.ID().String(), pid.String()) >= 0 {
			continue
		}
		go t.connectRoutine(dht, pid, partyProtocolId)
	}

	for atomic.LoadInt32(&t.numOfStreams) < int32(len(t.expectedPeers)) {
		time.Sleep(10 * time.Millisecond)
	}
	t.streams.Range(func(pid, stream interface{}) bool {
		go t.readDataRoutine(pid.(string), stream.(network.Stream))
		return true
	})
}

func (t *p2pTransporter) connectRoutine(dht *libp2pdht.IpfsDHT, pid peer.ID, protocolId string) {
	logger.Debugf("trying to connect with %s", pid.Pretty())
	timeout := time.NewTimer(15 * time.Minute)
	defer func() {
		timeout.Stop()
	}()

	for {
		select {
		case <-t.closed:
			break
		case <-timeout.C:
			break
		default:
			time.Sleep(1000 * time.Millisecond)
			if len(t.host.Peerstore().Addrs(pid)) == 0 {
				_, err := dht.FindPeer(t.ctx, pid)
				if err == nil {
					logger.Debug("Found peer:", pid)
				} else {
					logger.Warningf("Cannot resolve addr of peer: %s, err: %s", pid, err.Error())
					continue
				}

				if atomic.LoadInt32(&t.numOfStreams) == int32(len(t.expectedPeers)) {
					// if those peers have connected to us, we give up connect them
					return
				}
				logger.Debug("Connecting to:", pid)
				stream, err := t.host.NewStream(t.ctx, pid, protocol.ID(protocolId))

				if err != nil {
					logger.Info("Normal Connection failed:", err)
					if err := t.tryRelaying(pid, protocolId); err != nil {
						continue
					} else {
						return
					}
				} else {
					switch protocolId {
					case partyProtocolId:
						t.handleStream(stream)
					case bootstrapProtocolId:
						t.handleSigner(stream)
					}
					return
				}
			} else {
				err := t.host.Connect(t.ctx, peer.AddrInfo{pid, t.host.Peerstore().Addrs(pid)})
				if err != nil {
					if err != swarm.ErrDialBackoff {
						logger.Debugf("Direct Connection to %s failed, will retry, err: %v", pid.Pretty(), err)
					}
					continue
				} else {
					if atomic.LoadInt32(&t.numOfStreams) == int32(len(t.expectedPeers)) {
						// if those peers have connected to us, we give up connect them
						return
					}

					stream, err := t.host.NewStream(t.ctx, pid, protocol.ID(protocolId))
					if err != nil {
						logger.Info("Direct Connection failed, Will give up")
						common.Panic(err)
					} else {
						switch protocolId {
						case partyProtocolId:
							t.handleStream(stream)
						case bootstrapProtocolId:
							t.handleSigner(stream)
						}
						return
					}
				}
			}
		}
	}
}

func (t *p2pTransporter) tryRelaying(pid peer.ID, protocolId string) error {
	t.host.Network().(*swarm.Swarm).Backoff().Clear(pid)
	relayaddr, err := multiaddr.NewMultiaddr("/p2p-circuit/p2p/" + pid.Pretty())
	relayInfo := peer.AddrInfo{
		ID:    pid,
		Addrs: []multiaddr.Multiaddr{relayaddr},
	}
	err = t.host.Connect(t.ctx, relayInfo)
	if err != nil {
		logger.Warning("Relay Connection failed:", err)
		return err
	}
	stream, err := t.host.NewStream(t.ctx, pid, protocol.ID(protocolId))
	if err != nil {
		logger.Warning("Relay Stream failed:", err)
		return err
	}
	switch protocolId {
	case partyProtocolId:
		t.handleStream(stream)
	case bootstrapProtocolId:
		t.handleSigner(stream)
	}
	return nil
}

func (t *p2pTransporter) setupDHTClient() *libp2pdht.IpfsDHT {
	//ds, err := leveldb.NewDatastore(t.pathToRouteTable, nil)
	//if err != nil {
	//	common.Panic(err)
	//}
	ds := datastore.NewMapDatastore()

	kademliaDHT, err := libp2pdht.New(
		t.ctx,
		t.host,
		opts.Datastore(ds),
		opts.Client(true),
	)
	if err != nil {
		common.Panic(err)
	}

	// Connect to bootstrap peers
	for _, bootstrapAddr := range t.bootstrapPeers {
		bootstrapPeerInfo, err := peer.AddrInfoFromP2pAddr(bootstrapAddr)
		if err != nil {
			common.Panic(err)
		}
		if err := t.host.Connect(t.ctx, *bootstrapPeerInfo); err != nil {
			logger.Warning(err)
		} else {
			logger.Info("Connection established with bootstrap node:", *bootstrapPeerInfo)
		}
	}

	// Connect to relay peers to get NAT support
	// TODO: exclude relay peers that are same with bootstrap peers
	for _, relayAddr := range t.relayPeers {
		relayPeerInfo, err := peer.AddrInfoFromP2pAddr(relayAddr)
		if err != nil {
			common.Panic(err)
		}
		if err := t.host.Connect(t.ctx, *relayPeerInfo); err != nil {
			logger.Warning(err)
		} else {
			logger.Info("Connection established with relay node:", *relayPeerInfo)
		}
	}

	return kademliaDHT
}

func (t *p2pTransporter) setExpectedPeers(nodeId string, signers map[string]int, ps peerstore.Peerstore, config *common.P2PConfig) {
	mergedExpectedPeers := make(map[string]string) // peer -> addr
	for idx, expectedPeer := range config.ExpectedPeers {
		moniker := GetMonikerFromExpectedPeers(expectedPeer)
		if _, ok := signers[moniker]; ok || len(signers) == 0 {
			if len(config.PeerAddrs) > idx && config.PeerAddrs[idx] != "" {
				mergedExpectedPeers[expectedPeer] = config.PeerAddrs[idx]
			} else {
				mergedExpectedPeers[expectedPeer] = ""
			}
		}
	}
	for idx, expectedPeer := range config.ExpectedNewPeers {
		if len(config.NewPeerAddrs) > idx && config.NewPeerAddrs[idx] != "" {
			mergedExpectedPeers[expectedPeer] = config.NewPeerAddrs[idx]
		} else {
			mergedExpectedPeers[expectedPeer] = ""
		}
	}

	for expectedPeer, peerAddr := range mergedExpectedPeers {
		if pid, err := peer.IDB58Decode(string(GetClientIdFromExpecetdPeers(expectedPeer))); err != nil {
			common.Panic(err)
		} else {
			if pid.Pretty() == nodeId {
				continue
			}
			logger.Debugf("expect peer: %s", pid.Pretty())
			if peerAddr != "" {
				maddr, err := multiaddr.NewMultiaddr(peerAddr)
				if err != nil {
					logger.Errorf("invalid peeraddr: %s", peerAddr)
				} else {
					logger.Debugf("expect peer addr: %s", peerAddr)
					ps.AddAddr(pid, maddr, time.Hour)
				}
			}
			t.expectedPeers = append(t.expectedPeers, pid)
		}
	}
}
