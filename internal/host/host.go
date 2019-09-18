package host

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agencyenterprise/gossip-host/internal/config"
	"github.com/agencyenterprise/gossip-host/pkg/logger"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/routing"
	kaddht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	lconfig "github.com/libp2p/go-libp2p/config"
	"github.com/libp2p/go-libp2p/p2p/discovery"
)

type mdnsNotifee struct {
	h   host.Host
	ctx context.Context
}

// HandlePeerFound...
func (m *mdnsNotifee) HandlePeerFound(pi peer.AddrInfo) {
	m.h.Connect(m.ctx, pi)
}

// Start starts a new gossip host
func Start(conf *config.Config) error {
	if conf == nil {
		logger.Error("nil config")
		return config.ErrNilConfig
	}

	var lOpts []lconfig.Option

	// 1. create a context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 2. create transports
	transports, err := parseTransportOptions(conf.Host.Transports)
	if err != nil {
		logger.Errorf("err parsing transports\n%v", err)
		return err
	}
	lOpts = append(lOpts, transports)

	// 3. create muxers
	muxers, err := parseMuxerOptions(conf.Host.Muxers)
	if err != nil {
		logger.Errorf("err parsing muxers\n%v", err)
		return err
	}
	lOpts = append(lOpts, muxers)

	// 4. create security
	security, err := parseSecurityOptions(conf.Host.Security)
	if err != nil {
		logger.Errorf("err parsing security\n%v", err)
		return err
	}
	lOpts = append(lOpts, security)

	// 5. add listen addresses
	if len(conf.Host.Listens) > 0 {
		lOpts = append(lOpts, libp2p.ListenAddrStrings(conf.Host.Listens...))
	}

	// 6. create router
	var dht *kaddht.IpfsDHT
	newDHT := func(h host.Host) (routing.PeerRouting, error) {
		var err error
		dht, err = kaddht.New(ctx, h)
		if err != nil {
			logger.Errorf("err creating new kaddht\n%v", err)
		}

		return dht, err
	}
	routing := libp2p.Routing(newDHT)
	lOpts = append(lOpts, routing)

	if conf.Host.DisableRelay {
		lOpts = append(lOpts, libp2p.DisableRelay())
	}

	// 7. build the libp2p host
	host, err := libp2p.New(ctx, lOpts...)
	if err != nil {
		logger.Errorf("err creating new libp2p host\n%v", err)
		return err
	}

	// 8. build the gossip pub/sub
	ps, err := pubsub.NewGossipSub(ctx, host)
	if err != nil {
		logger.Errorf("err creating new gossip sub\n%v", err)
		return err
	}
	sub, err := ps.Subscribe(pubsubTopic)
	if err != nil {
		logger.Errorf("err subscribing\n%v", err)
		return err
	}
	go pubsubHandler(ctx, sub)

	for _, addr := range host.Addrs() {
		logger.Infof("Listening on %v", addr)
	}

	// 9. Connect to peers
	if err := bootstrapPeers(ctx, host, conf.Host.Peers); err != nil {
		logger.Errorf("err bootstrapping peers\n%v", err)
		return err
	}

	// 10. create discovery service
	mdns, err := discovery.NewMdnsService(ctx, host, time.Second*10, "")
	if err != nil {
		logger.Errorf("err discovering\n%v", err)
		return err
	}
	mdns.RegisterNotifee(&mdnsNotifee{h: host, ctx: ctx})

	// note: is there a reason this is after the creation of the discovery service, or can it be moved up with dht initialization?
	if err = dht.Bootstrap(ctx); err != nil {
		logger.Errorf("err bootstrapping\n%v", err)
		return err
	}

	donec := make(chan struct{}, 1)
	go chatInputLoop(ctx, host, ps, donec)

	// 11. capture the ctrl+c signal
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT)

	// 12. start the server
	select {
	case <-stop:
		logger.Info("Received stop signal from os. Shutting down...")
		host.Close()

	case <-donec:
		logger.Info("shutting down...")
		host.Close()
	}

	return nil
}