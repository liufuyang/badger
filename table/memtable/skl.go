/*
 * Copyright 2017 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

/*
Adapted from RocksDB inline skiplist.

Key differences:
- No optimization for sequential inserts (no "prev").
- No custom comparator.
- Support overwrites. This requires care when we see the same key when inserting.
  For RocksDB or LevelDB, overwrites are implemented as a newer sequence number in the key, so
	there is no need for values. We don't intend to support versioning. In-place updates of values
	would be more efficient.
- We discard all non-concurrent code.
- We do not support Splices. This simplifies the code a lot.
- No AllocateNode or other pointer arithmetic.
- We combine the findLessThan, findGreaterOrEqual, etc into one function.
*/

package memtable

import (
	"math"
	"sync/atomic"
	"unsafe"

	"github.com/coocood/badger/y"
	"github.com/coocood/rtutil"
)

const (
	maxHeight      = 20
	heightIncrease = math.MaxUint32 / 3
)

// MaxNodeSize is the memory footprint of a node of maximum height.
const (
	MaxNodeSize      = int(unsafe.Sizeof(node{}))
	EstimateNodeSize = MaxNodeSize + nodeAlign
)

type node struct {
	// Multiple parts of the value are encoded as a single uint64 so that it
	// can be atomically loaded and stored:
	//   value offset: uint32 (bits 0-31)
	//   value size  : uint16 (bits 32-47)
	value uint64

	// A byte slice is 24 bytes. We are trying to save space here.
	keyOffset uint32 // Immutable. No need to lock to access key.
	keySize   uint16 // Immutable. No need to lock to access key.

	// Height of the tower.
	height uint16

	// Most nodes do not need to use the full height of the tower, since the
	// probability of each successive level decreases exponentially. Because
	// these elements are never accessed, they do not need to be allocated.
	// Therefore, when a node is allocated in the arena, its memory footprint
	// is deliberately truncated to not include unneeded tower elements.
	//
	// All accesses to elements should use CAS operations, with no need to lock.
	tower [maxHeight]uint32
}

// skiplist maps keys to values (in memory)
type skiplist struct {
	height int32 // Current height. 1 <= height <= kMaxHeight. CAS.
	head   *node
	arena  *arena
}

// DecrRef decrements the refcount, deallocating the Skiplist when done using it
func (s *skiplist) Delete() {
	s.arena.reset()
	// Indicate we are closed. Good for testing.  Also, lets GC reclaim memory. Race condition
	// here would suggest we are accessing skiplist when we are supposed to have no reference!
	s.arena = nil
	s.head = nil
}

func (s *skiplist) valid() bool { return s.arena != nil }

func newNode(a *arena, key y.Key, v y.ValueStruct, height int) *node {
	// The base level is already allocated in the node struct.
	offset := a.putNode(height)
	node := a.getNode(offset)
	node.keyOffset = a.putKey(key)
	node.keySize = uint16(len(key.UserKey) + 8)
	node.height = uint16(height)
	node.value = encodeValue(a.putVal(v), v.EncodedSize())
	return node
}

func encodeValue(valOffset uint32, valSize uint32) uint64 {
	return uint64(valSize)<<32 | uint64(valOffset)
}

func decodeValue(value uint64) (valOffset uint32, valSize uint32) {
	return uint32(value), uint32(value >> 32)
}

// newSkiplist makes a new empty skiplist, with a given arena size
func newSkiplist(arenaSize int64) *skiplist {
	arena := newArena(arenaSize)
	head := newNode(arena, y.Key{}, y.ValueStruct{}, maxHeight)
	return &skiplist{
		height: 1,
		head:   head,
		arena:  arena,
	}
}

func (n *node) getValueOffset() (uint32, uint32) {
	value := atomic.LoadUint64(&n.value)
	return decodeValue(value)
}

func (n *node) key(a *arena) y.Key {
	return a.getKey(n.keyOffset, n.keySize)
}

func (n *node) setValue(a *arena, v y.ValueStruct) {
	valOffset := a.putVal(v)
	value := encodeValue(valOffset, v.EncodedSize())
	atomic.StoreUint64(&n.value, value)
}

func (n *node) getNextOffset(h int) uint32 {
	return atomic.LoadUint32(&n.tower[h])
}

func (n *node) casNextOffset(h int, old, val uint32) bool {
	return atomic.CompareAndSwapUint32(&n.tower[h], old, val)
}

// Returns true if key is strictly > n.key.
// If n is nil, this is an "end" marker and we return false.
//func (s *Skiplist) keyIsAfterNode(key []byte, n *node) bool {
//	y.Assert(n != s.head)
//	return n != nil && y.CompareKeysWithVer(key, n.key) > 0
//}

