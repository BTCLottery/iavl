package iavl

import (
	"bytes"
	"container/list"
	"fmt"
	"sort"
	"sync"

	dbm "github.com/tendermint/tendermint/libs/db"
)

// This NodeDB implementations tries to reduce contention between readers & a single writer.
//
// nodeDB has a single mutex that had to be acquired by a reader/writer before they can access
// either the node cache or the underlying DB. nodeDB2 adds a second mutex to synchronize access to
// the node cache, while the original mutex is now only used to synchronize write access to the
// underlying DB batch.
//
// Multiple readers may now access the underlying DB concurrently, which means the underlying DB
// must support concurrent readers (both GoLevelDB and CLevelDB support this).
type nodeDB2 struct {
	db       dbm.DB     // Persistent node storage.
	batchMtx sync.Mutex // Read/write lock to protect the batch.
	batch    dbm.Batch  // Batched writing buffer.

	latestVersion  int64
	nodeCacheMtx   sync.Mutex               // Read/write lock to protect the node cache.
	nodeCache      map[string]*list.Element // Node cache.
	nodeCacheSize  int                      // Node cache size limit in elements.
	nodeCacheQueue *list.List               // LRU queue of cache elements. Used for deletion.
	getLeafValueCb func(key []byte) []byte  // Optional callback to get values stored in leaf nodes.
}

var _ NodeDB = (*nodeDB2)(nil)

// NewNodeDB2 returns a new instance
func NewNodeDB2(db dbm.DB, cacheSize int, getLeafValueCb func(key []byte) []byte) NodeDB {
	ndb := &nodeDB2{
		db:             db,
		batch:          db.NewBatch(),
		latestVersion:  0, // initially invalid
		nodeCache:      make(map[string]*list.Element),
		nodeCacheSize:  cacheSize,
		nodeCacheQueue: list.New(),
		getLeafValueCb: getLeafValueCb,
	}
	return ndb
}

// GetNode gets a node from cache or disk. If it is an inner node, it does not
// load its children.
func (ndb *nodeDB2) GetNode(hash []byte) *Node {
	if len(hash) == 0 {
		panic("nodeDB.GetNode() requires hash")
	}

	// Check the cache.
	node := ndb.getCachedNode(hash)
	if node != nil {
		return node
	}

	// Doesn't exist, load.
	buf := ndb.db.Get(ndb.nodeKey(hash))
	if buf == nil {
		panic(fmt.Sprintf("Value missing for hash %x corresponding to nodeKey %s", hash, ndb.nodeKey(hash)))
	}

	node, err := MakeNode(buf, ndb.getLeafValueCb)
	if err != nil {
		panic(fmt.Sprintf("Error reading Node. bytes: %x, error: %v", buf, err))
	}

	node.hash = hash
	node.persisted = true
	ndb.cacheNode(node)

	return node
}

// SaveNode saves a node to disk.
func (ndb *nodeDB2) SaveNode(node *Node, flushToDisk bool) {
	if node.hash == nil {
		panic("Expected to find node.hash, but none found.")
	}
	if node.persisted {
		panic("Shouldn't be calling save on an already persisted node.")
	}

	ndb.writeNode(node)
	ndb.cacheNode(node)
}

func (ndb *nodeDB2) writeNode(node *Node) {
	ndb.batchMtx.Lock()
	defer ndb.batchMtx.Unlock()

	// Save node bytes to db.
	buf := new(bytes.Buffer)
	if err := node.writeBytes(buf, ndb.getLeafValueCb == nil); err != nil {
		panic(err)
	}

	ndb.batch.Set(ndb.nodeKey(node.hash), buf.Bytes())
	debug("BATCH SAVE %X %p\n", node.hash, node)

	node.persisted = true
}

// Has checks if a hash exists in the database.
func (ndb *nodeDB2) Has(hash []byte) bool {
	key := ndb.nodeKey(hash)

	if ldb, ok := ndb.db.(*dbm.GoLevelDB); ok {
		exists, err := ldb.DB().Has(key, nil)
		if err != nil {
			panic("Got error from leveldb: " + err.Error())
		}
		return exists
	}
	return ndb.db.Get(key) != nil
}

func (ndb *nodeDB2) SaveBranch(node *Node, flushToDisk bool) []byte {
	return ndb.saveBranch(node)
}

