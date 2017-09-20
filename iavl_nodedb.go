package iavl

import (
	"bytes"
	"container/list"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/syndtr/goleveldb/leveldb/iterator"
	"github.com/syndtr/goleveldb/leveldb/util"

	cmn "github.com/tendermint/tmlibs/common"
	dbm "github.com/tendermint/tmlibs/db"
)

var (
	// All node keys are prefixed with this. This ensures no collision is
	// possible with the other keys, and makes them easier to traverse.
	nodesPrefix = "nodes/"
	nodesKeyFmt = "nodes/%x"

	// Orphans are keyed in the database by their expected lifetime.
	// The first number represents the *last* version at which the orphan needs
	// to exist, while the second number represents the *earliest* version at
	// which it is expected to exist - which starts out by being the version
	// of the node being orphaned.
	orphansPrefix    = "orphans/"
	orphansPrefixFmt = "orphans/%d/"      // orphans/<version>/
	orphansKeyFmt    = "orphans/%d/%d/%x" // orphans/<version>/<version>/<hash>

	// These keys are used for the orphan reverse-lookups by node hash.
	orphansIndexPrefix = "orphans-index/"
	orphansIndexKeyFmt = "orphans-index/%x"

	// roots/<version>
	rootsPrefix    = "roots/"
	rootsPrefixFmt = "roots/%d"
)

type nodeDB struct {
	mtx   sync.Mutex // Read/write lock.
	db    dbm.DB     // Persistent node storage.
	batch dbm.Batch  // Batched writing buffer.

	versionCache  map[uint64][]byte // Cache of tree (root) versions.
	latestVersion uint64            // Latest root version.

	nodeCache      map[string]*list.Element // Node cache.
	nodeCacheSize  int                      // Node cache size limit in elements.
	nodeCacheQueue *list.List               // LRU queue of cache elements. Used for deletion.
}

func newNodeDB(cacheSize int, db dbm.DB) *nodeDB {
	ndb := &nodeDB{
		nodeCache:      make(map[string]*list.Element),
		nodeCacheSize:  cacheSize,
		nodeCacheQueue: list.New(),
		db:             db,
		batch:          db.NewBatch(),
		versionCache:   map[uint64][]byte{},
	}
	return ndb
}

// GetNode gets a node from cache or disk. If it is an inner node, it does not
// load its children.
func (ndb *nodeDB) GetNode(hash []byte) *IAVLNode {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()

	// Check the cache.
	if elem, ok := ndb.nodeCache[string(hash)]; ok {
		// Already exists. Move to back of nodeCacheQueue.
		ndb.nodeCacheQueue.MoveToBack(elem)
		return elem.Value.(*IAVLNode)
	}

	// Doesn't exist, load.
	buf := ndb.db.Get(ndb.nodeKey(hash))
	if len(buf) == 0 {
		cmn.PanicSanity(cmn.Fmt("Value missing for key %x", hash))
	}

	node, err := MakeIAVLNode(buf)
	if err != nil {
		cmn.PanicCrisis(cmn.Fmt("Error reading IAVLNode. bytes: %x, error: %v", buf, err))
	}

	node.hash = hash
	node.persisted = true
	ndb.cacheNode(node)

	return node
}

// SaveNode saves a node to disk.
func (ndb *nodeDB) SaveNode(node *IAVLNode) {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()

	if node.hash == nil {
		cmn.PanicSanity("Expected to find node.hash, but none found.")
	}
	if node.persisted {
		cmn.PanicSanity("Shouldn't be calling save on an already persisted node.")
	}

	// Don't overwrite nodes.
	// This is here because inner nodes are overwritten otherwise, losing
	// version information, due to the version not affecting the hash.
	// Adding the version to the hash breaks a lot of things, so this
	// seems like the best solution for now.
	if ndb.Has(node.hash) {
		return
	}

	// Save node bytes to db.
	buf := new(bytes.Buffer)
	if _, err := node.writeBytes(buf); err != nil {
		cmn.PanicCrisis(err)
	}
	ndb.batch.Set(ndb.nodeKey(node.hash), buf.Bytes())

	node.persisted = true
	ndb.cacheNode(node)
}