func (s *skiplist) randomHeight() int {
	h := 1
	for h < maxHeight && rtutil.FastRand() <= heightIncrease {
		h++
	}
	return h
}

func (s *skiplist) getNext(nd *node, height int) *node {
	return s.arena.getNode(nd.getNextOffset(height))
}

// findNear finds the node near to key.
// If less=true, it finds rightmost node such that node.key < key (if allowEqual=false) or
// node.key <= key (if allowEqual=true).
// If less=false, it finds leftmost node such that node.key > key (if allowEqual=false) or
// node.key >= key (if allowEqual=true).
// Returns the node found. The bool returned is true if the node has key equal to given key.
func (s *skiplist) findNear(key y.Key, less bool, allowEqual bool) (*node, bool) {
	x := s.head
	level := int(s.getHeight() - 1)
	var afterNode *node
	for {
		// Assume x.key < key.
		next := s.getNext(x, level)
		if next == nil {
			// x.key < key < END OF LIST
			if level > 0 {
				// Can descend further to iterate closer to the end.
				level--
				continue
			}
			// Level=0. Cannot descend further. Let's return something that makes sense.
			if !less {
				return nil, false
			}
			// Try to return x. Make sure it is not a head node.
			if x == s.head {
				return nil, false
			}
			return x, false
		}
		var cmp int
		if next == afterNode {
			// We compared the same node on the upper level, no need to compare again.
			cmp = -1
		} else {
			nextKey := next.key(s.arena)
			cmp = key.Compare(nextKey)
		}
		if cmp > 0 {
			// x.key < next.key < key. We can continue to move right.
			x = next
			continue
		}
		if cmp == 0 {
			// x.key < key == next.key.
			if allowEqual {
				return next, true
			}
			if !less {
				// We want >, so go to base level to grab the next bigger note.
				return s.getNext(next, 0), false
			}
			// We want <. If not base level, we should go closer in the next level.
			if level > 0 {
				level--
				continue
			}
			// On base level. Return x.
			if x == s.head {
				return nil, false
			}
			return x, false
		}
		// cmp < 0. In other words, x.key < key < next.
		if level > 0 {
			afterNode = next
			level--
			continue
		}
		// At base level. Need to return something.
		if !less {
			return next, false
		}
		// Try to return x. Make sure it is not a head node.
		if x == s.head {
			return nil, false
		}
		return x, false
	}
}

// findSpliceForLevel returns (outBefore, outAfter, match) with outBefore.key < key <= outAfter.key.
// The input "before" tells us where to start looking.
// If we found a node with the same key, then we return match = true.
// Otherwise, outBefore.key < key < outAfter.key.
func (s *skiplist) findSpliceForLevel(key y.Key, before *node, level int) (*node, *node, bool) {
	for {
		// Assume before.key < key.
		next := s.getNext(before, level)
		if next == nil {
			return before, next, false
		}
		nextKey := next.key(s.arena)
		cmp := key.Compare(nextKey)
		if cmp <= 0 {
			return before, next, cmp == 0
		}
		before = next // Keep moving right on this level.
	}
}

func (s *skiplist) getHeight() int32 {
	return atomic.LoadInt32(&s.height)
}

// Put inserts the key-value pair.
func (s *skiplist) Put(key y.Key, v y.ValueStruct) {
	s.PutWithHint(key, v, nil)
}

// Hint is used to speed up sequential write.
type hint struct {
	height int32

	// hitHeight is used to reduce cost of calculateRecomputeHeight.
	// For random workload, comparing hint keys from bottom up is wasted work.
	// So we record the hit height of the last operation, only grow recompute height from near that height.
	hitHeight int32
	prev      [maxHeight + 1]*node
	next      [maxHeight + 1]*node
}

func (s *skiplist) calculateRecomputeHeight(key y.Key, h *hint, listHeight int32) int32 {
	if h.height < listHeight {
		// Either splice is never used or list height has grown, we recompute all.
		h.prev[listHeight] = s.head
		h.next[listHeight] = nil
		h.height = int32(listHeight)
		h.hitHeight = h.height
		return listHeight
	}
	recomputeHeight := h.hitHeight - 2
	if recomputeHeight < 0 {
		recomputeHeight = 0
	}
	for recomputeHeight < listHeight {
		prevNode := h.prev[recomputeHeight]
		nextNode := h.next[recomputeHeight]
		prevNext := s.getNext(prevNode, int(recomputeHeight))
		if prevNext != nextNode {
			recomputeHeight++
			continue
		}
		if prevNode != s.head &&
			prevNode != nil &&
			key.Compare(prevNode.key(s.arena)) <= 0 {
			// Key is before splice.
			for prevNode == h.prev[recomputeHeight] {
				recomputeHeight++
			}
			continue
		}
		if nextNode != nil && key.Compare(nextNode.key(s.arena)) > 0 {
			// Key is after splice.
			for nextNode == h.next[recomputeHeight] {
				recomputeHeight++
			}
			continue
		}
		break
	}
	h.hitHeight = recomputeHeight
	return recomputeHeight
}

