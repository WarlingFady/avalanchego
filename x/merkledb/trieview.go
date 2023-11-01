// Copyright (C) 2019-2023, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package merkledb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"golang.org/x/sync/errgroup"
	"maps"
	"sync"

	"go.opentelemetry.io/otel/attribute"

	oteltrace "go.opentelemetry.io/otel/trace"

	"golang.org/x/exp/slices"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils"
	"github.com/ava-labs/avalanchego/utils/maybe"
)

const (
	initKeyValuesSize        = 256
	defaultPreallocationSize = 100
)

var (
	_ TrieView = (*trieView)(nil)

	ErrCommitted                  = errors.New("view has been committed")
	ErrInvalid                    = errors.New("the trie this view was based on has changed, rendering this view invalid")
	ErrPartialByteLengthWithValue = errors.New(
		"the underlying db only supports whole number of byte keys, so cannot record changes with partial byte lengths",
	)
	ErrGetPathToFailure       = errors.New("GetPathTo failed to return the closest node")
	ErrStartAfterEnd          = errors.New("start key > end key")
	ErrNoValidRoot            = errors.New("a valid root was not provided to the trieView constructor")
	ErrParentNotDatabase      = errors.New("parent trie is not database")
	ErrNodesAlreadyCalculated = errors.New("cannot modify the trie after the node changes have been calculated")
)

type trieView struct {
	// If true, this view has been committed.
	// [commitLock] must be held while accessing this field.
	committed  bool
	commitLock sync.RWMutex

	// tracking bool to enforce that no changes are made to the trie after the nodes have been calculated
	nodesAlreadyCalculated utils.Atomic[bool]

	// calculateNodesOnce is a once to ensure that node calculation only occurs a single time
	calculateNodesOnce sync.Once

	// Controls the trie's validity related fields.
	// Must be held while reading/writing [childViews], [invalidated], and [parentTrie].
	// Only use to lock current trieView or descendants of the current trieView
	// DO NOT grab the [validityTrackingLock] of any ancestor trie while this is held.
	validityTrackingLock sync.RWMutex

	// If true, this view has been invalidated and can't be used.
	//
	// Invariant: This view is marked as invalid before any of its ancestors change.
	// Since we ensure that all subviews are marked invalid before making an invalidating change
	// then if we are still valid at the end of the function, then no corrupting changes could have
	// occurred during execution.
	// Namely, if we have a method with:
	//
	// *Code Accessing Ancestor State*
	//
	// if t.isInvalid() {
	//     return ErrInvalid
	//  }
	// return [result]
	//
	// If the invalidated check passes, then we're guaranteed that no ancestor changes occurred
	// during the code that accessed ancestor state and the result of that work is still valid
	//
	// [validityTrackingLock] must be held when reading/writing this field.
	invalidated bool

	// the uncommitted parent trie of this view
	// [validityTrackingLock] must be held when reading/writing this field.
	parentTrie TrieView

	// The valid children of this trie.
	// [validityTrackingLock] must be held when reading/writing this field.
	childViews []*trieView

	// Changes made to this view.
	// May include nodes that haven't been updated
	// but will when their ID is recalculated.
	changes         *changeSummary
	childrenChanges map[Key]nodeChildren

	db *merkleDB

	tokenSize int
}

// NewView returns a new view on top of this Trie where the passed changes
// have been applied.
// Adds the new view to [t.childViews].
// Assumes [t.commitLock] isn't held.
func (t *trieView) NewView(
	ctx context.Context,
	changes ViewChanges,
) (TrieView, error) {
	if t.isInvalid() {
		return nil, ErrInvalid
	}
	t.commitLock.RLock()
	defer t.commitLock.RUnlock()

	if t.committed {
		return t.getParentTrie().NewView(ctx, changes)
	}

	if err := t.calculateNodeIDs(ctx); err != nil {
		return nil, err
	}

	newView, err := newTrieView(t.db, t, changes)
	if err != nil {
		return nil, err
	}

	t.validityTrackingLock.Lock()
	defer t.validityTrackingLock.Unlock()

	if t.invalidated {
		return nil, ErrInvalid
	}
	t.childViews = append(t.childViews, newView)

	return newView, nil
}