// Has checks if a hash exists in the database.
func (ndb *nodeDB) Has(hash []byte) bool {
	key := ndb.nodeKey(hash)

	if ldb, ok := ndb.db.(*dbm.GoLevelDB); ok {
		exists, err := ldb.DB().Has(key, nil)
		if err != nil {
			cmn.PanicSanity("Got error from leveldb: " + err.Error())
		}
		return exists
	}
	return len(ndb.db.Get(key)) != 0
}

// SaveBranch saves the given node and all of its descendants. For each node
// about to be saved, the supplied callback is called and the returned node is
// is saved. You may pass nil as the callback as a pass-through.
//
// Note that this function clears leftNode/rigthNode recursively and calls
// hashWithCount on the given node.
func (ndb *nodeDB) SaveBranch(node *IAVLNode, cb func(*IAVLNode)) {
	if node.hash == nil {
		node.hash, _ = node.hashWithCount()
	}
	if node.persisted {
		return
	}

	if node.leftNode != nil {
		ndb.SaveBranch(node.leftNode, cb)
		node.leftNode = nil
	}
	if node.rightNode != nil {
		ndb.SaveBranch(node.rightNode, cb)
		node.rightNode = nil
	}

	if cb != nil {
		cb(node)
	}
	ndb.SaveNode(node)
}

// DeleteVersion deletes a tree version from disk.
func (ndb *nodeDB) DeleteVersion(version uint64) {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()

	ndb.deleteOrphans(version)
	ndb.deleteRoot(version)
}

// Unorphan deletes the orphan entry from disk, but not the node it points to.
func (ndb *nodeDB) Unorphan(hash []byte) {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()

	indexKey := []byte(fmt.Sprintf(orphansIndexKeyFmt, hash))

	if orphansKey := ndb.db.Get(indexKey); len(orphansKey) > 0 {
		ndb.batch.Delete(orphansKey)
		ndb.batch.Delete(indexKey)
	}
}

// Saves orphaned nodes to disk under a special prefix.
func (ndb *nodeDB) SaveOrphans(version uint64, orphans map[string]uint64) {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()

	toVersion := ndb.getPreviousVersion(version)

	for hash, fromVersion := range orphans {
		ndb.saveOrphan([]byte(hash), fromVersion, toVersion)
	}
}

// Saves a single orphan to disk.
func (ndb *nodeDB) saveOrphan(hash []byte, fromVersion, toVersion uint64) {
	if fromVersion > toVersion {
		cmn.PanicSanity("Orphan expires before it comes alive")
	}
	key := fmt.Sprintf(orphansKeyFmt, toVersion, fromVersion, hash)
	ndb.batch.Set([]byte(key), hash)

	// Set reverse-lookup index.
	indexKey := fmt.Sprintf(orphansIndexKeyFmt, hash)
	ndb.batch.Set([]byte(indexKey), []byte(key))
}

// deleteOrphans deletes orphaned nodes from disk, and the associated orphan
// entries.
func (ndb *nodeDB) deleteOrphans(version uint64) {
	// Will be zero if there is no previous version.
	predecessor := ndb.getPreviousVersion(version)

	// Traverse orphans with a lifetime ending at the version specified.
	ndb.traverseOrphansVersion(version, func(key, hash []byte) {
		var fromVersion, toVersion uint64

		// See comment on `orphansKeyFmt`. Note that here, `version` and
		// `toVersion` are always equal.
		fmt.Sscanf(string(key), orphansKeyFmt, &toVersion, &fromVersion)

		// Delete orphan key and reverse-lookup key.
		ndb.batch.Delete(key)
		ndb.batch.Delete([]byte(fmt.Sprintf(orphansIndexKeyFmt, hash)))

		// If there is no predecessor, or the predecessor is earlier than the
		// beginning of the lifetime (ie: negative lifetime), or the lifetime
		// spans a single version and that version is the one being deleted, we
		// can delete the orphan.  Otherwise, we shorten its lifetime, by
		// moving its endpoint to the previous version.
		if predecessor < fromVersion || fromVersion == toVersion {
			ndb.batch.Delete(ndb.nodeKey(hash))
			ndb.uncacheNode(hash)
		} else {
			ndb.saveOrphan(hash, fromVersion, predecessor)
		}
	})
}

