// Copyright (C) 2019-2023, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package merkledb

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/stretchr/testify/require"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/logging"
)

func Test_Intercepter_empty_db(t *testing.T) {
	require := require.New(t)

	db, err := getBasicDB()
	require.NoError(err)

	testIntercepter(
		require,
		db,
		[][]byte{
			{2},
		},
		[]KeyChange{
			{
				Key:   []byte{0},
				Value: Some([]byte{0, 1, 2}),
			},
			{
				Key:   []byte{1},
				Value: Some([]byte{1, 2}),
			},
		},
	)
}

func Test_Intercepter_non_empty_initial_db(t *testing.T) {
	require := require.New(t)

	db, err := getBasicDB()
	require.NoError(err)

	require.NoError(db.Put([]byte{0}, []byte{2}))

	testIntercepter(
		require,
		db,
		[][]byte{
			{2},
		},
		[]KeyChange{
			{
				Key:   []byte{0},
				Value: Some([]byte{0, 1, 2}),
			},
			{
				Key:   []byte{1},
				Value: Some([]byte{1, 2}),
			},
		},
	)
}

func Test_Intercepter_non_empty_initial_db_with_delete(t *testing.T) {
	require := require.New(t)

	db, err := getBasicDB()
	require.NoError(err)

	require.NoError(db.Put([]byte{0}, []byte{2}))

	testIntercepter(
		require,
		db,
		[][]byte{
			{2},
		},
		[]KeyChange{
			{
				Key:   []byte{0},
				Value: Nothing[[]byte](),
			},
			{
				Key:   []byte{1},
				Value: Some([]byte{1, 2}),
			},
		},
	)
}

func Test_Intercepter_root_special(t *testing.T) {
	require := require.New(t)

	db, err := getBasicDB()
	require.NoError(err)

	require.NoError(db.Put([]byte{0}, []byte{2}))

	startRootID, startRoot, valueProofs, pathProofs, _ := build(require, db, nil, []KeyChange{
		{
			Key:   []byte{0},
			Value: Nothing[[]byte](),
		},
		{
			Key:   []byte{1},
			Value: Some([]byte{1, 2}),
		},
	})
	_, _, _, _, endRootID := build(require, db, nil, []KeyChange{
		{
			Key:   []byte{0},
			Value: Nothing[[]byte](),
		},
		{
			Key:   []byte{1},
			Value: Some([]byte{1, 2}),
		},
		{
			Key:   []byte{},
			Value: Some([]byte{2}),
		},
	})
	verify(require, startRootID, startRoot, valueProofs, pathProofs, []KeyChange{
		{
			Key:   []byte{0},
			Value: Nothing[[]byte](),
		},
		{
			Key:   []byte{1},
			Value: Some([]byte{1, 2}),
		},
		{
			Key:   []byte{},
			Value: Some([]byte{2}),
		},
	}, endRootID)
}

func Test_Intercepter_insert_stale(t *testing.T) {
	require := require.New(t)

	db, err := getBasicDB()
	require.NoError(err)

	ctx := context.Background()
	startRootID, err := db.GetMerkleRoot(ctx)
	require.NoError(err)

	startRootBytes, err := db.root.marshal()
	require.NoError(err)

	view0, err := db.NewView()
	require.NoError(err)

	view0.SetIntercepter()

	require.NoError(view0.Insert(ctx, []byte{1}, []byte{2}))

	expectedNewRoot0, err := view0.GetMerkleRoot(ctx)
	require.NoError(err)

	valueProofs0, pathProofs0 := view0.GetInterceptedProofs()
	for _, proof := range valueProofs0 {
		require.NoError(proof.Verify(ctx, startRootID))
	}
	for _, proof := range pathProofs0 {
		require.NoError(proof.Verify(ctx, startRootID))
	}

	values0 := make(map[Path]Maybe[[]byte])
	for _, proof := range valueProofs0 {
		values0[NewPath(proof.Key)] = proof.Value
	}

	nodes0 := make(map[Path]Maybe[*Node])
	for _, proof := range pathProofs0 {
		key := proof.KeyPath.Deserialize()
		nodes0[key] = proof.ToNode()
	}

	baseView, err := NewBaseStatelessView(
		startRootBytes,
		logging.NoLog{},
		prometheus.NewRegistry(),
		newNoopTracer(),
		1,
		10,
	)
	require.NoError(err)

	baseView.SetTemporaryState(values0, nodes0)

	require.NoError(baseView.Insert(ctx, []byte{1}, []byte{2}))

	newRoot, err := baseView.GetMerkleRoot(ctx)
	require.NoError(err)
	require.Equal(expectedNewRoot0, newRoot)

	view1, err := db.NewView()
	require.NoError(err)

	view1.SetIntercepter()

	require.NoError(view1.Insert(ctx, []byte{2}, []byte{3}))

	_, err = view1.GetMerkleRoot(ctx)
	require.NoError(err)

	valueProofs1, pathProofs1 := view1.GetInterceptedProofs()
	for _, proof := range valueProofs1 {
		require.NoError(proof.Verify(ctx, startRootID))
	}
	for _, proof := range pathProofs1 {
		require.NoError(proof.Verify(ctx, startRootID))
	}

	values1 := make(map[Path]Maybe[[]byte])
	for _, proof := range valueProofs1 {
		values1[NewPath(proof.Key)] = proof.Value
	}

	nodes1 := make(map[Path]Maybe[*Node])
	for _, proof := range pathProofs1 {
		key := proof.KeyPath.Deserialize()
		nodes1[key] = proof.ToNode()
	}

	childView := baseView.NewStatelessView(1)

	baseView.AddPermanentState(values0, nodes0)
	baseView.SetTemporaryState(values1, nodes1)

	require.NoError(childView.Insert(ctx, []byte{2}, []byte{3}))

	_, err = childView.GetMerkleRoot(ctx)
	require.NoError(err)
}