// Creates a new view with the given [parentTrie].
// Assumes [parentTrie] isn't locked.
func newTrieView(
	db *merkleDB,
	parentTrie TrieView,
	changes ViewChanges,
) (*trieView, error) {
	rootChildren, err := parentTrie.getChildren(emptyKey)
	if err != nil {
		if err == database.ErrNotFound {
			return nil, ErrNoValidRoot
		}
		return nil, err
	}

	newView := &trieView{
		db:              db,
		parentTrie:      parentTrie,
		changes:         newChangeSummary(len(changes.BatchOps) + len(changes.MapOps)),
		childrenChanges: make(map[Key]nodeChildren, len(changes.BatchOps)+len(changes.MapOps)),
		tokenSize:       db.tokenSize,
	}
	newView.childrenChanges[emptyKey] = maps.Clone(rootChildren)

	for _, op := range changes.BatchOps {
		key := op.Key
		if !changes.ConsumeBytes {
			key = slices.Clone(op.Key)
		}

		newVal := maybe.Nothing[[]byte]()
		if !op.Delete {
			newVal = maybe.Some(op.Value)
			if !changes.ConsumeBytes {
				newVal = maybe.Some(slices.Clone(op.Value))
			}
		}
		if err := newView.recordValueChange(toKey(key), newVal); err != nil {
			return nil, err
		}
	}
	for key, val := range changes.MapOps {
		if !changes.ConsumeBytes {
			val = maybe.Bind(val, slices.Clone[[]byte])
		}
		if err := newView.recordValueChange(toKey(stringToByteSlice(key)), val); err != nil {
			return nil, err
		}
	}
	return newView, nil
}

// Creates a view of the db at a historical root using the provided changes
func newHistoricalTrieView(
	db *merkleDB,
	changes *changeSummary,
) (*trieView, error) {

	changeOps := make([]database.BatchOp, 0, len(changes.values))
	for key, valueChange := range changes.values {
		changeOps = append(changeOps, database.BatchOp{Key: key.Bytes(), Value: valueChange.after.Value(), Delete: valueChange.after.IsNothing()})
	}
	return newTrieView(db, db, ViewChanges{BatchOps: changeOps})
}

// Recalculates the node IDs for all changed nodes in the trie.
// Cancelling [ctx] doesn't cancel calculation. It's used only for tracing.
func (t *trieView) calculateNodeIDs(ctx context.Context) error {
	var err error
	t.calculateNodesOnce.Do(func() {
		if t.isInvalid() {
			err = ErrInvalid
			return
		}
		defer t.nodesAlreadyCalculated.Set(true)

		// We wait to create the span until after checking that we need to actually
		// calculateNodeIDs to make traces more useful (otherwise there may be a span
		// per key modified even though IDs are not re-calculated).
		_, span := t.db.infoTracer.Start(ctx, "MerkleDB.trieview.calculateNodeIDs")
		defer span.End()

		// add all the changed key/values to the nodes of the trie
		for key, change := range t.changes.values {
			if change.after.IsNothing() {
				// Note we're setting [err] defined outside this function.
				if err = t.remove(key); err != nil {
					return
				}
				// Note we're setting [err] defined outside this function.
			} else if _, err = t.insert(key); err != nil {
				return
			}
		}

		_ = t.db.calculateNodeIDsSema.Acquire(context.Background(), 1)
		var val maybe.Maybe[[]byte]
		val, err = t.getValue(emptyKey)
		if err != nil {
			return
		}
		t.changes.rootID, err = t.calculateNodeIDsHelper(emptyKey, val)
		if err != nil {
			return
		}
		t.db.calculateNodeIDsSema.Release(1)

		// ensure no ancestor changes occurred during execution
		if t.isInvalid() {
			err = ErrInvalid
			return
		}
	})
	return err
}