func (ndb *nodeDB) nodeKey(hash []byte) []byte {
	return []byte(fmt.Sprintf(nodesKeyFmt, hash))
}

func (ndb *nodeDB) getLatestVersion() uint64 {
	if ndb.latestVersion == 0 {
		ndb.getVersions()
	}
	return ndb.latestVersion
}

func (ndb *nodeDB) getVersions() map[uint64][]byte {
	if len(ndb.versionCache) == 0 {
		ndb.traversePrefix([]byte(rootsPrefix), func(k, hash []byte) {
			var version uint64
			fmt.Sscanf(string(k), rootsPrefixFmt, &version)
			ndb.cacheVersion(version, hash)
		})
	}
	return ndb.versionCache
}

func (ndb *nodeDB) cacheVersion(version uint64, hash []byte) {
	ndb.versionCache[version] = hash

	if version > ndb.getLatestVersion() {
		ndb.latestVersion = version
	}
}

func (ndb *nodeDB) getPreviousVersion(version uint64) uint64 {
	var result uint64 = 0
	for v, _ := range ndb.getVersions() {
		if v < version && (result == 0 || v > result) {
			result = v
		}
	}
	return result
}

// deleteRoot deletes the root entry from disk, but not the node it points to.
func (ndb *nodeDB) deleteRoot(version uint64) {
	key := fmt.Sprintf(rootsPrefixFmt, version)
	ndb.batch.Delete([]byte(key))

	delete(ndb.versionCache, version)

	if version == ndb.getLatestVersion() {
		cmn.PanicSanity("Tried to delete latest version")
	}
}

func (ndb *nodeDB) traverseOrphans(fn func(k, v []byte)) {
	ndb.traversePrefix([]byte(orphansPrefix), fn)
}

// Traverse orphans ending at a certain version.
func (ndb *nodeDB) traverseOrphansVersion(version uint64, fn func(k, v []byte)) {
	prefix := fmt.Sprintf(orphansPrefixFmt, version)
	ndb.traversePrefix([]byte(prefix), fn)
}

// Traverse all keys with a certain prefix.
func (ndb *nodeDB) traversePrefix(prefix []byte, fn func(k, v []byte)) {
	if ldb, ok := ndb.db.(*dbm.GoLevelDB); ok {
		it := ldb.DB().NewIterator(util.BytesPrefix([]byte(prefix)), nil)
		for it.Next() {
			k := make([]byte, len(it.Key()))
			v := make([]byte, len(it.Value()))

			// Leveldb reuses the memory, we are forced to copy.
			copy(k, it.Key())
			copy(v, it.Value())

			fn(k, v)
		}
		if err := it.Error(); err != nil {
			cmn.PanicSanity(err.Error())
		}
		it.Release()
	} else {
		ndb.traverse(func(key, value []byte) {
			if bytes.HasPrefix(key, prefix) {
				fn(key, value)
			}
		})
	}
}

func (ndb *nodeDB) uncacheNode(hash []byte) {
	if elem, ok := ndb.nodeCache[string(hash)]; ok {
		ndb.nodeCacheQueue.Remove(elem)
		delete(ndb.nodeCache, string(hash))
	}
}

// Add a node to the cache and pop the least recently used node if we've
// reached the cache size limit.
func (ndb *nodeDB) cacheNode(node *IAVLNode) {
	elem := ndb.nodeCacheQueue.PushBack(node)
	ndb.nodeCache[string(node.hash)] = elem

	if ndb.nodeCacheQueue.Len() > ndb.nodeCacheSize {
		oldest := ndb.nodeCacheQueue.Front()
		hash := ndb.nodeCacheQueue.Remove(oldest).(*IAVLNode).hash
		delete(ndb.nodeCache, string(hash))
	}
}

// Write to disk.
func (ndb *nodeDB) Commit() {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()

	ndb.batch.Write()
	ndb.batch = ndb.db.NewBatch()
}

func (ndb *nodeDB) getRoots() ([][]byte, error) {
	roots := [][]byte{}

	ndb.traversePrefix([]byte(rootsPrefix), func(k, v []byte) {
		roots = append(roots, v)
	})
	return roots, nil
}

