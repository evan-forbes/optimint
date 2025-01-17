package mock

import (
	"github.com/lazyledger/optimint/da"
	"github.com/lazyledger/optimint/log"
	"github.com/lazyledger/optimint/types"
)

type MockDataAvailabilityLayerClient struct {
	logger log.Logger

	Blocks []*types.Block
}

// Init is called once to allow DA client to read configuration and initialize resources.
func (m *MockDataAvailabilityLayerClient) Init(config []byte, logger log.Logger) error {
	m.logger = logger
	return nil
}

func (m *MockDataAvailabilityLayerClient) Start() error {
	m.logger.Debug("Mock Data Availability Layer Client starting")
	return nil
}

func (m *MockDataAvailabilityLayerClient) Stop() error {
	m.logger.Debug("Mock Data Availability Layer Client stopped")
	return nil
}

// SubmitBlock submits the passed in block to the DA layer.
// This should create a transaction which (potentially)
// triggers a state transition in the DA layer.
func (m *MockDataAvailabilityLayerClient) SubmitBlock(block *types.Block) da.ResultSubmitBlock {
	m.Blocks = append(m.Blocks, block)

	return da.ResultSubmitBlock{
		Code:    da.StatusSuccess,
		Message: "OK",
	}
}
