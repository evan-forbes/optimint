package node

import (
	"context"
	"fmt"
	"time"

	abci "github.com/lazyledger/lazyledger-core/abci/types"
	llcfg "github.com/lazyledger/lazyledger-core/config"
	"github.com/lazyledger/lazyledger-core/libs/clist"
	"github.com/lazyledger/lazyledger-core/libs/log"
	"github.com/lazyledger/lazyledger-core/libs/service"
	corep2p "github.com/lazyledger/lazyledger-core/p2p"
	"github.com/lazyledger/lazyledger-core/proxy"
	lltypes "github.com/lazyledger/lazyledger-core/types"
	"github.com/libp2p/go-libp2p-core/crypto"
	"go.uber.org/multierr"

	"github.com/lazyledger/optimint/config"
	"github.com/lazyledger/optimint/da"
	"github.com/lazyledger/optimint/da/registry"
	"github.com/lazyledger/optimint/mempool"
	"github.com/lazyledger/optimint/p2p"
	"github.com/lazyledger/optimint/store"
	"github.com/lazyledger/optimint/types"
)

type Node struct {
	service.BaseService
	eventBus *lltypes.EventBus
	proxyApp proxy.AppConns

	genesis *lltypes.GenesisDoc

	conf config.NodeConfig
	P2P  *p2p.Client

	// TODO(tzdybal): consider extracting "mempool reactor"
	Mempool      mempool.Mempool
	mempoolIDs   *mempoolIDs
	incomingTxCh chan *p2p.Tx

	BlockStore store.Store

	dalc da.DataAvailabilityLayerClient

	// keep context here only because of API compatibility
	// - it's used in `OnStart` (defined in service.Service interface)
	ctx context.Context
}

func NewNode(ctx context.Context, conf config.NodeConfig, nodeKey crypto.PrivKey, clientCreator proxy.ClientCreator, genesis *lltypes.GenesisDoc, logger log.Logger) (*Node, error) {
	proxyApp := proxy.NewAppConns(clientCreator)
	proxyApp.SetLogger(logger.With("module", "proxy"))
	if err := proxyApp.Start(); err != nil {
		return nil, fmt.Errorf("error starting proxy app connections: %w", err)
	}

	eventBus := lltypes.NewEventBus()
	eventBus.SetLogger(logger.With("module", "events"))
	if err := eventBus.Start(); err != nil {
		return nil, err
	}

	client, err := p2p.NewClient(conf.P2P, nodeKey, genesis.ChainID, logger.With("module", "p2p"))
	if err != nil {
		return nil, err
	}

	dalc := registry.GetClient(conf.DALayer)
	if dalc == nil {
		return nil, fmt.Errorf("couldn't get data availability client named '%s'", conf.DALayer)
	}
	err = dalc.Init(conf.DAConfig, logger)
	if err != nil {
		return nil, fmt.Errorf("data availability layer client initialization error: %w", err)
	}

	mp := mempool.NewCListMempool(llcfg.DefaultMempoolConfig(), proxyApp.Mempool(), 0)

	node := &Node{
		proxyApp:     proxyApp,
		eventBus:     eventBus,
		genesis:      genesis,
		conf:         conf,
		P2P:          client,
		dalc:         dalc,
		Mempool:      mp,
		mempoolIDs:   newMempoolIDs(),
		incomingTxCh: make(chan *p2p.Tx),
		BlockStore:   store.NewBlockStore(),
		ctx:          ctx,
	}
	node.BaseService = *service.NewBaseService(logger, "Node", node)

	return node, nil
}

func (n *Node) mempoolReadLoop(ctx context.Context) {
	for {
		select {
		case tx := <-n.incomingTxCh:
			n.Logger.Debug("tx received", "from", tx.From, "bytes", len(tx.Data))
			err := n.Mempool.CheckTx(tx.Data, func(resp *abci.Response) {}, mempool.TxInfo{
				SenderID:    n.mempoolIDs.GetForPeer(tx.From),
				SenderP2PID: corep2p.ID(tx.From),
				Context:     ctx,
			})
			if err != nil {
				n.Logger.Error("failed to execute CheckTx", "error", err)
			}
		case <-ctx.Done():
			return
		}
	}
}