// SaveBranch saves the given node and all of its descendants.
// NOTE: This function clears leftNode/rigthNode recursively and
// calls _hash() on the given node.
// TODO refactor, maybe use hashWithCount() but provide a callback.
func (ndb *nodeDB2) saveBranch(node *Node) []byte {
	if node.persisted {
		return node.hash
	}

	if node.leftNode != nil {
		node.leftHash = ndb.saveBranch(node.leftNode)
	}
	if node.rightNode != nil {
		node.rightHash = ndb.saveBranch(node.rightNode)
	}

	node._hash()
	ndb.SaveNode(node, true)

	node.leftNode = nil
	node.rightNode = nil

	return node.hash
}

// DeleteVersion deletes a tree version from disk.
func (ndb *nodeDB2) DeleteVersion(version int64, checkLatestVersion bool) {
	ndb.batchMtx.Lock()
	defer ndb.batchMtx.Unlock()

	ndb.deleteOrphans(version)
	ndb.deleteRoot(version, checkLatestVersion)
}

func (ndb *nodeDB2) DeleteMemoryVersion(version, previous int64, _ *map[string]int64) {
	ndb.DeleteVersion(version, false)
}

// Saves orphaned nodes to disk under a special prefix.
// version: the new version being saved.
// orphans: the orphan nodes created since version-1
func (ndb *nodeDB2) SaveOrphans(version int64, orphans map[string]int64) {
	ndb.batchMtx.Lock()
	defer ndb.batchMtx.Unlock()

	toVersion := ndb.getPreviousVersion(version)
	for hash, fromVersion := range orphans {
		debug("SAVEORPHAN %v-%v %X\n", fromVersion, toVersion, hash)
		ndb.saveOrphan([]byte(hash), fromVersion, toVersion)
	}
}

// Saves a single orphan to disk.
func (ndb *nodeDB2) saveOrphan(hash []byte, fromVersion, toVersion int64) {
	if fromVersion > toVersion {
		panic(fmt.Sprintf("Orphan expires before it comes alive.  %d > %d", fromVersion, toVersion))
	}
	key := ndb.orphanKey(fromVersion, toVersion, hash)
	ndb.batch.Set(key, hash)
}

// deleteOrphans deletes orphaned nodes from disk, and the associated orphan
// entries.
func (ndb *nodeDB2) deleteOrphans(version int64) {
	// Will be zero if there is no previous version.
	predecessor := ndb.getPreviousVersion(version)

	// Traverse orphans with a lifetime ending at the version specified.
	// TODO optimize.
	ndb.traverseOrphansVersion(version, func(key, hash []byte) {
		var fromVersion, toVersion int64

		// See comment on `orphanKeyFmt`. Note that here, `version` and
		// `toVersion` are always equal.
		orphanKeyFormat.Scan(key, &toVersion, &fromVersion)

		// Delete orphan key and reverse-lookup key.
		ndb.batch.Delete(key)

		// If there is no predecessor, or the predecessor is earlier than the
		// beginning of the lifetime (ie: negative lifetime), or the lifetime
		// spans a single version and that version is the one being deleted, we
		// can delete the orphan.  Otherwise, we shorten its lifetime, by
		// moving its endpoint to the previous version.
		if predecessor < fromVersion || fromVersion == toVersion {
			debug("DELETE predecessor:%v fromVersion:%v toVersion:%v %X\n", predecessor, fromVersion, toVersion, hash)
			ndb.batch.Delete(ndb.nodeKey(hash))
			ndb.uncacheNode(hash)
		} else {
			debug("MOVE predecessor:%v fromVersion:%v toVersion:%v %X\n", predecessor, fromVersion, toVersion, hash)
			ndb.saveOrphan(hash, fromVersion, predecessor)
		}
	})
}

func (ndb *nodeDB2) nodeKey(hash []byte) []byte {
	return nodeKeyFormat.KeyBytes(hash)
}

func (ndb *nodeDB2) orphanKey(fromVersion, toVersion int64, hash []byte) []byte {
	return orphanKeyFormat.Key(toVersion, fromVersion, hash)
}

func (ndb *nodeDB2) rootKey(version int64) []byte {
	return rootKeyFormat.Key(version)
}

func (ndb *nodeDB2) getLatestVersion() int64 {
	if ndb.latestVersion == 0 {
		ndb.latestVersion = ndb.getPreviousVersion(1<<63 - 1)
	}
	return ndb.latestVersion
}

