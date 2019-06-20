package iavl

import (
	"bytes"
	"fmt"

	cmn "github.com/tendermint/tendermint/libs/common"
	dbm "github.com/tendermint/tendermint/libs/db"
)

// ErrVersionDoesNotExist is returned if a requested version does not exist.
var ErrVersionDoesNotExist = fmt.Errorf("version does not exist")

// MutableTree is a persistent tree which keeps track of versions.
type MutableTree struct {
	*ImmutableTree                  // The current, working tree.
	lastSaved      *ImmutableTree   // The most recently saved tree.
	orphans        map[string]int64 // Nodes removed by changes to working tree.
	memversions    map[int64]bool   // The previous, saved versions of the tree in mem.
	versions       map[int64]bool   // The previous, saved versions of the tree.
	ndb            NodeDB
}

// NewMutableTree returns a new tree with the specified cache size and datastore.
func NewMutableTree(db dbm.DB, cacheSize int) *MutableTree {
	ndb := NewNodeDB(db, cacheSize, nil)
	return NewMutableTreeWithNodeDB(ndb)
}

// NewMutableTreeWithNodeDB returns a new tree with the specified NodeDB.
func NewMutableTreeWithNodeDB(ndb NodeDB) *MutableTree {
	head := &ImmutableTree{ndb: ndb}

	return &MutableTree{
		ImmutableTree: head,
		lastSaved:     head.clone(),
		orphans:       map[string]int64{},
		memversions:   map[int64]bool{},
		versions:      map[int64]bool{},
		ndb:           ndb,
	}
}

// IsEmpty returns whether or not the tree has any keys. Only trees that are
// not empty can be saved.
func (tree *MutableTree) IsEmpty() bool {
	return tree.ImmutableTree.Size() == 0
}

// VersionExists returns whether or not a version exists.
func (tree *MutableTree) VersionExists(version int64) bool {
	return tree.versions[version]
}

// Hash returns the hash of the latest saved version of the tree, as returned
// by SaveVersion. If no versions have been saved, Hash returns nil.
func (tree *MutableTree) Hash() []byte {
	if tree.version > 0 {
		return tree.lastSaved.Hash()
	}
	return nil
}

// WorkingHash returns the hash of the current working tree.
func (tree *MutableTree) WorkingHash() []byte {
	return tree.ImmutableTree.Hash()
}

// String returns a string representation of the tree.
func (tree *MutableTree) String() string {
	return tree.ndb.String()
}

// Set sets a key in the working tree. Nil values are not supported.
func (tree *MutableTree) Set(key, value []byte) bool {
	orphaned, updated := tree.set(key, value)
	tree.addOrphans(orphaned)
	return updated
}

func (tree *MutableTree) set(key []byte, value []byte) (orphaned []*Node, updated bool) {
	if value == nil {
		panic(fmt.Sprintf("Attempt to store nil value at key '%s'", key))
	}
	if tree.ImmutableTree.root == nil {
		tree.ImmutableTree.root = NewNode(key, value, tree.version+1)
		return nil, false
	}
	tree.ImmutableTree.root, updated, orphaned = tree.recursiveSet(tree.ImmutableTree.root, key, value)

	return orphaned, updated
}

func (tree *MutableTree) recursiveSet(node *Node, key []byte, value []byte) (
	newSelf *Node, updated bool, orphaned []*Node,
) {
	version := tree.version + 1

	if node.isLeaf() {
		switch bytes.Compare(key, node.key) {
		case -1:
			return &Node{
				key:       node.key,
				height:    1,
				size:      2,
				leftNode:  NewNode(key, value, version),
				rightNode: node,
				version:   version,
			}, false, []*Node{}
		case 1:
			return &Node{
				key:       key,
				height:    1,
				size:      2,
				leftNode:  node,
				rightNode: NewNode(key, value, version),
				version:   version,
			}, false, []*Node{}
		default:
			return NewNode(key, value, version), true, []*Node{node}
		}
	} else {
		orphaned = append(orphaned, node)
		node = node.clone(version)

		if bytes.Compare(key, node.key) < 0 {
			var leftOrphaned []*Node
			node.leftNode, updated, leftOrphaned = tree.recursiveSet(node.getLeftNode(tree.ImmutableTree), key, value)
			node.leftHash = nil // leftHash is yet unknown
			orphaned = append(orphaned, leftOrphaned...)
		} else {
			var rightOrphaned []*Node
			node.rightNode, updated, rightOrphaned = tree.recursiveSet(node.getRightNode(tree.ImmutableTree), key, value)
			node.rightHash = nil // rightHash is yet unknown
			orphaned = append(orphaned, rightOrphaned...)
		}

		if updated {
			return node, updated, orphaned
		}
		node.calcHeightAndSize(tree.ImmutableTree)
		newNode, balanceOrphaned := tree.balance(node)
		return newNode, updated, append(orphaned, balanceOrphaned...)
	}
}