// Calculates the ID of all descendants of [n] which need to be recalculated,
// and then calculates the ID of [n] itself.
func (t *trieView) calculateNodeIDsHelper(key Key, value maybe.Maybe[[]byte]) (ids.ID, error) {
	// We use [wg] to wait until all descendants of [n] have been updated.
	var eg errgroup.Group

	children, err := t.getChildren(key)
	if err != nil {
		return ids.Empty, err
	}
	for childIndex, child := range children {
		childPath := key.Extend(ToToken(childIndex, t.tokenSize), child.compressedKey)
		_, childrenChanged := t.childrenChanges[childPath]
		valueChange, valueChanged := t.changes.values[childPath]

		if !childrenChanged && !valueChanged || child.id != ids.Empty {
			continue
		}

		currentIndex := childIndex
		calculateChildID := func() error {
			var changedValue maybe.Maybe[[]byte]
			if valueChanged {
				changedValue = valueChange.after
			}
			children[currentIndex].id, err = t.calculateNodeIDsHelper(childPath, changedValue)

			if err != nil {
				return err
			}
			return nil
		}

		// Try updating the child and its descendants in a goroutine.
		if ok := t.db.calculateNodeIDsSema.TryAcquire(1); ok {
			eg.Go(
				func() error {
					defer t.db.calculateNodeIDsSema.Release(1)
					return calculateChildID()
				})
		} else {
			// We're at the goroutine limit; do the work in this goroutine.
			if err := calculateChildID(); err != nil {
				return ids.Empty, err
			}
		}
	}

	// Wait until all descendants of [n] have been updated.
	if err := eg.Wait(); err != nil {
		return ids.Empty, err
	}

	// The IDs [n]'s descendants are up to date so we can calculate [n]'s ID.
	return calculateID(t.db.metrics, key, children, value), nil
}

// GetProof returns a proof that [bytesPath] is in or not in trie [t].
func (t *trieView) GetProof(ctx context.Context, key []byte) (*Proof, error) {
	_, span := t.db.infoTracer.Start(ctx, "MerkleDB.trieview.GetProof")
	defer span.End()

	if err := t.calculateNodeIDs(ctx); err != nil {
		return nil, err
	}

	return t.getProof(ctx, key)
}

// Returns a proof that [bytesPath] is in or not in trie [t].
func (t *trieView) getProof(ctx context.Context, key []byte) (*Proof, error) {
	_, span := t.db.infoTracer.Start(ctx, "MerkleDB.trieview.getProof")
	defer span.End()

	proof := &Proof{
		Key: ToKey(key),
	}

	var (
		closestKey      Key
		closestChildren nodeChildren
		closestValue    maybe.Maybe[[]byte]
		err             error
	)

	if err := t.visitPathToKey(proof.Key, func(key Key, children nodeChildren) error {
		closestKey = key
		closestChildren = children
		closestValue, err = t.getValue(key)
		if err != nil {
			return err
		}
		proof.Path = append(proof.Path, asProofNode(key, children, closestValue))
		return nil
	}); err != nil {
		return nil, err
	}

	if closestKey == proof.Key {
		// There is a node with the given [key].
		proof.Value = maybe.Bind(closestValue, slices.Clone[[]byte])
		return proof, nil
	}

	// There is no node with the given [key].
	// If there is a child at the index where the node would be
	// if it existed, include that child in the proof.
	nextIndex := proof.Key.Token(closestKey.length, t.tokenSize)
	child, ok := closestChildren[nextIndex]
	if !ok {
		return proof, nil
	}

	childKey := closestKey.Extend(ToToken(nextIndex, t.tokenSize), child.compressedKey)
	childChildren, err := t.getChildren(childKey)
	if err != nil {
		return nil, err
	}
	childValue, err := t.getValue(childKey)
	if err != nil {
		return nil, err
	}
	proof.Path = append(proof.Path, asProofNode(childKey, childChildren, childValue))
	if t.isInvalid() {
		return nil, ErrInvalid
	}
	return proof, nil
}

