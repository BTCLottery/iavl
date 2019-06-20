package iavl

import (
	"bytes"
	"container/list"
	"fmt"
	"sort"
	"sync"

	"github.com/tendermint/tendermint/crypto/tmhash"
	dbm "github.com/tendermint/tendermint/libs/db"
)

const (
	int64Size = 8
	hashSize  = tmhash.Size
)

var (
	// All node keys are prefixed with the byte 'n'. This ensures no collision is
	// possible with the other keys, and makes them easier to traverse. They are indexed by the node hash.
	nodeKeyFormat = NewKeyFormat('n', hashSize) // n<hash>

	// Orphans are keyed in the database by their expected lifetime.
	// The first number represents the *last* version at which the orphan needs
	// to exist, while the second number represents the *earliest* version at
	// which it is expected to exist - which starts out by being the version
	// of the node being orphaned.
	orphanKeyFormat = NewKeyFormat('o', int64Size, int64Size, hashSize) // o<last-version><first-version><hash>

	// Root nodes are indexed separately by their version
	rootKeyFormat = NewKeyFormat('r', int64Size) // r<version>
)

// NodeDB is used by MutableTree & ImmutableTree to persist & load nodes to a DB
type NodeDB interface {
	GetNode(hash []byte) *Node
	SaveNode(node *Node, flushToDisk bool)
	Has(hash []byte) bool
	SaveBranch(node *Node, flushToDisk bool) []byte
	DeleteVersion(version int64, checkLatestVersion bool)
	DeleteMemoryVersion(version, previous int64, unsavedOrphans *map[string]int64)
	SaveOrphans(version int64, orphans map[string]int64)
	SaveRoot(root *Node, version int64) error
	SaveEmptyRoot(version int64) error
	Commit()
	String() string
	MaxChacheSizeExceeded() bool
	ResetMemNodes()
	ResetBatch()
	RestMemBatch()

	getRoot(version int64) []byte
	getRoots() (map[int64][]byte, error)
	getLatestVersion() int64
	resetLatestVersion(version int64)

	// Only for tests
	roots() map[int64][]byte
	leafNodes() []*Node
	nodes() []*Node
	orphans() [][]byte
	size() int
	traverseOrphans(fn func(k, v []byte))
}

type nodeDB struct {
	mtx      sync.Mutex // Read/write lock.
	db       dbm.DB     // Persistent node storage.
	dbMem    dbm.DB     // Memory node storage.
	batch    dbm.Batch  // Batched writing buffer.
	memNodes map[string]*Node

	latestVersion            int64
	nodeCache                map[string]*list.Element // Node cache.
	nodeCacheSize            int                      // Node cache size limit in elements.
	nodeMaxCacheSize         uint64                   // Node maximum cache size. Save to disk and reduce cache if exceeded.
	nodeCacheOnlyFlushOnSave bool                     // Only check the cache when saving.
	nodeCacheQueue           *list.List               // LRU queue of cache elements. Used for deletion.
	getLeafValueCb           func(key []byte) []byte  // Optional callback to get values stored in leaf nodes.
}

var _ NodeDB = (*nodeDB)(nil)

func NewNodeDB(db dbm.DB, cacheSize int, getLeafValueCb func(key []byte) []byte) NodeDB {
	ndb := &nodeDB{
		db:             db,
		dbMem:          dbm.NewMemDB(),
		memNodes:       map[string]*Node{},
		batch:          db.NewBatch(),
		latestVersion:  0, // initially invalid
		nodeCache:      make(map[string]*list.Element),
		nodeCacheSize:  cacheSize,
		nodeCacheQueue: list.New(),
		getLeafValueCb: getLeafValueCb,
	}
	return ndb
}

func NewNodeDB3(db dbm.DB, minCacheSize, maxCacheSize uint64, nodeCacheOnlyFlushOnSave bool, getLeafValueCb func(key []byte) []byte) NodeDB {
	ndb := &nodeDB{
		db:                       db,
		batch:                    db.NewBatch(),
		latestVersion:            0, // initially invalid
		nodeCache:                make(map[string]*list.Element),
		nodeCacheSize:            int(minCacheSize),
		nodeMaxCacheSize:         maxCacheSize,
		nodeCacheQueue:           list.New(),
		getLeafValueCb:           getLeafValueCb,
		nodeCacheOnlyFlushOnSave: nodeCacheOnlyFlushOnSave,
	}
	return ndb
}