func (ndb *nodeDB2) updateLatestVersion(version int64) {
	if ndb.latestVersion < version {
		ndb.latestVersion = version
	}
}

func (ndb *nodeDB2) resetLatestVersion(version int64) {
	ndb.latestVersion = version
}

func (ndb *nodeDB2) getPreviousVersion(version int64) int64 {
	itr := ndb.db.ReverseIterator(
		rootKeyFormat.Key(1),
		rootKeyFormat.Key(version),
	)
	defer itr.Close()

	pversion := int64(-1)
	for itr.Valid() {
		k := itr.Key()
		rootKeyFormat.Scan(k, &pversion)
		return pversion
	}

	return 0
}

func (ndb *nodeDB2) ResetMemNodes() {
}

func (ndb *nodeDB2) ResetBatch() {
	ndb.batch = ndb.db.NewBatch()
}
func (ndb *nodeDB2) RestMemBatch() {
	ndb.batch = ndb.db.NewBatch()
}

// deleteRoot deletes the root entry from disk, but not the node it points to.
func (ndb *nodeDB2) deleteRoot(version int64, checkLatestVersion bool) {
	if checkLatestVersion && version == ndb.getLatestVersion() {
		panic("Tried to delete latest version")
	}

	key := ndb.rootKey(version)
	ndb.batch.Delete(key)
}

func (ndb *nodeDB2) traverseOrphans(fn func(k, v []byte)) {
	ndb.traversePrefix(orphanKeyFormat.Key(), fn)
}

// Traverse orphans ending at a certain version.
func (ndb *nodeDB2) traverseOrphansVersion(version int64, fn func(k, v []byte)) {
	ndb.traversePrefix(orphanKeyFormat.Key(version), fn)
}

// Traverse all keys.
func (ndb *nodeDB2) traverse(fn func(key, value []byte)) {
	itr := ndb.db.Iterator(nil, nil)
	defer itr.Close()

	for ; itr.Valid(); itr.Next() {
		fn(itr.Key(), itr.Value())
	}
}

// Traverse all keys with a certain prefix.
func (ndb *nodeDB2) traversePrefix(prefix []byte, fn func(k, v []byte)) {
	itr := dbm.IteratePrefix(ndb.db, prefix)
	defer itr.Close()

	for ; itr.Valid(); itr.Next() {
		fn(itr.Key(), itr.Value())
	}
}

func (ndb *nodeDB2) getCachedNode(hash []byte) *Node {
	ndb.nodeCacheMtx.Lock()
	defer ndb.nodeCacheMtx.Unlock()

	if elem, ok := ndb.nodeCache[string(hash)]; ok {
		// Already exists. Move to back of nodeCacheQueue.
		ndb.nodeCacheQueue.MoveToBack(elem)
		return elem.Value.(*Node)
	}

	return nil
}

func (ndb *nodeDB2) uncacheNode(hash []byte) {
	ndb.nodeCacheMtx.Lock()
	defer ndb.nodeCacheMtx.Unlock()

	if elem, ok := ndb.nodeCache[string(hash)]; ok {
		ndb.nodeCacheQueue.Remove(elem)
		delete(ndb.nodeCache, string(hash))
	}
}

// Add a node to the cache and pop the least recently used node if we've
// reached the cache size limit.
func (ndb *nodeDB2) cacheNode(node *Node) {
	ndb.nodeCacheMtx.Lock()
	defer ndb.nodeCacheMtx.Unlock()

	elem := ndb.nodeCacheQueue.PushBack(node)
	ndb.nodeCache[string(node.hash)] = elem

	if ndb.nodeCacheQueue.Len() > ndb.nodeCacheSize {
		oldest := ndb.nodeCacheQueue.Front()
		hash := ndb.nodeCacheQueue.Remove(oldest).(*Node).hash
		delete(ndb.nodeCache, string(hash))
	}
}

// Write to disk.
func (ndb *nodeDB2) Commit() {
	ndb.batchMtx.Lock()
	defer ndb.batchMtx.Unlock()

	ndb.batch.Write()
	ndb.batch = ndb.db.NewBatch()
}

func (ndb *nodeDB2) getRoot(version int64) []byte {
	return ndb.db.Get(ndb.rootKey(version))
}