// GetRangeProof returns a range proof for (at least part of) the key range [start, end].
// The returned proof's [KeyValues] has at most [maxLength] values.
// [maxLength] must be > 0.
func (t *trieView) GetRangeProof(
	ctx context.Context,
	start maybe.Maybe[[]byte],
	end maybe.Maybe[[]byte],
	maxLength int,
) (*RangeProof, error) {
	ctx, span := t.db.infoTracer.Start(ctx, "MerkleDB.trieview.GetRangeProof")
	defer span.End()

	if start.HasValue() && end.HasValue() && bytes.Compare(start.Value(), end.Value()) == 1 {
		return nil, ErrStartAfterEnd
	}

	if maxLength <= 0 {
		return nil, fmt.Errorf("%w but was %d", ErrInvalidMaxLength, maxLength)
	}

	if err := t.calculateNodeIDs(ctx); err != nil {
		return nil, err
	}

	var result RangeProof

	result.KeyValues = make([]KeyValue, 0, initKeyValuesSize)
	it := t.NewIteratorWithStart(start.Value())
	for it.Next() && len(result.KeyValues) < maxLength && (end.IsNothing() || bytes.Compare(it.Key(), end.Value()) <= 0) {
		// clone the value to prevent editing of the values stored within the trie
		result.KeyValues = append(result.KeyValues, KeyValue{
			Key:   it.Key(),
			Value: slices.Clone(it.Value()),
		})
	}
	it.Release()
	if err := it.Error(); err != nil {
		return nil, err
	}

	// This proof may not contain all key-value pairs in [start, end] due to size limitations.
	// The end proof we provide should be for the last key-value pair in the proof, not for
	// the last key-value pair requested, which may not be in this proof.
	var (
		endProof *Proof
		err      error
	)
	if len(result.KeyValues) > 0 {
		greatestKey := result.KeyValues[len(result.KeyValues)-1].Key
		endProof, err = t.getProof(ctx, greatestKey)
		if err != nil {
			return nil, err
		}
	} else if end.HasValue() {
		endProof, err = t.getProof(ctx, end.Value())
		if err != nil {
			return nil, err
		}
	}
	if endProof != nil {
		result.EndProof = endProof.Path
	}

	if start.HasValue() {
		startProof, err := t.getProof(ctx, start.Value())
		if err != nil {
			return nil, err
		}
		result.StartProof = startProof.Path

		// strip out any common nodes to reduce proof size
		i := 0
		for ; i < len(result.StartProof) &&
			i < len(result.EndProof) &&
			result.StartProof[i].Key == result.EndProof[i].Key; i++ {
		}
		result.StartProof = result.StartProof[i:]
	}

	if len(result.StartProof) == 0 && len(result.EndProof) == 0 && len(result.KeyValues) == 0 {
		// If the range is empty, return the root proof.
		rootProof, err := t.getProof(ctx, rootKey)
		if err != nil {
			return nil, err
		}
		result.EndProof = rootProof.Path
	}

	if t.isInvalid() {
		return nil, ErrInvalid
	}
	return &result, nil
}

// CommitToDB commits changes from this trie to the underlying DB.
func (t *trieView) CommitToDB(ctx context.Context) error {
	ctx, span := t.db.infoTracer.Start(ctx, "MerkleDB.trieview.CommitToDB")
	defer span.End()

	t.db.commitLock.Lock()
	defer t.db.commitLock.Unlock()

	return t.commitToDB(ctx)
}