// Remove removes a key from the working tree.
func (tree *MutableTree) Remove(key []byte) ([]byte, bool) {
	val, orphaned, removed := tree.remove(key)
	tree.addOrphans(orphaned)
	return val, removed
}

// remove tries to remove a key from the tree and if removed, returns its
// value, nodes orphaned and 'true'.
func (tree *MutableTree) remove(key []byte) (value []byte, orphans []*Node, removed bool) {
	if tree.root == nil {
		return nil, nil, false
	}
	newRootHash, newRoot, _, value, orphaned := tree.recursiveRemove(tree.root, key)
	if len(orphaned) == 0 {
		return nil, nil, false
	}

	if newRoot == nil && newRootHash != nil {
		tree.root = tree.ndb.GetNode(newRootHash)
	} else {
		tree.root = newRoot
	}
	return value, orphaned, true
}

// removes the node corresponding to the passed key and balances the tree.
// It returns:
// - the hash of the new node (or nil if the node is the one removed)
// - the node that replaces the orig. node after remove
// - new leftmost leaf key for tree after successfully removing 'key' if changed.
// - the removed value
// - the orphaned nodes.
func (tree *MutableTree) recursiveRemove(node *Node, key []byte) ([]byte, *Node, []byte, []byte, []*Node) {
	version := tree.version + 1

	if node.isLeaf() {
		if bytes.Equal(key, node.key) {
			return nil, nil, nil, node.value, []*Node{node}
		}
		return node.hash, node, nil, nil, nil
	}

	// node.key < key; we go to the left to find the key:
	if bytes.Compare(key, node.key) < 0 {
		newLeftHash, newLeftNode, newKey, value, orphaned := tree.recursiveRemove(node.getLeftNode(tree.ImmutableTree), key)

		if len(orphaned) == 0 {
			return node.hash, node, nil, value, orphaned
		} else if newLeftHash == nil && newLeftNode == nil { // left node held value, was removed
			return node.rightHash, node.rightNode, node.key, value, orphaned
		}
		orphaned = append(orphaned, node)

		newNode := node.clone(version)
		newNode.leftHash, newNode.leftNode = newLeftHash, newLeftNode
		newNode.calcHeightAndSize(tree.ImmutableTree)
		newNode, balanceOrphaned := tree.balance(newNode)

		return newNode.hash, newNode, newKey, value, append(orphaned, balanceOrphaned...)
	}
	// node.key >= key; either found or look to the right:
	newRightHash, newRightNode, newKey, value, orphaned := tree.recursiveRemove(node.getRightNode(tree.ImmutableTree), key)

	if len(orphaned) == 0 {
		return node.hash, node, nil, value, orphaned
	} else if newRightHash == nil && newRightNode == nil { // right node held value, was removed
		return node.leftHash, node.leftNode, nil, value, orphaned
	}
	orphaned = append(orphaned, node)

	newNode := node.clone(version)
	newNode.rightHash, newNode.rightNode = newRightHash, newRightNode
	if newKey != nil {
		newNode.key = newKey
	}
	newNode.calcHeightAndSize(tree.ImmutableTree)
	newNode, balanceOrphaned := tree.balance(newNode)

	return newNode.hash, newNode, nil, value, append(orphaned, balanceOrphaned...)
}

// Load the latest versioned tree from disk.
func (tree *MutableTree) Load() (int64, error) {
	return tree.LoadVersion(int64(0))
}

// Returns the version number of the latest version found
func (tree *MutableTree) LoadVersion(targetVersion int64) (int64, error) {
	roots, err := tree.ndb.getRoots()
	if err != nil {
		return 0, err
	}
	if len(roots) == 0 {
		return 0, nil
	}
	latestVersion := int64(0)
	var latestRoot []byte
	for version, r := range roots {
		tree.versions[version] = true
		if version > latestVersion &&
			(targetVersion == 0 || version <= targetVersion) {
			latestVersion = version
			latestRoot = r
		}
	}

	if !(targetVersion == 0 || latestVersion == targetVersion) {
		return latestVersion, fmt.Errorf("wanted to load target %v but only found up to %v",
			targetVersion, latestVersion)
	}

	t := &ImmutableTree{
		ndb:     tree.ndb,
		version: latestVersion,
	}
	if len(latestRoot) != 0 {
		t.root = tree.ndb.GetNode(latestRoot)
	}

	tree.orphans = map[string]int64{}
	tree.ImmutableTree = t
	tree.lastSaved = t.clone()
	return latestVersion, nil
}