// SaveRoot creates an entry on disk for the given root, so that it can be
// loaded later.
func (ndb *nodeDB) SaveRoot(root *IAVLNode, version uint64) error {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()

	if len(root.hash) == 0 {
		cmn.PanicSanity("Hash should not be empty")
	}
	if version <= ndb.getLatestVersion() {
		return errors.New("can't save root with lower or equal version than latest")
	}

	// Note that we don't use the version attribute of the root. This is
	// because we might be saving an old root at a new version in the case
	// where the tree wasn't modified between versions.
	key := fmt.Sprintf(rootsPrefixFmt, version)
	ndb.batch.Set([]byte(key), root.hash)
	ndb.cacheVersion(version, root.hash)

	return nil
}

///////////////////////////////////////////////////////////////////////////////

func (ndb *nodeDB) leafNodes() []*IAVLNode {
	leaves := []*IAVLNode{}

	ndb.traverseNodes(func(hash []byte, node *IAVLNode) {
		if node.isLeaf() {
			leaves = append(leaves, node)
		}
	})
	return leaves
}

func (ndb *nodeDB) nodes() []*IAVLNode {
	nodes := []*IAVLNode{}

	ndb.traverseNodes(func(hash []byte, node *IAVLNode) {
		nodes = append(nodes, node)
	})
	return nodes
}

func (ndb *nodeDB) orphans() [][]byte {
	orphans := [][]byte{}

	ndb.traverseOrphans(func(k, v []byte) {
		orphans = append(orphans, v)
	})
	return orphans
}

func (ndb *nodeDB) roots() [][]byte {
	roots, _ := ndb.getRoots()
	return roots
}

func (ndb *nodeDB) size() int {
	it := ndb.db.Iterator()
	size := 0

	for it.Next() {
		size++
	}
	return size
}

func (ndb *nodeDB) traverse(fn func(key, value []byte)) {
	it := ndb.db.Iterator()

	for it.Next() {
		k := make([]byte, len(it.Key()))
		v := make([]byte, len(it.Value()))

		// Leveldb reuses the memory, we are forced to copy.
		copy(k, it.Key())
		copy(v, it.Value())

		fn(k, v)
	}
	if iter, ok := it.(iterator.Iterator); ok {
		if err := iter.Error(); err != nil {
			cmn.PanicSanity(err.Error())
		}
		iter.Release()
	}
}

func (ndb *nodeDB) traverseNodes(fn func(hash []byte, node *IAVLNode)) {
	nodes := []*IAVLNode{}

	ndb.traversePrefix([]byte(nodesPrefix), func(key, value []byte) {
		node, err := MakeIAVLNode(value)
		if err != nil {
			cmn.PanicSanity("Couldn't decode node from database")
		}
		fmt.Sscanf(string(key), nodesKeyFmt, &node.hash)
		nodes = append(nodes, node)
	})

	sort.Slice(nodes, func(i, j int) bool {
		return bytes.Compare(nodes[i].key, nodes[j].key) < 0
	})

	for _, n := range nodes {
		fn(n.hash, n)
	}
}

func (ndb *nodeDB) String() string {
	var str string
	index := 0

	ndb.traversePrefix([]byte(rootsPrefix), func(key, value []byte) {
		str += fmt.Sprintf("%s: %x\n", string(key), value)
	})
	str += "\n"

	ndb.traverseOrphans(func(key, value []byte) {
		str += fmt.Sprintf("%s: %x\n", string(key), value)
	})
	str += "\n"

	ndb.traversePrefix([]byte(orphansIndexPrefix), func(key, value []byte) {
		str += fmt.Sprintf("%s: %s\n", string(key), value)
	})
	str += "\n"

	ndb.traverseNodes(func(hash []byte, node *IAVLNode) {
		if len(hash) == 0 {
			str += fmt.Sprintf("<nil>\n")
		} else if node == nil {
			str += fmt.Sprintf("%s%40x: <nil>\n", nodesPrefix, hash)
		} else if node.value == nil && node.height > 0 {
			str += fmt.Sprintf("%s%40x: %s   %-16s h=%d version=%d\n", nodesPrefix, hash, node.key, "", node.height, node.version)
		} else {
			str += fmt.Sprintf("%s%40x: %s = %-16s h=%d version=%d\n", nodesPrefix, hash, node.key, node.value, node.height, node.version)
		}
		index++
	})
	return "-" + "\n" + str + "-"
}