// GetNode gets a node from cache or disk. If it is an inner node, it does not
// load its children.
func (ndb *nodeDB) GetNode(hash []byte) *Node {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()

	if len(hash) == 0 {
		panic("nodeDB.GetNode() requires hash")
	}

	// Check the cache.
	if elem, ok := ndb.nodeCache[string(hash)]; ok {
		// Already exists. Move to back of nodeCacheQueue.
		ndb.nodeCacheQueue.MoveToBack(elem)
		return elem.Value.(*Node)
	}
	//Try reading from memory
	var err error
	node := ndb.memNodes[string(hash)]
	if node == nil {
		// Doesn't exist, load from disk
		buf := ndb.db.Get(ndb.nodeKey(hash))
		if buf == nil {
			panic(fmt.Sprintf("Value missing for hash %x corresponding to nodeKey %s", hash, ndb.nodeKey(hash)))
		}

		node, err = MakeNode(buf, ndb.getLeafValueCb) // MakeNode(buf)
		if err != nil {
			panic(fmt.Sprintf("Error reading Node. bytes: %x, error: %v", buf, err))
		}
		node.persisted = true
	}

	node.hash = hash
	ndb.cacheNode(node)

	return node
}

// SaveNode saves a node to disk.
func (ndb *nodeDB) SaveNode(node *Node, flushToDisk bool) {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()

	if node.hash == nil {
		panic("Expected to find node.hash, but none found.")
	}
	if node.persistedMem == true && flushToDisk == false {
		return
	}
	if node.persisted {
		panic("Shouldn't be calling save on an already persisted node.")
	}

	// Save node bytes to db.
	buf := new(bytes.Buffer)
	if err := node.writeBytes(buf, ndb.getLeafValueCb == nil); err != nil {
		panic(err)
	}

	if flushToDisk == true {
		ndb.batch.Set(ndb.nodeKey(node.hash), buf.Bytes())
		node.persisted = true
	} else {
		node.persistedMem = true
		ndb.memNodes[string(node.hash)] = node
	}
	ndb.cacheNode(node)
}

func (ndb *nodeDB) ResetMemNodes() {
	ndb.dbMem = dbm.NewMemDB()
	ndb.memNodes = map[string]*Node{}
}

func (ndb *nodeDB) ResetBatch() {
	ndb.batch = ndb.db.NewBatch()
}
func (ndb *nodeDB) RestMemBatch() {
	ndb.batch = ndb.dbMem.NewBatch()
}