// LoadVersionOverwrite returns the version number of targetVersion.
// Higher versions' data will be deleted.
func (tree *MutableTree) LoadVersionForOverwriting(targetVersion int64) (int64, error) {
	latestVersion, err := tree.LoadVersion(targetVersion)
	if err != nil {
		return latestVersion, err
	}
	err = tree.deleteVersionsFrom(targetVersion + 1)
	if err != nil {
		return latestVersion, err
	}
	return targetVersion, nil
}

// GetImmutable loads an ImmutableTree at a given version for querying
func (tree *MutableTree) GetImmutable(version int64) (*ImmutableTree, error) {
	rootHash := tree.ndb.getRoot(version)
	if rootHash == nil {
		return nil, ErrVersionDoesNotExist
	} else if len(rootHash) == 0 {
		return &ImmutableTree{
			ndb:     tree.ndb,
			version: version,
		}, nil
	}
	return &ImmutableTree{
		root:    tree.ndb.GetNode(rootHash),
		ndb:     tree.ndb,
		version: version,
	}, nil
}

// Rollback resets the working tree to the latest saved version, discarding
// any unsaved modifications.
func (tree *MutableTree) Rollback() {
	if tree.version > 0 {
		tree.ImmutableTree = tree.lastSaved.clone()
	} else {
		tree.ImmutableTree = &ImmutableTree{ndb: tree.ndb, version: 0}
	}
	tree.orphans = map[string]int64{}
}

// GetVersioned gets the value at the specified key and version.
func (tree *MutableTree) GetVersioned(key []byte, version int64) (
	index int64, value []byte,
) {
	if tree.versions[version] {
		t, err := tree.GetImmutable(version)
		if err != nil {
			return -1, nil
		}
		return t.Get(key)
	}
	return -1, nil
}

//var nextTimeClear = false

// SaveVersionMem saves a new tree version to disk, based on the current state of
// the tree. Returns the hash and new version number.
func (tree *MutableTree) SaveVersionMem() ([]byte, int64, error) {
	return tree.saveVersion(false)
}

// FlushMemDisk saves a new tree to disk and removes all the versions in memory
// TODO keep some in an LRU Cache
func (tree *MutableTree) FlushMemVersionDisk() ([]byte, int64, error) {
	//if version%MutateVersionInterval == 0 {
	x, y, err := tree.saveVersion(true)
	//nextTimeClear = true
	tree.memversions = map[int64]bool{}
	tree.ndb.ResetMemNodes()
	fmt.Printf("CLEARED MEMORY-----height-%d-------\n", tree.version+1)
	return x, y, err
	//}
}

// SaveVersion saves a new tree version to disk, based on the current state of
// the tree. Returns the hash and new version number.
func (tree *MutableTree) SaveVersion() ([]byte, int64, error) {
	return tree.saveVersion(true)
}

// SaveVersion saves a new tree version to disk, based on the current state of
// the tree. Returns the hash and new version number.
func (tree *MutableTree) saveVersion(flushToDisk bool) ([]byte, int64, error) {
	hash, version, err := tree.saveTree(flushToDisk)
	if err != nil {
		return hash, version, err
	}
	tree.ndb.Commit()
	tree.orphans = map[string]int64{}
	return tree.setVersion(version)
}

// Creates a new version and its hash without saving to disk
func (tree *MutableTree) NewVersion() ([]byte, int64, error) {
	hash, version, err := tree.saveTree(true)
	if err != nil {
		return hash, version, err
	}
	return tree.setVersion(version)
}

func (tree *MutableTree) saveTree(flushToDisk bool) ([]byte, int64, error) {
	version := tree.version + 1

	if flushToDisk == true {
		tree.ndb.ResetBatch()
	} else {
		tree.ndb.RestMemBatch()
		tree.memversions[version] = true
	}

	if tree.versions[version] {
		//version already exists, throw an error if attempting to overwrite
		// Same hash means idempotent.  Return success.
		existingHash := tree.ndb.getRoot(version)
		var newHash = tree.WorkingHash()
		if bytes.Equal(existingHash, newHash) {
			tree.version = version
			tree.ImmutableTree = tree.ImmutableTree.clone()
			tree.lastSaved = tree.ImmutableTree.clone()
			tree.orphans = map[string]int64{}
			return existingHash, version, nil
		}
		return nil, version, fmt.Errorf("version %d was already saved to different hash %X (existing hash %X)",
			version, newHash, existingHash)
	}
	if tree.root == nil {
		// There can still be orphans, for example if the root is the node being
		// removed.
		debug("SAVE EMPTY TREE %v\n", version)
		tree.ndb.SaveOrphans(version, tree.orphans)
		tree.ndb.SaveEmptyRoot(version)
	} else {
		debug("SAVE TREE %v\n", version)
		// Save the current tree.
		tree.ndb.SaveBranch(tree.root, flushToDisk)
		tree.ndb.SaveOrphans(version, tree.orphans)
		tree.ndb.SaveRoot(tree.root, version)
	}

	return nil, version, nil
}