func testIntercepter(
	require *require.Assertions,
	db *merkleDB,
	reads [][]byte,
	changes []KeyChange,
) {
	startRootID, startRoot, valueProofs, pathProofs, endRootID := build(require, db, reads, changes)
	verify(require, startRootID, startRoot, valueProofs, pathProofs, changes, endRootID)
}

func build(
	require *require.Assertions,
	db *merkleDB,
	reads [][]byte,
	changes []KeyChange,
) (
	ids.ID,
	[]byte,
	[]*Proof,
	[]*PathProof,
	ids.ID,
) {
	ctx := context.Background()
	startRootID, err := db.GetMerkleRoot(ctx)
	require.NoError(err)

	startRootBytes, err := db.root.marshal()
	require.NoError(err)

	view, err := db.NewView()
	require.NoError(err)

	view.SetIntercepter()
	for _, key := range reads {
		_, _ = view.GetValue(ctx, key)
	}
	for _, change := range changes {
		if change.Value.IsNothing() {
			require.NoError(view.Remove(ctx, change.Key))
		} else {
			require.NoError(view.Insert(ctx, change.Key, change.Value.Value()))
		}
	}

	expectedNewRoot, err := view.GetMerkleRoot(ctx)
	require.NoError(err)

	valueProofs, pathProofs := view.GetInterceptedProofs()

	return startRootID, startRootBytes, valueProofs, pathProofs, expectedNewRoot
}

func verify(
	require *require.Assertions,
	startRootID ids.ID,
	startRootBytes []byte,
	valueProofs []*Proof,
	pathProofs []*PathProof,
	changes []KeyChange,
	expectedRootID ids.ID,
) {
	ctx := context.Background()
	for _, proof := range valueProofs {
		require.NoError(proof.Verify(ctx, startRootID))
	}
	for _, proof := range pathProofs {
		require.NoError(proof.Verify(ctx, startRootID))
	}

	values := make(map[Path]Maybe[[]byte])
	for _, proof := range valueProofs {
		values[NewPath(proof.Key)] = proof.Value
	}

	nodes := make(map[Path]Maybe[*Node])
	for _, proof := range pathProofs {
		key := proof.KeyPath.Deserialize()
		nodes[key] = proof.ToNode()
	}

	view, err := NewBaseStatelessView(
		startRootBytes,
		logging.NoLog{},
		prometheus.NewRegistry(),
		newNoopTracer(),
		1,
		0,
	)
	require.NoError(err)

	view.SetTemporaryState(values, nodes)

	for _, change := range changes {
		if change.Value.IsNothing() {
			require.NoError(view.Remove(ctx, change.Key))
		} else {
			require.NoError(view.Insert(ctx, change.Key, change.Value.Value()))
		}
	}

	newRoot, err := view.GetMerkleRoot(ctx)
	require.NoError(err)
	require.Equal(expectedRootID, newRoot)
}