func (ndb *nodeDB2) getRoots() (map[int64][]byte, error) {
	roots := map[int64][]byte{}

	ndb.traversePrefix(rootKeyFormat.Key(), func(k, v []byte) {
		var version int64
		rootKeyFormat.Scan(k, &version)
		roots[version] = v
	})
	return roots, nil
}

// SaveRoot creates an entry on disk for the given root, so that it can be
// loaded later.
func (ndb *nodeDB2) SaveRoot(root *Node, version int64) error {
	if len(root.hash) == 0 {
		panic("Hash should not be empty")
	}
	return ndb.saveRoot(root.hash, version)
}

// SaveEmptyRoot creates an entry on disk for an empty root.
func (ndb *nodeDB2) SaveEmptyRoot(version int64) error {
	return ndb.saveRoot([]byte{}, version)
}

func (ndb *nodeDB2) saveRoot(hash []byte, version int64) error {
	ndb.batchMtx.Lock()
	defer ndb.batchMtx.Unlock()

	if version != ndb.getLatestVersion()+1 {
		return fmt.Errorf("Must save consecutive versions. Expected %d, got %d", ndb.getLatestVersion()+1, version)
	}

	key := ndb.rootKey(version)
	ndb.batch.Set(key, hash)
	ndb.updateLatestVersion(version)

	return nil
}

////////////////// Utility and test functions /////////////////////////////////

func (ndb *nodeDB2) leafNodes() []*Node {
	leaves := []*Node{}

	ndb.traverseNodes(func(hash []byte, node *Node) {
		if node.isLeaf() {
			leaves = append(leaves, node)
		}
	})
	return leaves
}

func (ndb *nodeDB2) nodes() []*Node {
	nodes := []*Node{}

	ndb.traverseNodes(func(hash []byte, node *Node) {
		nodes = append(nodes, node)
	})
	return nodes
}

func (ndb *nodeDB2) orphans() [][]byte {
	orphans := [][]byte{}

	ndb.traverseOrphans(func(k, v []byte) {
		orphans = append(orphans, v)
	})
	return orphans
}

func (ndb *nodeDB2) roots() map[int64][]byte {
	roots, _ := ndb.getRoots()
	return roots
}

// Not efficient.
// NOTE: DB cannot implement Size() because
// mutations are not always synchronous.
func (ndb *nodeDB2) size() int {
	size := 0
	ndb.traverse(func(k, v []byte) {
		size++
	})
	return size
}

func (ndb *nodeDB2) traverseNodes(fn func(hash []byte, node *Node)) {
	nodes := []*Node{}

	ndb.traversePrefix(nodeKeyFormat.Key(), func(key, value []byte) {
		node, err := MakeNode(value, ndb.getLeafValueCb)
		if err != nil {
			panic(fmt.Sprintf("Couldn't decode node from database: %v", err))
		}
		nodeKeyFormat.Scan(key, &node.hash)
		nodes = append(nodes, node)
	})

	sort.Slice(nodes, func(i, j int) bool {
		return bytes.Compare(nodes[i].key, nodes[j].key) < 0
	})

	for _, n := range nodes {
		fn(n.hash, n)
	}
}

func (ndb *nodeDB2) String() string {
	var str string
	index := 0

	ndb.traversePrefix(rootKeyFormat.Key(), func(key, value []byte) {
		str += fmt.Sprintf("%s: %x\n", string(key), value)
	})
	str += "\n"

	ndb.traverseOrphans(func(key, value []byte) {
		str += fmt.Sprintf("%s: %x\n", string(key), value)
	})
	str += "\n"

	ndb.traverseNodes(func(hash []byte, node *Node) {
		if len(hash) == 0 {
			str += fmt.Sprintf("<nil>\n")
		} else if node == nil {
			str += fmt.Sprintf("%s%40x: <nil>\n", nodeKeyFormat.Prefix(), hash)
		} else if node.value == nil && node.height > 0 {
			str += fmt.Sprintf("%s%40x: %s   %-16s h=%d version=%d\n",
				nodeKeyFormat.Prefix(), hash, node.key, "", node.height, node.version)
		} else {
			str += fmt.Sprintf("%s%40x: %s = %-16s h=%d version=%d\n",
				nodeKeyFormat.Prefix(), hash, node.key, node.value, node.height, node.version)
		}
		index++
	})
	return "-" + "\n" + str + "-"
}

func (ndb *nodeDB2) MaxChacheSizeExceeded() bool {
	return true
}