// Has checks if a hash exists in the database.
func (ndb *nodeDB) Has(hash []byte) bool {
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

// SaveBranch saves the given node and all of its descendants.
// NOTE: This function clears leftNode/rigthNode recursively and
// calls _hash() on the given node.
// TODO refactor, maybe use hashWithCount() but provide a callback.
func (ndb *nodeDB) SaveBranch(node *Node, flushToDisk bool) []byte {
	if node.persisted {
		return node.hash
	}
	if node.persistedMem && flushToDisk == false {
		return node.hash
	}
	if node.leftNode != nil {
		node.leftHash = ndb.SaveBranch(node.leftNode, flushToDisk)
	}
	if node.rightNode != nil {
		node.rightHash = ndb.SaveBranch(node.rightNode, flushToDisk)
	}

	node._hash()
	ndb.SaveNode(node, flushToDisk)

	if flushToDisk == true {
		node.leftNode = nil
		node.rightNode = nil
	}

	return node.hash
}

// DeleteVersion deletes a tree version from disk.
func (ndb *nodeDB) DeleteVersion(version int64, checkLatestVersion bool) {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()

	ndb.deleteOrphans(version)
	ndb.deleteRoot(version, checkLatestVersion)
}

// DeleteVersion deletes a tree version from memory.
func (ndb *nodeDB) DeleteMemoryVersion(version, previous int64, unsavedOrphans *map[string]int64) {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()

	ndb.deleteOrphansWithPredecessor(version, previous, unsavedOrphans)
	ndb.deleteRoot(version, false)
}

// Saves orphaned nodes to disk under a special prefix.
// version: the new version being saved.
// orphans: the orphan nodes created since version-1
func (ndb *nodeDB) SaveOrphans(version int64, orphans map[string]int64) {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()

	//var toVersion int64
	//toVersion = ndb.getPreviousVersioni(version, ndb.dbMem)
	//if toVersion == 0 {
	//	toVersion = ndb.getPreviousVersion(version)
	//} //see if we have something on disk if we dont have anything from mem

	// When saving a new version, the previous version will always be one less than the new version
	toVersion := version - 1
	for hash, fromVersion := range orphans {
		debug("SAVEORPHAN %v-%v %X\n", fromVersion, toVersion, hash)
		ndb.saveOrphan([]byte(hash), fromVersion, toVersion)
	}
}

// Saves a single orphan to disk.
func (ndb *nodeDB) saveOrphan(hash []byte, fromVersion, toVersion int64) {
	if fromVersion > toVersion {
		panic(fmt.Sprintf("Orphan expires before it comes alive.  %d > %d", fromVersion, toVersion))
	}
	key := ndb.orphanKey(fromVersion, toVersion, hash)
	ndb.batch.Set(key, hash)
}

// deleteOrphans deletes orphaned nodes from disk, and the associated orphan
// entries.
func (ndb *nodeDB) deleteOrphans(version int64) {
	// Will be zero if there is no previous version.
	predecessor := ndb.getPreviousVersion(version)

	ndb.deleteOrphansWithPredecessor(version, predecessor, nil)
}

func (ndb *nodeDB) deleteOrphansWithPredecessor(version, predecessor int64, unsavedOrphans *map[string]int64) {
	// Traverse orphans with a lifetime ending at the version specified.
	// TODO optimize.
	ndb.traverseOrphansVersion(version, func(key, hash []byte) {
		var fromVersion, toVersion int64

		// See comment on `orphanKeyFmt`. Note that here, `version` and
		// `toVersion` are always equal.
		orphanKeyFormat.Scan(key, &toVersion, &fromVersion)

		// Delete orphan key and reverse-lookup key.
		ndb.batch.Delete(key)

		// If there is no predecessor,
		// or the predecessor is earlier than the  beginning of the lifetime (ie: negative lifetime),
		// or the lifetime spans a single version and that version is the one being deleted,
		// we can delete the orphan.
		// Otherwise, we shorten its lifetime, by moving its endpoint to the previous version.
		if predecessor < fromVersion || fromVersion == toVersion {
			debug("DELETE predecessor:%v fromVersion:%v toVersion:%v %X\n", predecessor, fromVersion, toVersion, hash)
			ndb.batch.Delete(ndb.nodeKey(hash))
			ndb.uncacheNode(hash)
		} else {
			debug("MOVE predecessor:%v fromVersion:%v toVersion:%v %X\n", predecessor, fromVersion, toVersion, hash)
			ndb.saveOrphan(hash, fromVersion, predecessor)
		}
	})
	if unsavedOrphans == nil {
		return
	}

	toVersion := version
	if toVersion > 1 {
		toVersion++
	}
	for hash, fromVersion := range *unsavedOrphans {
		key := orphanKeyFormat.Key(toVersion, fromVersion, []byte(hash))
		hasit := ndb.Has([]byte(hash))
		hasit = hasit

		ndb.batch.Delete(key)
		delete(*unsavedOrphans, hash)
		if predecessor < fromVersion || fromVersion == toVersion {
			debug("DELETE predecessor:%v fromVersion:%v toVersion:%v %X\n", predecessor, fromVersion, toVersion, hash)
			ndb.batch.Delete(ndb.nodeKey([]byte(hash)))
			ndb.uncacheNode([]byte(hash))
		} else {
			debug("MOVE predecessor:%v fromVersion:%v toVersion:%v %X\n", predecessor, fromVersion, toVersion, hash)
			ndb.saveOrphan([]byte(hash), fromVersion, predecessor)
			(*unsavedOrphans)[hash] = predecessor
		}
	}
}

func (ndb *nodeDB) nodeKey(hash []byte) []byte {
	return nodeKeyFormat.KeyBytes(hash)
}

func (ndb *nodeDB) orphanKey(fromVersion, toVersion int64, hash []byte) []byte {
	return orphanKeyFormat.Key(toVersion, fromVersion, hash)
}

func (ndb *nodeDB) rootKey(version int64) []byte {
	return rootKeyFormat.Key(version)
}

func (ndb *nodeDB) getLatestVersion() int64 {
	if ndb.latestVersion == 0 {
		ndb.latestVersion = ndb.getPreviousVersion(1<<63 - 1)
	}
	return ndb.latestVersion
}

func (ndb *nodeDB) updateLatestVersion(version int64) {
	if ndb.latestVersion < version {
		ndb.latestVersion = version
	}
}

func (ndb *nodeDB) resetLatestVersion(version int64) {
	ndb.latestVersion = version
}

func (ndb *nodeDB) getPreviousVersion(version int64) int64 {
	return ndb.getPreviousVersioni(version, ndb.db)
}

func (ndb *nodeDB) getPreviousVersioni(version int64, db dbm.DB) int64 {
	itr := db.ReverseIterator(
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

// deleteRoot deletes the root entry from disk, but not the node it points to.
func (ndb *nodeDB) deleteRoot(version int64, checkLatestVersion bool) {
	if checkLatestVersion && version == ndb.getLatestVersion() {
		panic("Tried to delete latest version")
	}

	key := ndb.rootKey(version)
	ndb.batch.Delete(key)
}

func (ndb *nodeDB) traverseOrphans(fn func(k, v []byte)) {
	ndb.traversePrefix(orphanKeyFormat.Key(), fn)
}

// Traverse orphans ending at a certain version.
func (ndb *nodeDB) traverseOrphansVersion(version int64, fn func(k, v []byte)) {
	ndb.traversePrefix(orphanKeyFormat.Key(version), fn)
}

// Traverse all keys.
func (ndb *nodeDB) traverse(fn func(key, value []byte)) {
	itr := ndb.db.Iterator(nil, nil)
	defer itr.Close()

	for ; itr.Valid(); itr.Next() {
		fn(itr.Key(), itr.Value())
	}
}

// Traverse all keys with a certain prefix.
func (ndb *nodeDB) traversePrefix(prefix []byte, fn func(k, v []byte)) {
	itr := dbm.IteratePrefix(ndb.db, prefix)
	defer itr.Close()

	for ; itr.Valid(); itr.Next() {
		fn(itr.Key(), itr.Value())
	}
}

func (ndb *nodeDB) uncacheNode(hash []byte) {
	if elem, ok := ndb.nodeCache[string(hash)]; ok {
		ndb.nodeCacheQueue.Remove(elem)
		delete(ndb.nodeCache, string(hash))
	}
}

func (ndb *nodeDB) MaxChacheSizeExceeded() bool {
	return ndb.nodeMaxCacheSize > 0 && uint64(ndb.nodeCacheQueue.Len()) > ndb.nodeMaxCacheSize
}

// Add a node to the cache and pop the least recently used node if we've
// reached the cache size limit.
func (ndb *nodeDB) cacheNode(node *Node) {
	elem := ndb.nodeCacheQueue.PushBack(node)
	ndb.nodeCache[string(node.hash)] = elem

	if !ndb.nodeCacheOnlyFlushOnSave && ndb.nodeCacheQueue.Len() > ndb.nodeCacheSize {
		oldest := ndb.nodeCacheQueue.Front()
		hash := ndb.nodeCacheQueue.Remove(oldest).(*Node).hash
		delete(ndb.nodeCache, string(hash))
	}
}

func (ndb *nodeDB) FlushCache() {
	for ndb.nodeCacheQueue.Len() > ndb.nodeCacheSize {
		oldest := ndb.nodeCacheQueue.Front()
		hash := ndb.nodeCacheQueue.Remove(oldest).(*Node).hash
		delete(ndb.nodeCache, string(hash))
	}
}

// Write to disk.
func (ndb *nodeDB) Commit() {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()

	ndb.batch.Write()
	ndb.batch = ndb.db.NewBatch()
	ndb.FlushCache()
}

func (ndb *nodeDB) getRoot(version int64) []byte {
	memroot := ndb.dbMem.Get(ndb.rootKey(version))
	if len(memroot) > 0 {
		return memroot
	}

	return ndb.db.Get(ndb.rootKey(version))
}

func (ndb *nodeDB) getRoots() (map[int64][]byte, error) {
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
func (ndb *nodeDB) SaveRoot(root *Node, version int64) error {
	if len(root.hash) == 0 {
		panic("Hash should not be empty")
	}
	return ndb.saveRoot(root.hash, version)
}

// SaveEmptyRoot creates an entry on disk for an empty root.
func (ndb *nodeDB) SaveEmptyRoot(version int64) error {
	return ndb.saveRoot([]byte{}, version)
}

func (ndb *nodeDB) saveRoot(hash []byte, version int64) error {
	ndb.mtx.Lock()
	defer ndb.mtx.Unlock()

	//TODO NEED TO BE ABLE TO SET THIS TO MEMORY ALSO
	if version != ndb.getLatestVersion()+1 {
		return fmt.Errorf("Must save consecutive versions. Expected %d, got %d", ndb.getLatestVersion()+1, version)
	}

	key := ndb.rootKey(version)
	ndb.batch.Set(key, hash)
	ndb.updateLatestVersion(version)

	return nil
}

////////////////// Utility and test functions /////////////////////////////////

func (ndb *nodeDB) leafNodes() []*Node {
	leaves := []*Node{}

	ndb.traverseNodes(func(hash []byte, node *Node) {
		if node.isLeaf() {
			leaves = append(leaves, node)
		}
	})
	return leaves
}

func (ndb *nodeDB) nodes() []*Node {
	nodes := []*Node{}

	ndb.traverseNodes(func(hash []byte, node *Node) {
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

func (ndb *nodeDB) roots() map[int64][]byte {
	roots, _ := ndb.getRoots()
	return roots
}

// Not efficient.
// NOTE: DB cannot implement Size() because
// mutations are not always synchronous.
func (ndb *nodeDB) size() int {
	size := 0
	ndb.traverse(func(k, v []byte) {
		size++
	})
	return size
}

func (ndb *nodeDB) traverseNodes(fn func(hash []byte, node *Node)) {
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

func (ndb *nodeDB) String() string {
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
