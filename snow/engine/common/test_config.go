// Copyright (C) 2019-2023, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package common

import (
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/snow/engine/common/tracker"
	"github.com/ava-labs/avalanchego/snow/validators"
	"github.com/ava-labs/avalanchego/utils/constants"
)

// DefaultConfigTest returns a test configuration
func DefaultConfigTest() Config {
	beacons := validators.NewManager()

	connectedPeers := tracker.NewPeers()
	startupTracker := tracker.NewStartup(connectedPeers, 0)
	beacons.RegisterCallbackListener(constants.PrimaryNetworkID, startupTracker)

	return Config{
		Ctx:            snow.DefaultConsensusContextTest(),
		Beacons:        beacons,
		StartupTracker: startupTracker,
		Sender:         &SenderTest{},
	}
}