// Commits the changes from [trieToCommit] to this view,
// this view to its parent, and so on until committing to the db.
// Assumes [t.db.commitLock] is held.
func (t *trieView) commitToDB(ctx context.Context) error {
	t.commitLock.Lock()
	defer t.commitLock.Unlock()

	ctx, span := t.db.infoTracer.Start(ctx, "MerkleDB.trieview.commitToDB", oteltrace.WithAttributes(
		attribute.Int("changeCount", len(t.changes.values)),
	))
	defer span.End()

	// Call this here instead of in [t.db.commitChanges]
	// because doing so there would be a deadlock.
	if err := t.calculateNodeIDs(ctx); err != nil {
		return err
	}

	if err := t.db.commitChanges(ctx, t); err != nil {
		return err
	}

	t.committed = true

	return nil
}

// Assumes [t.validityTrackingLock] isn't held.
func (t *trieView) isInvalid() bool {
	t.validityTrackingLock.RLock()
	defer t.validityTrackingLock.RUnlock()

	return t.invalidated
}

// Invalidates this view and all descendants.
// Assumes [t.validityTrackingLock] isn't held.
func (t *trieView) invalidate() {
	t.validityTrackingLock.Lock()
	defer t.validityTrackingLock.Unlock()

	t.invalidated = true

	for _, childView := range t.childViews {
		childView.invalidate()
	}

	// after invalidating the children, they no longer need to be tracked
	t.childViews = make([]*trieView, 0, defaultPreallocationSize)
}

func (t *trieView) updateParent(newParent TrieView) {
	t.validityTrackingLock.Lock()
	defer t.validityTrackingLock.Unlock()

	t.parentTrie = newParent
}

// GetMerkleRoot returns the ID of the root of this trie.
func (t *trieView) GetMerkleRoot(ctx context.Context) (ids.ID, error) {
	if err := t.calculateNodeIDs(ctx); err != nil {
		return ids.Empty, err
	}
	return t.changes.rootID, nil
}

func (t *trieView) GetValues(ctx context.Context, keys [][]byte) ([][]byte, []error) {
	_, span := t.db.debugTracer.Start(ctx, "MerkleDB.trieview.GetValues", oteltrace.WithAttributes(
		attribute.Int("keyCount", len(keys)),
	))
	defer span.End()

	results := make([][]byte, len(keys))
	valueErrors := make([]error, len(keys))

	for i, key := range keys {
		val, err := t.getValueCopy(ToKey(key))
		switch {
		case err != nil:
			valueErrors[i] = err
		case val.IsNothing():
			valueErrors[i] = database.ErrNotFound
		default:
			results[i] = val.Value()
		}
	}
	return results, valueErrors
}

// GetValue returns the value for the given [key].
// Returns database.ErrNotFound if it doesn't exist.
func (t *trieView) GetValue(ctx context.Context, key []byte) ([]byte, error) {
	_, span := t.db.debugTracer.Start(ctx, "MerkleDB.trieview.GetValue")
	defer span.End()
	val, err := t.getValueCopy(ToKey(key))
	if err != nil {
		return nil, err
	}
	if val.IsNothing() {
		return nil, database.ErrNotFound
	}
	return val.Value(), nil
}

// getValueCopy returns a copy of the value for the given [key].
// Returns database.ErrNotFound if it doesn't exist.
func (t *trieView) getValueCopy(key Key) (maybe.Maybe[[]byte], error) {
	val, err := t.getValue(key)
	if err != nil {
		return maybe.Nothing[[]byte](), err
	}
	return maybe.Bind(val, slices.Clone[[]byte]), nil
}

func (t *trieView) getValue(key Key) (maybe.Maybe[[]byte], error) {
	if t.isInvalid() {
		return maybe.Nothing[[]byte](), ErrInvalid
	}

	if change, ok := t.changes.values[key]; ok {
		t.db.metrics.ViewValueCacheHit()
		return change.after, nil
	}
	t.db.metrics.ViewValueCacheMiss()

	// if we don't have local copy of the key, then grab a copy from the parent trie
	value, err := t.getParentTrie().getValue(key)
	if err != nil {
		return maybe.Nothing[[]byte](), err
	}

	// ensure no ancestor changes occurred during execution
	if t.isInvalid() {
		return maybe.Nothing[[]byte](), ErrInvalid
	}

	return value, nil
}