func (tree *MutableTree) setVersion(version int64) ([]byte, int64, error) {
	tree.version = version
	tree.versions[version] = true

	// Set new working tree.
	tree.ImmutableTree = tree.ImmutableTree.clone()
	tree.lastSaved = tree.ImmutableTree.clone()

	return tree.Hash(), version, nil
}

// DeleteVersion deletes a tree version from disk. The version can then no
// longer be accessed.
func (tree *MutableTree) DeleteVersion(version int64) error {
	return tree.DeleteVersionFull(version, true)
}

// DeleteVersion deletes a tree version from disk. The version can then no
// longer be accessed.
func (tree *MutableTree) DeleteVersionFull(version int64, memDeleteAlso bool) error {
	if tree.memversions[version] == true {
		//sometimes you dont want to bother deleting versions in memory
		if memDeleteAlso == false {
			return nil
		}
		tree.ndb.RestMemBatch()
		delete(tree.memversions, version)
	} else {
		tree.ndb.ResetBatch()
	}

	if version == 0 {
		return cmn.NewError("version must be greater than 0")
	}
	if version == tree.version {
		return cmn.NewError("cannot delete latest saved version (%d)", version)
	}
	if _, ok := tree.versions[version]; !ok {
		return cmn.ErrorWrap(ErrVersionDoesNotExist, "")
	}

	tree.ndb.DeleteVersion(version, true)
	tree.ndb.Commit()

	delete(tree.versions, version)

	return nil
}

func (tree *MutableTree) DeleteMemoryVersion(version int64, noPreviousVersion bool) error {
	if version == 0 {
		return cmn.NewError("version must be greater than 0")
	}
	if version == tree.version {
		return cmn.NewError("cannot delete latest saved version (%d)", version)
	}
	if _, ok := tree.versions[version]; !ok {
		return cmn.ErrorWrap(ErrVersionDoesNotExist, "")
	}
	previous := int64(0)
	if !noPreviousVersion {
		previous = getPreviousVersion(tree.versions, version)
	}
	tree.ndb.DeleteMemoryVersion(version, previous, &tree.orphans)
	delete(tree.versions, version)

	return nil
}

func getPreviousVersion(versions map[int64]bool, version int64) int64 {
	for v := int64(version - 1); v > 0; v-- {
		found, ok := versions[v]
		if ok && found {
			return v
		}
	}
	return 0
}

// deleteVersionsFrom deletes tree version from disk specified version to latest version. The version can then no
// longer be accessed.
func (tree *MutableTree) deleteVersionsFrom(version int64) error {
	if version <= 0 {
		return cmn.NewError("version must be greater than 0")
	}
	newLatestVersion := version - 1
	lastestVersion := tree.ndb.getLatestVersion()
	for ; version <= lastestVersion; version++ {
		if version == tree.version {
			return cmn.NewError("cannot delete latest saved version (%d)", version)
		}
		if _, ok := tree.versions[version]; !ok {
			return cmn.ErrorWrap(ErrVersionDoesNotExist, "")
		}
		tree.ndb.DeleteVersion(version, false)
		delete(tree.versions, version)
	}
	tree.ndb.Commit()
	tree.ndb.resetLatestVersion(newLatestVersion)
	return nil
}

// Rotate right and return the new node and orphan.
func (tree *MutableTree) rotateRight(node *Node) (*Node, *Node) {
	version := tree.version + 1

	// TODO: optimize balance & rotate.
	node = node.clone(version)
	orphaned := node.getLeftNode(tree.ImmutableTree)
	newNode := orphaned.clone(version)

	newNoderHash, newNoderCached := newNode.rightHash, newNode.rightNode
	newNode.rightHash, newNode.rightNode = node.hash, node
	node.leftHash, node.leftNode = newNoderHash, newNoderCached

	node.calcHeightAndSize(tree.ImmutableTree)
	newNode.calcHeightAndSize(tree.ImmutableTree)

	return newNode, orphaned
}