// PutWithHint inserts the key-value pair with Hint for better sequential write performance.
func (s *skiplist) PutWithHint(key y.Key, v y.ValueStruct, h *hint) {
	// Since we allow overwrite, we may not need to create a new node. We might not even need to
	// increase the height. Let's defer these actions.
	listHeight := s.getHeight()
	height := s.randomHeight()

	// Try to increase s.height via CAS.
	for height > int(listHeight) {
		if atomic.CompareAndSwapInt32(&s.height, listHeight, int32(height)) {
			// Successfully increased skiplist.height.
			listHeight = int32(height)
			break
		}
		listHeight = s.getHeight()
	}
	spliceIsValid := h != nil
	if h == nil {
		h = new(hint)
	}
	recomputeHeight := s.calculateRecomputeHeight(key, h, listHeight)
	if recomputeHeight > 0 {
		for i := recomputeHeight - 1; i >= 0; i-- {
			var match bool
			h.prev[i], h.next[i], match = s.findSpliceForLevel(key, h.prev[i+1], int(i))
			if match {
				// In place update.
				h.next[i].setValue(s.arena, v)
				for i > 0 {
					h.prev[i-1] = h.prev[i]
					h.next[i-1] = h.next[i]
					i--
				}
				return
			}
		}
	}

	// We do need to create a new node.
	x := newNode(s.arena, key, v, height)

	// We always insert from the base level and up. After you add a node in base level, we cannot
	// create a node in the level above because it would have discovered the node in the base level.
	for i := 0; i < height; i++ {
		for {
			nextOffset := s.arena.getNodeOffset(h.next[i])
			x.tower[i] = nextOffset
			if h.prev[i].casNextOffset(i, nextOffset, s.arena.getNodeOffset(x)) {
				// Managed to insert x between prev[i] and next[i]. Go to the next level.
				break
			}
			// CAS failed. We need to recompute prev and next.
			// It is unlikely to be helpful to try to use a different level as we redo the search,
			// because it is unlikely that lots of nodes are inserted between prev[i] and next[i].
			h.prev[i], h.next[i], _ = s.findSpliceForLevel(key, h.prev[i], i)
			if i > 0 {
				spliceIsValid = false
			}
		}
	}
	if spliceIsValid {
		for i := 0; i < height; i++ {
			h.prev[i] = x
			h.next[i] = s.getNext(x, i)
		}
	} else {
		h.height = 0
	}
}

func (s *skiplist) GetWithHint(key y.Key, h *hint) y.ValueStruct {
	if h == nil {
		h = new(hint)
	}
	listHeight := s.getHeight()
	recomputeHeight := s.calculateRecomputeHeight(key, h, listHeight)
	var n *node
	if recomputeHeight > 0 {
		for i := recomputeHeight - 1; i >= 0; i-- {
			var match bool
			h.prev[i], h.next[i], match = s.findSpliceForLevel(key, h.prev[i+1], int(i))
			if match {
				n = h.next[i]
				for j := i; j >= 0; j-- {
					h.prev[j] = n
					h.next[j] = s.getNext(n, int(j))
				}
				break
			}
		}
	} else {
		n = h.next[0]
	}
	if n == nil {
		return y.ValueStruct{}
	}
	nextKey := s.arena.getKey(n.keyOffset, n.keySize)
	if !key.SameUserKey(nextKey) {
		return y.ValueStruct{}
	}
	valOffset, valSize := n.getValueOffset()
	vs := s.arena.getVal(valOffset, valSize)
	vs.Version = nextKey.Version
	return vs
}

// Empty returns if the Skiplist is empty.
func (s *skiplist) Empty() bool {
	return s.findLast() == nil
}

// findLast returns the last element. If head (empty list), we return nil. All the find functions
// will NEVER return the head nodes.
func (s *skiplist) findLast() *node {
	n := s.head
	level := int(s.getHeight()) - 1
	for {
		next := s.getNext(n, level)
		if next != nil {
			n = next
			continue
		}
		if level == 0 {
			if n == s.head {
				return nil
			}
			return n
		}
		level--
	}
}