// Must not be called after [calculateNodeIDs] has returned.
func (t *trieView) remove(key Key) error {
	if t.nodesAlreadyCalculated.Get() {
		return ErrNodesAlreadyCalculated
	}

	// confirm a node exists with a value
	keyNode, err := t.getChildren(key)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			// key didn't exist
			return nil
		}
		return err
	}

	// node has already been deleted
	if keyNode == nil {
		return nil
	}

	// if the node exists and contains a value
	// mark all ancestor for change
	// grab parent and grandparent nodes for path compression
	var (
		grandParentKey, parentKey, nodeToDeleteKey Key
		grandParent, parent, nodeToDelete          nodeChildren
	)

	if err := t.visitPathToKey(key, func(key Key, children nodeChildren) error {
		grandParentKey = parentKey
		parentKey = nodeToDeleteKey
		nodeToDeleteKey = key

		grandParent = parent
		parent = nodeToDelete
		nodeToDelete = children
		if parent != nil {
			t.recordPendingChildChange(parentKey, nodeToDeleteKey)
		}
		return nil
	}); err != nil {
		return err
	}

	if len(nodeToDelete) != 0 {
		// merge this node and its child into a single node if possible
		return t.compressNodePath(parent, parentKey, nodeToDelete, nodeToDeleteKey)
	}

	// if the removed node has no children, the node can be removed from the trie
	t.recordNodeDeleted(nodeToDeleteKey)
	if parent != nil {
		t.recordChildRemoved(parentKey, nodeToDeleteKey)

		// merge the parent node and its child into a single node if possible
		return t.compressNodePath(grandParent, grandParentKey, parent, parentKey)
	}
	return nil
}

// Merges together nodes in the inclusive descendants of [node] that
// have no value and a single child into one node with a compressed
// path until a node that doesn't meet those criteria is reached.
// [parent] is [node]'s parent.
// Assumes at least one of the following is true:
// * [node] has a value.
// * [node] has children.
// Must not be called after [calculateNodeIDs] has returned.
func (t *trieView) compressNodePath(parent nodeChildren, parentKey Key, node nodeChildren, nodeKey Key) error {
	if t.nodesAlreadyCalculated.Get() {
		return ErrNodesAlreadyCalculated
	}

	// don't collapse into this node if it's the root, doesn't have 1 child, or has a value
	if parent == nil || len(node) != 1 {
		return nil
	}
	val, err := t.getValue(nodeKey)
	// if there was an error, return the error
	// if the key has a value return nil
	if err != nil || val.HasValue() {
		return err
	}

	t.recordNodeDeleted(nodeKey)

	var (
		childEntry *child
		childKey   Key
	)
	// There is only one child, but we don't know the index.
	// "Cycle" over the key/values to find the only child.
	// Note this iteration once because len(node.children) == 1.
	for index, entry := range node {
		childKey = nodeKey.Extend(ToToken(index, t.tokenSize), entry.compressedKey)
		childEntry = entry
	}

	// [node] is the first node with multiple children.
	// combine it with the [node] passed in.
	t.recordChildChange(parentKey, childKey, childEntry.id)
	return nil
}