// Rotate left and return the new node and orphan.
func (tree *MutableTree) rotateLeft(node *Node) (*Node, *Node) {
	version := tree.version + 1

	// TODO: optimize balance & rotate.
	node = node.clone(version)
	orphaned := node.getRightNode(tree.ImmutableTree)
	newNode := orphaned.clone(version)

	newNodelHash, newNodelCached := newNode.leftHash, newNode.leftNode
	newNode.leftHash, newNode.leftNode = node.hash, node
	node.rightHash, node.rightNode = newNodelHash, newNodelCached

	node.calcHeightAndSize(tree.ImmutableTree)
	newNode.calcHeightAndSize(tree.ImmutableTree)

	return newNode, orphaned
}

// NOTE: assumes that node can be modified
// TODO: optimize balance & rotate
func (tree *MutableTree) balance(node *Node) (newSelf *Node, orphaned []*Node) {
	if node.persisted {
		panic("Unexpected balance() call on persisted node")
	}
	balance := node.calcBalance(tree.ImmutableTree)

	if balance > 1 {
		if node.getLeftNode(tree.ImmutableTree).calcBalance(tree.ImmutableTree) >= 0 {
			// Left Left Case
			newNode, orphaned := tree.rotateRight(node)
			return newNode, []*Node{orphaned}
		}
		// Left Right Case
		var leftOrphaned *Node

		left := node.getLeftNode(tree.ImmutableTree)
		node.leftHash = nil
		node.leftNode, leftOrphaned = tree.rotateLeft(left)
		newNode, rightOrphaned := tree.rotateRight(node)

		return newNode, []*Node{left, leftOrphaned, rightOrphaned}
	}
	if balance < -1 {
		if node.getRightNode(tree.ImmutableTree).calcBalance(tree.ImmutableTree) <= 0 {
			// Right Right Case
			newNode, orphaned := tree.rotateLeft(node)
			return newNode, []*Node{orphaned}
		}
		// Right Left Case
		var rightOrphaned *Node

		right := node.getRightNode(tree.ImmutableTree)
		node.rightHash = nil
		node.rightNode, rightOrphaned = tree.rotateRight(right)
		newNode, leftOrphaned := tree.rotateLeft(node)

		return newNode, []*Node{right, leftOrphaned, rightOrphaned}
	}
	// Nothing changed
	return node, []*Node{}
}

func (tree *MutableTree) addOrphans(orphans []*Node) {
	for _, node := range orphans {
		if !node.persisted {
			// We don't need to orphan nodes that were never persisted.
			continue
		}
		if len(node.hash) == 0 {
			panic("Expected to find node hash, but was empty")
		}
		tree.orphans[string(node.hash)] = node.version
	}
}

// IMPORTANT This function assumes there has been no transactions since the last SaveVersion. If unsure about
// this do a SaveVersion immediately before calling this function.
//
// SaveVersionToDB creates a copy of the current tree consisting of just one version
// version - The version we wish to clone. Use version == 0 to save the latest version.
// newDb   - Database to hold the cloned data.
// savesPerCommit - Number of saves between each commit, allows memory speed fine tuning.
//                      savesPerCommit = 0 no saves, maximum RAM use.
//                      savesPerCommit = 1 commit after each save.
// callback     - Callback function, allows for displaying debug information. Parameter is height of node being processed.
func (tree *MutableTree) SaveVersionToDB(
	version int64,
	ndb NodeDB,
	savesPerCommit uint64,
	callback func(height int8) bool,
) ([]byte, int64, error) {
	if version == 0 {
		version = tree.ndb.getLatestVersion()
	} else {
		if _, err := tree.LoadVersion(version); err != nil {
			return nil, 0, err
		}
	}

	// Build a new tree for our desired version
	immutableTree, err := tree.GetImmutable(version)
	if err != nil {
		return nil, 0, cmn.NewError("Getting imutable tree: %v", err)
	}

	// Set the version. Version number gets incremented on saving, so set to version before the one we want.
	ndb.resetLatestVersion(version - 1)
	if err := ndb.SaveRoot(tree.root, version); err != nil {
		return nil, 0, err
	}

	// Recursively save tree to the database
	savesSinceLastCommit := uint64(0)
	tree.root.LoadAndSave(immutableTree, ndb, savesPerCommit, &savesSinceLastCommit, callback)

	// Ensure all data in the tree has been committed to the database
	ndb.Commit()
	return tree.root.hash, version, nil
}