// Get gets the value associated with the key. It returns a valid value if it finds equal or earlier
// version of the same key.
func (s *skiplist) Get(key y.Key) y.ValueStruct {
	n, _ := s.findNear(key, false, true) // findGreaterOrEqual.
	if n == nil {
		return y.ValueStruct{}
	}

	nextKey := s.arena.getKey(n.keyOffset, n.keySize)
	if !key.SameUserKey(nextKey) {
		return y.ValueStruct{}
	}

	valOffset, valSize := n.getValueOffset()
	vs := s.arena.getVal(valOffset, valSize)
	vs.Version = nextKey.Version
	return vs
}

// NewIterator returns a skiplist iterator.  You have to Close() the iterator.
func (s *skiplist) NewIterator() *Iterator {
	return &Iterator{list: s}
}

// MemSize returns the size of the Skiplist in terms of how much memory is used within its internal
// arena.
func (s *skiplist) MemSize() int64 { return s.arena.size() }

// Iterator is an iterator over skiplist object. For new objects, you just
// need to initialize Iterator.list.
type Iterator struct {
	list *skiplist
	n    *node
}

// Valid returns true iff the iterator is positioned at a valid node.
func (s *Iterator) Valid() bool { return s.n != nil }

// Key returns the key at the current position.
func (s *Iterator) Key() y.Key {
	return s.list.arena.getKey(s.n.keyOffset, s.n.keySize)
}

// Value returns value.
func (s *Iterator) Value() y.ValueStruct {
	valOffset, valSize := s.n.getValueOffset()
	return s.list.arena.getVal(valOffset, valSize)
}

// FillValue fills value.
func (s *Iterator) FillValue(vs *y.ValueStruct) {
	valOffset, valSize := s.n.getValueOffset()
	s.list.arena.fillVal(vs, valOffset, valSize)
}

// Next advances to the next position.
func (s *Iterator) Next() {
	y.Assert(s.Valid())
	s.n = s.list.getNext(s.n, 0)
}

// Prev advances to the previous position.
func (s *Iterator) Prev() {
	y.Assert(s.Valid())
	s.n, _ = s.list.findNear(s.Key(), true, false) // find <. No equality allowed.
}

// Seek advances to the first entry with a key >= target.
func (s *Iterator) Seek(target y.Key) {
	s.n, _ = s.list.findNear(target, false, true) // find >=.
}

// SeekForPrev finds an entry with key <= target.
func (s *Iterator) SeekForPrev(target y.Key) {
	s.n, _ = s.list.findNear(target, true, true) // find <=.
}

// SeekToFirst seeks position at the first entry in list.
// Final state of iterator is Valid() iff list is not empty.
func (s *Iterator) SeekToFirst() {
	s.n = s.list.getNext(s.list.head, 0)
}

// SeekToLast seeks position at the last entry in list.
// Final state of iterator is Valid() iff list is not empty.
func (s *Iterator) SeekToLast() {
	s.n = s.list.findLast()
}

// UniIterator is a unidirectional memtable iterator. It is a thin wrapper around
// Iterator. We like to keep Iterator as before, because it is more powerful and
// we might support bidirectional iterators in the future.
type UniIterator struct {
	iter     *Iterator
	reversed bool
}

// NewUniIterator returns a UniIterator.
func (s *skiplist) NewUniIterator(reversed bool) *UniIterator {
	return &UniIterator{
		iter:     s.NewIterator(),
		reversed: reversed,
	}
}

// Next implements y.Interface
func (s *UniIterator) Next() {
	if !s.reversed {
		s.iter.Next()
	} else {
		s.iter.Prev()
	}
}

// Rewind implements y.Interface
func (s *UniIterator) Rewind() {
	if !s.reversed {
		s.iter.SeekToFirst()
	} else {
		s.iter.SeekToLast()
	}
}

// Seek implements y.Interface
func (s *UniIterator) Seek(key y.Key) {
	if !s.reversed {
		s.iter.Seek(key)
	} else {
		s.iter.SeekForPrev(key)
	}
}

// Key implements y.Interface
func (s *UniIterator) Key() y.Key { return s.iter.Key() }

// Value implements y.Interface
func (s *UniIterator) Value() y.ValueStruct { return s.iter.Value() }

// FillValue implements y.Interface
func (s *UniIterator) FillValue(vs *y.ValueStruct) { s.iter.FillValue(vs) }

// Valid implements y.Interface
func (s *UniIterator) Valid() bool { return s.iter.Valid() }