func (n *Node) mempoolPublishLoop(ctx context.Context) {
	rawMempool := n.Mempool.(*mempool.CListMempool)
	var next *clist.CElement

	for {
		// wait for transactions
		if next == nil {
			select {
			case <-rawMempool.TxsWaitChan():
				if next = rawMempool.TxsFront(); next != nil {
					continue
				}
			case <-ctx.Done():
				return
			}
		}

		// send transactions
		for {
			memTx := next.Value.(*mempool.MempoolTx)
			tx := memTx.Tx

			err := n.P2P.GossipTx(ctx, tx)
			if err != nil {
				n.Logger.Error("failed to gossip transaction", "error", err)
				continue
			}

			nx := next.Next()
			if nx == nil {
				break
			}
			next = nx
		}

		select {
		case <-next.NextWaitChan():
			next = next.Next()
		case <-ctx.Done():
			return
		}
	}
}

func (n *Node) aggregationLoop(ctx context.Context) {
	tick := time.NewTicker(n.conf.BlockTime)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			err := n.publishBlock(ctx)
			if err != nil {
				n.Logger.Error("error while publishing block", "error", err)
			}
		}
	}
}

func (n *Node) publishBlock(ctx context.Context) error {
	n.Logger.Info("Creating and publishing block")

	var maxBlockSize = int64(32 * 1024) // TODO(tzdybal): is this consensus param or config
	// TODO(tzdybal): mempool should use types.Tx, not lltypes.Tx - merge the types
	txs := n.Mempool.ReapMaxBytesMaxGas(maxBlockSize, -1)
	if len(txs) == 0 {
		return nil
	}

	block, err := n.makeBlock(n.BlockStore.Height()+1, types.Txs(txs))
	if err != nil {
		return err
	}

	err = n.BlockStore.SaveBlock(block)
	if err != nil {
		return err
	}
	return n.broadcastBlock(ctx, block)
}

func (n *Node) makeBlock(height uint64, txs types.Txs) (*types.Block, error) {
	// TODO(tzdybal): fill all fields
	lastBlock, err := n.BlockStore.LoadBlock(height - 1)
	if err != nil {
		return nil, err
	}
	lastHash, err := types.Hash(&lastBlock.Header)
	if err != nil {
		return nil, err
	}

	block := &types.Block{
		Header: types.Header{
			Version: types.Version{
				Block: 0,
				App:   0,
			},
			NamespaceID:     [8]byte{},
			Height:          height,
			Time:            uint64(time.Now().UnixNano()), // TODO(tzdybal): how to get TAI64?
			LastHeaderHash:  lastHash,
			LastCommitHash:  [32]byte{},
			DataHash:        [32]byte{},
			ConsensusHash:   [32]byte{},
			AppHash:         [32]byte{},
			LastResultsHash: [32]byte{},
			ProposerAddress: nil,
		},
		Data: types.Data{
			Txs:                    txs,
			IntermediateStateRoots: types.IntermediateStateRoots{RawRootsList: nil},
			Evidence:               types.EvidenceData{Evidence: nil},
		},
		// LastCommit: nil, // TODO(tzdybal)
	}

	block.Header.DataHash, err = types.Hash(&block.Data)
	if err != nil {
		return nil, err
	}

	return block, nil
}

func (n *Node) broadcastBlock(ctx context.Context, block *types.Block) error {
	return nil
}

func (n *Node) OnStart() error {
	n.Logger.Info("starting P2P client")
	err := n.P2P.Start(n.ctx)
	if err != nil {
		return fmt.Errorf("error while starting P2P client: %w", err)
	}
	err = n.dalc.Start()
	if err != nil {
		return fmt.Errorf("error while starting data availability layer client: %w", err)
	}
	go n.mempoolReadLoop(n.ctx)
	go n.mempoolPublishLoop(n.ctx)
	if n.conf.Aggregator {
		go n.aggregationLoop(n.ctx)
	}
	n.P2P.SetTxHandler(func(tx *p2p.Tx) {
		n.incomingTxCh <- tx
	})

	return nil
}

func (n *Node) OnStop() {
	err := n.dalc.Stop()
	err = multierr.Append(err, n.P2P.Close())
	n.Logger.Error("errors while stopping node: %w", err)
}

func (n *Node) OnReset() error {
	panic("OnReset - not implemented!")
}

func (n *Node) SetLogger(logger log.Logger) {
	n.Logger = logger
}

func (n *Node) GetLogger() log.Logger {
	return n.Logger
}

func (n *Node) EventBus() *lltypes.EventBus {
	return n.eventBus
}

func (n *Node) ProxyApp() proxy.AppConns {
	return n.proxyApp
}
