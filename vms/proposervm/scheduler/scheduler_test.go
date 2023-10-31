// Copyright (C) 2019-2023, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package scheduler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/utils"
	"github.com/ava-labs/avalanchego/utils/logging"
)

func TestDelayFromNew(t *testing.T) {
	var (
		toEngine           = make(chan common.Message, 10)
		startTime          = time.Now().Add(50 * time.Millisecond)
		dummyStateSyncDone = utils.Atomic[bool]{}
	)

	s, fromVM := New(logging.NoLog{}, toEngine, &dummyStateSyncDone)
	defer s.Close()
	go s.Dispatch(startTime)

	fromVM <- common.PendingTxs

	<-toEngine
	require.LessOrEqual(t, time.Until(startTime), time.Duration(0))
}

func TestDelayFromSetTime(t *testing.T) {
	var (
		toEngine           = make(chan common.Message, 10)
		now                = time.Now()
		startTime          = now.Add(50 * time.Millisecond)
		dummyStateSyncDone = utils.Atomic[bool]{}
	)

	s, fromVM := New(logging.NoLog{}, toEngine, &dummyStateSyncDone)
	defer s.Close()
	go s.Dispatch(now)

	s.SetBuildBlockTime(startTime)

	fromVM <- common.PendingTxs

	<-toEngine
	require.LessOrEqual(t, time.Until(startTime), time.Duration(0))
}

func TestReceipt(*testing.T) {
	var (
		toEngine           = make(chan common.Message, 10)
		now                = time.Now()
		startTime          = now.Add(50 * time.Millisecond)
		dummyStateSyncDone = utils.Atomic[bool]{}
	)

	s, fromVM := New(logging.NoLog{}, toEngine, &dummyStateSyncDone)
	defer s.Close()
	go s.Dispatch(now)

	fromVM <- common.PendingTxs

	s.SetBuildBlockTime(startTime)

	<-toEngine
}