// Returns the nodes along the path to [key].
// The first node is the root, and the last node is either the node with the
// given [key], if it's in the trie, or the node with the largest prefix of
// the [key] if it isn't in the trie.
// Always returns at least the root node.
func (t *trieView) visitPathToKey(key Key, visitNode func(key Key, children nodeChildren) error) error {
	var (
		// all node paths start at the root
		currentKey      = emptyKey
		currentChildren = t.childrenChanges[emptyKey]
		err             error
	)
	if err := visitNode(currentKey, currentChildren); err != nil {
		return err
	}
	// while the entire path hasn't been matched
	for currentKey.length < key.length {
		// confirm that a child exists and grab its ID before attempting to load it
		nextChildEntry, hasChild := currentChildren[key.Token(currentKey.length, t.tokenSize)]

		if !hasChild || !key.iteratedHasPrefix(nextChildEntry.compressedKey, currentKey.length+t.tokenSize, t.tokenSize) {
			// there was no child along the path or the child that was there doesn't match the remaining path
			return nil
		}
		// grab the next node along the path
		currentKey = key.Take(currentKey.length + t.tokenSize + nextChildEntry.compressedKey.length)
		currentChildren, err = t.getChildrenInternal(currentKey)
		if err != nil {
			return err
		}
		if err := visitNode(currentKey, currentChildren); err != nil {
			return err
		}
	}
	return nil
}

// insert a key/value pair into the correct node of the trie.
// Must not be called after [calculateNodeIDs] has returned.
func (t *trieView) insert(
	key Key,
) (nodeChildren, error) {
	if t.nodesAlreadyCalculated.Get() {
		return nil, ErrNodesAlreadyCalculated
	}

	var (
		closestChildren nodeChildren
		closestKey      Key
		previousKey     *Key
	)
	if err := t.visitPathToKey(key, func(localKey Key, children nodeChildren) error {
		if previousKey != nil {
			t.recordPendingChildChange(*previousKey, localKey)
		}
		previousKey = &localKey
		closestKey = localKey
		closestChildren = children
		return nil
	}); err != nil {
		return nil, err
	}

	// a node with that exact key already exists
	if closestKey == key {
		return closestChildren, nil
	}

	// A node with the exact key doesn't exist so determine the portion of the
	// key that hasn't been matched yet
	// Note that [key] has prefix [closestNodeKey] but exactMatch was false,
	// so [key] must be longer than [key] and the following index and slice won't OOB.
	existingChildEntry, hasChild := closestChildren[key.Token(closestKey.length, t.tokenSize)]
	if !hasChild {
		// there are no existing nodes along the key [key], so create a new node to insert [value]
		t.recordChildChange(closestKey, key, ids.Empty)
		return t.recordNewNode(key), nil
	}

	// if we have reached this point, then the [key] we are trying to insert and
	// the existing path node have some common prefix.
	// a new branching node will be created that will represent this common prefix and
	// have the existing path node and the value being inserted as children.

	// generate the new branch node
	// find how many tokens are common between the existing child's compressed path and
	// the current key(offset by the closest node's key),
	// then move all the common tokens into the branch node
	commonPrefixLength := getLengthOfCommonPrefix(existingChildEntry.compressedKey, key, closestKey.length+t.tokenSize, t.tokenSize)

	// If the length of the existing child's compressed path is less than or equal to the branch node's key that implies that the existing child's key matched the key to be inserted.
	// Since it matched the key to be inserted, it should have been the last node returned by GetPathTo
	if existingChildEntry.compressedKey.length <= commonPrefixLength {
		return nil, ErrGetPathToFailure
	}

	branchKey := key.Take(closestKey.length + t.tokenSize + commonPrefixLength)
	t.recordChildChange(closestKey, branchKey, ids.Empty)
	if key.length != branchKey.length {
		// the key to be inserted is a child of the branch node
		t.recordChildChange(branchKey, key, ids.Empty)
	}

	// add the existing child onto the branch node
	t.recordChildEntry(branchKey, existingChildEntry.compressedKey.Token(commonPrefixLength, t.tokenSize),
		&child{
			compressedKey: existingChildEntry.compressedKey.Skip(commonPrefixLength + t.tokenSize),
			id:            existingChildEntry.id,
		})

	return t.recordNewNode(key), nil
}

func getLengthOfCommonPrefix(first, second Key, secondOffset int, tokenSize int) int {
	commonIndex := 0
	for first.length > commonIndex && second.length > commonIndex+secondOffset &&
		first.Token(commonIndex, tokenSize) == second.Token(commonIndex+secondOffset, tokenSize) {
		commonIndex += tokenSize
	}
	return commonIndex
}

