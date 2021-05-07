package p2p

import (
	"fmt"
	tmrand "github.com/tendermint/tendermint/libs/rand"
)

const (
	MConnProtocol Protocol = "mconn"
	TCPProtocol   Protocol = "tcp"
)

type ChannelID uint16

func CreateRoutableAddr() (addr string, netAddr *NetAddress) {
	for {
		var err error
		addr = fmt.Sprintf("%X@%v.%v.%v.%v:26656",
			tmrand.Bytes(20),
			tmrand.Int()%256,
			tmrand.Int()%256,
			tmrand.Int()%256,
			tmrand.Int()%256)
		netAddr, err = NewNetAddressString(addr)
		if err != nil {
			panic(err)
		}
		if netAddr.Routable() {
			break
		}
	}
	return
}