func (t *trieView) recordNewNode(key Key) nodeChildren {
	result := make(nodeChildren, 2)
	t.childrenChanges[key] = result
	return result
}

func (t *trieView) recordChildRemoved(key Key, childKey Key) {
	if existing, ok := t.childrenChanges[key]; ok {
		delete(existing, childKey.Token(key.length, t.tokenSize))
	}
}

func (t *trieView) recordNodeDeleted(key Key) {
	t.childrenChanges[key] = nil
}

func (t *trieView) recordPendingChildChange(key Key, childKey Key) {
	t.recordChildEntry(key, childKey.Token(key.length, t.tokenSize), &child{
		compressedKey: childKey.Skip(key.length + t.tokenSize),
	})
}

func (t *trieView) recordChildEntry(key Key, childIndex byte, entry *child) {
	var childMap nodeChildren
	if existing, ok := t.childrenChanges[key]; ok && existing != nil {
		childMap = existing
	} else {
		childMap = make(nodeChildren, 2)
		t.childrenChanges[key] = childMap
	}
	childMap[childIndex] = entry
}

func (t *trieView) recordChildChange(key Key, childKey Key, id ids.ID) {
	t.recordChildEntry(key, childKey.Token(key.length, t.tokenSize), &child{
		compressedKey: childKey.Skip(key.length + t.tokenSize),
		id:            id,
	})
}

// Records that a key's value has been added or updated.
// Doesn't actually change the trie data structure.
// That's deferred until we call [calculateNodeIDs].
// Must not be called after [calculateNodeIDs] has returned.
func (t *trieView) recordValueChange(key Key, value maybe.Maybe[[]byte]) error {
	if t.nodesAlreadyCalculated.Get() {
		return ErrNodesAlreadyCalculated
	}

	// update the existing change if it exists
	if existing, ok := t.changes.values[key]; ok {
		existing.after = value
		return nil
	}

	// grab the before value
	before, err := t.getParentTrie().getValue(key)
	if err != nil {
		return err
	}
	t.changes.values[key] = &change[maybe.Maybe[[]byte]]{
		before: before,
		after:  value,
	}
	return nil
}

// Get a copy of the node matching the passed key from the trie.
// Used by views to get nodes from their ancestors.
func (t *trieView) getChildren(key Key) (nodeChildren, error) {
	if t.isInvalid() {
		return nil, ErrInvalid
	}

	// grab the node in question
	n, err := t.getChildrenInternal(key)
	if err != nil {
		return nil, err
	}

	// ensure no ancestor changes occurred during execution
	if t.isInvalid() {
		return nil, ErrInvalid
	}

	// return a clone of the node, so it can be edited without affecting this trie
	return n, nil
}

// Retrieves a node with the given [key].
// If the node is fetched from [t.parentTrie] and [id] isn't empty,
// sets the node's ID to [id].
// If the node is loaded from the baseDB, [hasValue] determines which database the node is stored in.
// Returns database.ErrNotFound if the node doesn't exist.
func (t *trieView) getChildrenInternal(key Key) (nodeChildren, error) {
	// check for the key within the changed nodes
	if childrenChanges, isChanged := t.childrenChanges[key]; isChanged {
		t.db.metrics.ViewNodeCacheHit()
		if childrenChanges == nil {
			return nil, database.ErrNotFound
		}
		return childrenChanges, nil
	}

	// get the node from the parent trie and store a local copy
	parentTrieNode, err := t.getParentTrie().getChildren(key)
	if err != nil {
		return nil, err
	}

	return maps.Clone(parentTrieNode), nil
}

// Get the parent trie of the view
func (t *trieView) getParentTrie() TrieView {
	t.validityTrackingLock.RLock()
	defer t.validityTrackingLock.RUnlock()
	return t.parentTrie
}
