package headerfs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/pearl-research-labs/pearl/node/blockchain"
	"github.com/pearl-research-labs/pearl/node/btcutil/gcs/builder"
	"github.com/pearl-research-labs/pearl/node/chaincfg"
	"github.com/pearl-research-labs/pearl/node/chaincfg/chainhash"
	"github.com/pearl-research-labs/pearl/node/wire"
	"github.com/pearl-research-labs/pearl/wallet/walletdb"
)

const (
	// BlockHeadersFileName is the name of the flat file that holds the
	// block headers.
	BlockHeadersFileName = "block_headers.bin"

	// RegFilterHeadersFileName is the name of the flat file that holds the
	// regular filter headers.
	RegFilterHeadersFileName = "reg_filter_headers.bin"
)

// BlockStamp represents a block, identified by its height and time stamp in
// the chain. We also lift the timestamp from the block header itself into this
// struct as well.
type BlockStamp struct {
	// Height is the height of the target block.
	Height int32

	// Hash is the hash that uniquely identifies this block.
	Hash chainhash.Hash

	// Timestamp is the timestamp of the block in the chain.
	Timestamp time.Time
}

// BlockHeaderStore is an interface that provides an abstraction for a generic
// store for block headers.
type BlockHeaderStore interface {
	// ChainTip returns the best known block header and height for the
	// BlockHeaderStore.
	ChainTip() (*wire.BlockHeader, uint32, error)

	// LatestBlockLocator returns the latest block locator object based on
	// the tip of the current main chain from the PoV of the
	// BlockHeaderStore.
	LatestBlockLocator() (blockchain.BlockLocator, error)

	// FetchHeaderByHeight attempts to retrieve a target block header based
	// on a block height.
	FetchHeaderByHeight(height uint32) (*wire.BlockHeader, error)

	// FetchHeaderAncestors fetches the numHeaders block headers that are
	// the ancestors of the target stop hash. A total of numHeaders+1
	// headers will be returned, as we'll walk back numHeaders distance to
	// collect each header, then return the final header specified by the
	// stop hash. We'll also return the starting height of the header range
	// as well so callers can compute the height of each header without
	// knowing the height of the stop hash.
	FetchHeaderAncestors(uint32, *chainhash.Hash) ([]wire.BlockHeader,
		uint32, error)

	// HeightFromHash returns the height of a particular block header given
	// its hash.
	HeightFromHash(*chainhash.Hash) (uint32, error)

	// FetchHeader attempts to retrieve a block header determined by the
	// passed block height.
	FetchHeader(*chainhash.Hash) (*wire.BlockHeader, uint32, error)

	// WriteHeaders adds a set of headers to the BlockHeaderStore in a
	// single atomic transaction.
	WriteHeaders(...BlockHeader) error

	// RollbackBlockHeaders rolls back a specified number of headers from
	// the tip of the chain. It removes the most recent 'numHeaders' from
	// the block headers file and updates the corresponding indices. This
	// method is used during blockchain reorganizations to remove headers
	// that are no longer part of the main chain. The function will return
	// an error if the rollback would reach or go before the genesis block
	// (height 0). The information about the new header tip after truncation
	// is returned.
	RollbackBlockHeaders(numHeaders uint32) (*BlockStamp, error)

	// RollbackLastBlock rolls back the BlockHeaderStore by a _single_
	// header. This method is meant to be used in the case of re-org which
	// disconnects the latest block header from the end of the main chain.
	// The information about the new header tip after truncation is
	// returned.
	//
	// NOTE: This function is maintained for backward compatibility since it
	// is a publicly exposed function. It now internally utilizes
	// RollbackBlockHeaders API.
	RollbackLastBlock() (*BlockStamp, error)
}

// headerBufPool is a pool of bytes.Buffer that will be re-used by the various
// headerStore implementations to batch their header writes to disk. By
// utilizing this variable we can minimize the total number of allocations when
// writing headers to disk.
var headerBufPool = sync.Pool{
	New: func() interface{} { return new(bytes.Buffer) },
}

// File defines the minimum file operations needed by headerStore.
type File interface {
	// Basic I/O operations.
	io.Reader
	io.Writer
	io.Closer

	// Extended I/O positioning.
	io.Seeker
	io.ReaderAt

	// File-specific operations.
	Stat() (os.FileInfo, error)
	Sync() error
	Truncate(size int64) error

	// Returns the name of the file.
	Name() string
}

type headerFile struct {
	file File
}

// truncateHeaders truncates one or more headers from the end of the header
// file. This can be used in the case of a re-org to remove headers from the end
// of the main chain.
//
// The numHeaders parameter specifies how many headers to truncate. If
// numHeaders is 1, this is equivalent to the old singleTruncate behavior.
func (h *headerFile) truncateHeaders(numHeaders uint32,
	headerType HeaderType) error {

	// If numHeaders is 0, treat it as a no-op and return no error.
	if numHeaders == 0 {
		return nil
	}

	// In order to truncate the file, we'll need to grab the absolute size
	// of the file as it stands currently.
	fileInfo, err := h.file.Stat()
	if err != nil {
		return err
	}
	fileSize := fileInfo.Size()

	// Calculate the total bytes to truncate based on number of headers.
	headerTypeSize, err := headerType.Size()
	if err != nil {
		return err
	}
	truncateLength := int64(numHeaders) * int64(headerTypeSize)

	// A negative size makes the truncate syscall fail (EINVAL, or error 87
	// on Windows), so never ask for more than the file holds.
	if truncateLength > fileSize {
		return fmt.Errorf("cannot truncate %d %s headers (%d bytes) "+
			"from a file of %d bytes", numHeaders, headerType,
			truncateLength, fileSize)
	}

	return h.truncateToSize(fileSize - truncateLength)
}

// truncateToSize truncates the file to newSize bytes.
//
// This function handles platform-specific differences in file truncation. On
// Windows, the file is closed, truncated, and reopened due to Windows
// limitations on truncating open files. On other platforms, the file is
// truncated directly without closing.
func (h *headerFile) truncateToSize(newSize int64) error {
	if newSize < 0 {
		return fmt.Errorf("invalid truncate size %d", newSize)
	}

	// On Windows, we need to close, truncate, and reopen the file.
	if runtime.GOOS == "windows" {
		fileName := h.file.Name()
		if err := h.file.Close(); err != nil {
			return err
		}

		if err := os.Truncate(fileName, newSize); err != nil {
			return err
		}

		fileFlags := os.O_RDWR | os.O_APPEND | os.O_CREATE
		file, err := os.OpenFile(fileName, fileFlags, 0644)
		if err != nil {
			return err
		}
		h.file = file

		return nil
	}

	return h.file.Truncate(newSize)
}

// Close closes the flat file. The index database is owned by the caller and
// stays open.
func (h *headerFile) Close() error {
	return h.file.Close()
}

// headerStore combines a on-disk set of headers within a flat file in addition
// to a database which indexes that flat file. Together, these two abstractions
// can be used in order to build an indexed header store for any type of
// "header" as it deals only with raw bytes, and leaves it to a higher layer to
// interpret those raw bytes accordingly.
//
// TODO(roasbeef): quickcheck coverage.
type headerStore struct {
	mtx sync.RWMutex // nolint:structcheck // false positive because used as embedded struct only

	*headerFile

	*headerIndex
}

// newHeaderStore creates a new headerStore given an already open database, a
// target file path for the flat-file and a particular header type. The target
// file will be created as necessary.
func newHeaderStore(db walletdb.DB, filePath string,
	hType HeaderType) (*headerStore, error) {

	var headerFile = &headerFile{}

	var flatFileName string
	switch hType {
	case Block:
		flatFileName = BlockHeadersFileName
	case RegularFilter:
		flatFileName = RegFilterHeadersFileName
	default:
		return nil, fmt.Errorf("unrecognized filter type: %v", hType)
	}

	flatFileName = filepath.Join(filePath, flatFileName)

	// We'll open the file, creating it if necessary and ensuring that all
	// writes are actually appends to the end of the file.
	fileFlags := os.O_RDWR | os.O_APPEND | os.O_CREATE
	file, err := os.OpenFile(flatFileName, fileFlags, 0644)
	if err != nil {
		return nil, err
	}
	headerFile.file = file

	// With the file open, we'll then create the header index so we can
	// have random access into the flat files.
	index, err := newHeaderIndex(db, hType)
	if err != nil {
		_ = file.Close()
		return nil, err
	}

	return &headerStore{
		headerFile:  headerFile,
		headerIndex: index,
	}, nil
}

// trimPartialRecord cuts a trailing partial header off the file. A torn append
// leaves one behind, and it would misalign every header appended after it. The
// number of complete headers left in the file is returned.
func (h *headerStore) trimPartialRecord() (uint64, error) {
	headerSize, err := h.indexType.Size()
	if err != nil {
		return 0, err
	}

	fileInfo, err := h.file.Stat()
	if err != nil {
		return 0, err
	}

	fileSize := fileInfo.Size()
	if partial := fileSize % int64(headerSize); partial != 0 {
		log.Warnf("%v file %v ends in a partial header (%d of %d "+
			"bytes), truncating it", h.indexType, h.file.Name(),
			partial, headerSize)

		fileSize -= partial
		if err := h.truncateToSize(fileSize); err != nil {
			return 0, err
		}
	}

	return uint64(fileSize) / uint64(headerSize), nil
}

// lastHeight returns the height of the last of numHeaders consecutive headers
// starting at the genesis block.
func lastHeight(numHeaders uint64) (uint32, error) {
	if numHeaders == 0 || numHeaders-1 > math.MaxUint32 {
		return 0, corruptErrf("header file holds %d headers",
			numHeaders)
	}

	return uint32(numHeaders - 1), nil
}

// blockHeaderStore is an implementation of the BlockHeaderStore interface, a
// fully fledged database for block headers. The blockHeaderStore
// combines a flat file to store the block headers with a database instance for
// managing the index into the set of flat files.
type blockHeaderStore struct {
	*headerStore
}

// A compile-time check to ensure the blockHeaderStore adheres to the
// BlockHeaderStore interface.
var _ BlockHeaderStore = (*blockHeaderStore)(nil)

// NewBlockHeaderStore creates a new instance of the blockHeaderStore based on
// a target file path, an open database instance, and finally a set of
// parameters for the target chain. These parameters are required as if this is
// the initial start up of the blockHeaderStore, then the initial genesis
// header will need to be inserted.
//
// If the file and the index disagree on the chain tip, they are reconciled:
// the side that is ahead is cut back to the other. An error wrapping
// ErrHeaderStoreCorrupt is returned if that isn't possible.
func NewBlockHeaderStore(filePath string, db walletdb.DB,
	netParams *chaincfg.Params) (store BlockHeaderStore, err error) {

	hStore, err := newHeaderStore(db, filePath, Block)
	if err != nil {
		return nil, err
	}

	bhs := &blockHeaderStore{
		headerStore: hStore,
	}
	defer func() {
		if err != nil {
			_ = bhs.Close()
		}
	}()

	numHeaders, err := bhs.trimPartialRecord()
	if err != nil {
		return nil, err
	}

	// If the file holds no header, then this means that we haven't yet
	// written the initial genesis header to disk, so we'll do so now.
	if numHeaders == 0 {
		if err := bhs.writeGenesis(netParams); err != nil {
			return nil, err
		}

		return bhs, nil
	}

	// As a final initialization step (if this isn't the first time), we'll
	// ensure that the header tip within the flat files, is in sync with
	// out database index.
	if err := bhs.reconcileTip(numHeaders); err != nil {
		return nil, err
	}

	return bhs, nil
}

// writeGenesis writes the genesis header to an empty file. Index entries that
// remain from headers the file no longer holds are dropped, as they would map
// hashes to heights the file can't serve.
func (h *blockHeaderStore) writeGenesis(netParams *chaincfg.Params) error {
	var removed int
	err := walletdb.Update(h.db, func(tx walletdb.ReadWriteTx) error {
		var err error
		removed, err = deleteEntriesAbove(
			tx.ReadWriteBucket(indexBucket), 0,
		)

		return err
	})
	if err != nil {
		return err
	}
	if removed > 0 {
		log.Warnf("Block header file is empty but its index had %d "+
			"entries above the genesis block, dropped them", removed)
	}

	genesisHeader := BlockHeader{
		BlockHeader: netParams.GenesisBlock.BlockHeader(),
		Height:      0,
	}

	return h.WriteHeaders(genesisHeader)
}

// reconcileTip brings the file, which holds numHeaders complete headers, and
// the index in agreement about the chain tip.
func (h *blockHeaderStore) reconcileTip(numHeaders uint64) error {
	fileHeight, err := lastHeight(numHeaders)
	if err != nil {
		return err
	}

	tipHash, tipHeight, err := h.chainTip()
	if err != nil {
		return corruptErrf("block header index has no usable tip: %w",
			err)
	}

	switch {
	// The file got ahead of the index, as a crash can leave the headers
	// appended but not yet indexed. Nothing refers to them, so they go.
	case fileHeight > tipHeight:
		log.Warnf("Block header file is ahead of its index (file "+
			"height %d, index height %d), truncating the file",
			fileHeight, tipHeight)

		err := h.truncateHeaders(fileHeight-tipHeight, h.indexType)
		if err != nil {
			return err
		}

	// The index is durable but the file lost data it had been given, so
	// the index can only be cut back to the headers that survived.
	case fileHeight < tipHeight:
		log.Warnf("Block header index is ahead of its file (index "+
			"height %d, file height %d), rolling the index back",
			tipHeight, fileHeight)

		latest, err := h.readHeader(fileHeight)
		if err != nil {
			return corruptErrf("block header at height %d: %w",
				fileHeight, err)
		}
		latestHash := latest.BlockHash()

		return h.rollbackIndex(&latestHash, fileHeight)
	}

	latest, err := h.readHeader(tipHeight)
	if err != nil {
		return corruptErrf("block header at height %d: %w", tipHeight,
			err)
	}
	latestHash := latest.BlockHash()
	if !tipHash.IsEqual(&latestHash) {
		return corruptErrf("block header index tip %v at height %d "+
			"doesn't match the header file (%v)", tipHash, tipHeight,
			latestHash)
	}

	return nil
}

// FetchHeader attempts to retrieve a block header determined by the passed
// block height.
//
// NOTE: Part of the BlockHeaderStore interface.
func (h *blockHeaderStore) FetchHeader(hash *chainhash.Hash) (*wire.BlockHeader, uint32, error) {
	// Lock store for read.
	h.mtx.RLock()
	defer h.mtx.RUnlock()

	// First, we'll query the index to obtain the block height of the
	// passed block hash.
	height, err := h.heightFromHash(hash)
	if err != nil {
		return nil, 0, err
	}

	// With the height known, we can now read the header from disk.
	header, err := h.readHeader(height)
	if err != nil {
		return nil, 0, err
	}

	return &header, height, nil
}

// FetchHeaderByHeight attempts to retrieve a target block header based on a
// block height.
//
// NOTE: Part of the BlockHeaderStore interface.
func (h *blockHeaderStore) FetchHeaderByHeight(height uint32) (*wire.BlockHeader, error) {
	// Lock store for read.
	h.mtx.RLock()
	defer h.mtx.RUnlock()

	// For this query, we don't need to consult the index, and can instead
	// just seek into the flat file based on the target height and return
	// the full header.
	header, err := h.readHeader(height)
	if err != nil {
		return nil, err
	}

	return &header, nil
}

// FetchHeaderAncestors fetches the numHeaders block headers that are the
// ancestors of the target stop hash. A total of numHeaders+1 headers will be
// returned, as we'll walk back numHeaders distance to collect each header,
// then return the final header specified by the stop hash. We'll also return
// the starting height of the header range as well so callers can compute the
// height of each header without knowing the height of the stop hash.
//
// NOTE: Part of the BlockHeaderStore interface.
func (h *blockHeaderStore) FetchHeaderAncestors(numHeaders uint32,
	stopHash *chainhash.Hash) ([]wire.BlockHeader, uint32, error) {

	// First, we'll find the final header in the range, this will be the
	// ending height of our scan.
	endHeight, err := h.heightFromHash(stopHash)
	if err != nil {
		return nil, 0, err
	}
	if numHeaders > endHeight {
		return nil, 0, fmt.Errorf("cannot fetch %d header ancestors ending at height %d", numHeaders, endHeight)
	}
	startHeight := endHeight - numHeaders

	headers, err := h.readHeaderRange(startHeight, endHeight)
	if err != nil {
		return nil, 0, err
	}

	return headers, startHeight, nil
}

// HeightFromHash returns the height of a particular block header given its
// hash.
//
// NOTE: Part of the BlockHeaderStore interface.
func (h *blockHeaderStore) HeightFromHash(hash *chainhash.Hash) (uint32, error) {
	return h.heightFromHash(hash)
}

// RollbackBlockHeaders removes the specified number of block headers from the
// end of the chain. It returns a BlockStamp representing the new chain tip. If
// numHeaders is 0, it returns an empty BlockStamp without performing any
// operations.
//
// The function ensures rollback doesn't remove or go beyond the genesis block
// (height 0). It determines the current chain tip height, reads the header
// range to be removed along with the new tip header, truncates the headers file
// to remove the specified number of headers, and updates the header indices to
// reflect the new chain tip.
func (h *blockHeaderStore) RollbackBlockHeaders(n uint32) (*BlockStamp, error) {
	if n == 0 {
		return &BlockStamp{}, nil
	}

	// Lock store for rollback.
	h.mtx.Lock()
	defer h.mtx.Unlock()

	// First, we'll obtain the latest height that the index knows of.
	_, chainTipHeight, err := h.chainTip()
	if err != nil {
		return nil, err
	}

	// Ensure the rollback doesn't remove or go beyond the genesis block.
	if n > chainTipHeight {
		return nil, fmt.Errorf("cannot roll back %d headers when "+
			"chain height is %d", n, chainTipHeight)
	}

	// With this height obtained, we'll use it to read the previous header
	// from disk, so we can populate our return value which requires the
	// prev header hash.
	headers, err := h.readHeaderRange(chainTipHeight-n, chainTipHeight)
	if err != nil {
		return nil, fmt.Errorf("failed to read headers range: %v", err)
	}
	prevHeader := headers[0]
	prevHeaderHash := prevHeader.BlockHash()

	// Transform to blockhashes for downstream operations, starting at
	// headers + 1 skipping the previous header.
	headersToTruncate := make([]*chainhash.Hash, len(headers)-1)
	for i, header := range headers[1:] {
		blkHash := header.BlockHash()
		headersToTruncate[i] = &blkHash
	}

	if err := h.truncateHeaders(n, h.indexType); err != nil {
		return nil, err
	}

	err = h.truncateIndices(&prevHeaderHash, headersToTruncate, true)
	if err != nil {
		return nil, err
	}

	return &BlockStamp{
		Height:    int32(chainTipHeight - n),
		Hash:      prevHeaderHash,
		Timestamp: prevHeader.Timestamp,
	}, nil
}

// RollbackLastBlock rollsback both the index, and on-disk header file by a
// _single_ header. This method is meant to be used in the case of re-org which
// disconnects the latest block header from the end of the main chain. The
// information about the new header tip after truncation is returned.
//
// NOTE: Part of the BlockHeaderStore interface.
func (h *blockHeaderStore) RollbackLastBlock() (*BlockStamp, error) {
	return h.RollbackBlockHeaders(1)
}

// BlockHeader is a block header that also has its height included.
type BlockHeader struct {
	*wire.BlockHeader

	// Height is the height of this block header within the current main
	// chain.
	Height uint32
}

// toIndexEntry converts the BlockHeader into a matching headerEntry. This
// method is used when a header is to be written to disk.
func (b *BlockHeader) toIndexEntry() headerEntry {
	return headerEntry{
		hash:   b.BlockHash(),
		height: b.Height,
	}
}

// WriteHeaders writes a set of headers to disk and updates the index in a
// single atomic transaction.
//
// NOTE: Part of the BlockHeaderStore interface.
func (h *blockHeaderStore) WriteHeaders(hdrs ...BlockHeader) error {
	// Lock store for write.
	h.mtx.Lock()
	defer h.mtx.Unlock()

	// First, we'll grab a buffer from the write buffer pool so we an
	// reduce our total number of allocations, and also write the headers
	// in a single swoop.
	headerBuf := headerBufPool.Get().(*bytes.Buffer)
	headerBuf.Reset()
	defer headerBufPool.Put(headerBuf)

	// Next, we'll write out all the passed headers in series into the
	// buffer we just extracted from the pool.
	for _, header := range hdrs {
		if err := header.Serialize(headerBuf); err != nil {
			return err
		}
	}

	// With all the headers written to the buffer, we'll now write out the
	// entire batch in a single write call.
	if err := h.appendRaw(headerBuf.Bytes()); err != nil {
		return err
	}

	// Once those are written, we'll then collate all the headers into
	// headerEntry instances so we can write them all into the index in a
	// single atomic batch.
	headerLocs := make([]headerEntry, len(hdrs))
	for i, header := range hdrs {
		headerLocs[i] = header.toIndexEntry()
	}

	// Attempt to add the headers to the database. If this fails, we'll need
	// to roll back any changes to the header file to maintain consistency.
	// The rollback process bases on the number of header serialized. If
	// both the initial operation and the rollback fail, we return
	// a detailed error explaining both failures to aid in debugging.
	if err := h.addHeaders(headerLocs); err != nil {
		// Since the probability of failing to write to the database is
		// very low, it is mostly worth the cost of file sync operation
		// to make sure truncate headers does it correctly.
		if err := h.file.Sync(); err != nil {
			return fmt.Errorf("failed to sync block headers "+
				"file: %v", err)
		}

		headersToRollback := len(hdrs)
		truncateErr := h.truncateHeaders(
			uint32(headersToRollback), h.indexType,
		)
		if truncateErr != nil {
			return fmt.Errorf("failed to rollback block headers "+
				"from binary file to previous valid state: "+
				"%v, error writing to database: %v, headers "+
				"to rollback: %d", truncateErr, err,
				headersToRollback)
		}
		return fmt.Errorf("failed to add block headers to db: %v", err)
	}

	return nil
}

// blockLocatorFromHash takes a given block hash and then creates a block
// locator using it as the root of the locator. We'll start by taking a single
// step backwards, then keep doubling the distance until genesis after we get
// 10 locators.
//
// TODO(roasbeef): make into single transaction.
func (h *blockHeaderStore) blockLocatorFromHash(hash *chainhash.Hash) (
	blockchain.BlockLocator, error) {

	var locator blockchain.BlockLocator

	// Append the initial hash
	locator = append(locator, hash)

	// If hash isn't found in DB or this is the genesis block, return the
	// locator as is.
	height, err := h.heightFromHash(hash)
	if height == 0 || err != nil {
		return locator, nil
	}

	decrement := uint32(1)
	for height > 0 && len(locator) < wire.MaxBlockLocatorsPerMsg {
		// Decrement by 1 for the first 10 blocks, then double the jump
		// until we get to the genesis hash
		if len(locator) > 10 {
			decrement *= 2
		}

		if decrement > height {
			height = 0
		} else {
			height -= decrement
		}

		blockHeader, err := h.FetchHeaderByHeight(height)
		if err != nil {
			return locator, err
		}
		headerHash := blockHeader.BlockHash()

		locator = append(locator, &headerHash)
	}

	return locator, nil
}

// LatestBlockLocator returns the latest block locator object based on the tip
// of the current main chain from the PoV of the database and flat files.
//
// NOTE: Part of the BlockHeaderStore interface.
func (h *blockHeaderStore) LatestBlockLocator() (blockchain.BlockLocator, error) {
	// Lock store for read.
	h.mtx.RLock()
	defer h.mtx.RUnlock()

	var locator blockchain.BlockLocator

	chainTipHash, _, err := h.chainTip()
	if err != nil {
		return locator, err
	}

	return h.blockLocatorFromHash(chainTipHash)
}

// BlockLocatorFromHash computes a block locator given a particular hash. The
// standard algorithm to compute block locators is employed.
func (h *blockHeaderStore) BlockLocatorFromHash(hash *chainhash.Hash) (
	blockchain.BlockLocator, error) {

	// Lock store for read.
	h.mtx.RLock()
	defer h.mtx.RUnlock()

	return h.blockLocatorFromHash(hash)
}

// CheckConnectivity cycles through all of the block headers on disk, from last
// to first, and makes sure they all connect to each other. Additionally, at
// each block header, we also ensure that the index entry for that height and
// hash also match up properly.
func (h *blockHeaderStore) CheckConnectivity() error {
	// Lock store for read.
	h.mtx.RLock()
	defer h.mtx.RUnlock()

	return walletdb.View(h.db, func(tx walletdb.ReadTx) error {
		// First, we'll fetch the chain tip so we can start our
		// backwards scan.
		_, tipHeight, err := h.chainTipWithTx(tx)
		if err != nil {
			return err
		}

		// With the height extracted, we'll now read the _last_ block
		// header within the file before we kick off our connectivity
		// loop.
		header, err := h.readHeader(tipHeight)
		if err != nil {
			return err
		}

		// We'll now cycle backwards, seeking backwards along the
		// header file to ensure each header connects properly and the
		// index entries are also accurate. To do this, we start from a
		// height of one before our current tip.
		var newHeader wire.BlockHeader
		for height := tipHeight - 1; height > 0; height-- {
			// First, read the block header for this block height,
			// and also compute the block hash for it.
			newHeader, err = h.readHeader(height)
			if err != nil {
				return fmt.Errorf("couldn't retrieve header "+
					"%s: %s", header.PrevBlock, err)
			}
			newHeaderHash := newHeader.BlockHash()

			// With the header retrieved, we'll now fetch the
			// height for this current header hash to ensure the
			// on-disk state and the index matches up properly.
			indexHeight, err := h.heightFromHashWithTx(
				tx, &newHeaderHash,
			)
			if err != nil {
				return fmt.Errorf("index and on-disk file "+
					"out of sync at height: %v", height)
			}

			// With the index entry retrieved, we'll now assert
			// that the height matches up with our current height
			// in this backwards walk.
			if indexHeight != height {
				return fmt.Errorf("index height isn't " +
					"monotonically increasing")
			}

			// Finally, we'll assert that this new header is
			// actually the prev header of the target header from
			// the last loop. This ensures connectivity.
			if newHeader.BlockHash() != header.PrevBlock {
				return fmt.Errorf("block %s doesn't match "+
					"block %s's PrevBlock (%s)",
					newHeader.BlockHash(),
					header.BlockHash(), header.PrevBlock)
			}

			// As all the checks have passed, we'll now reset our
			// header pointer to this current location, and
			// continue our backwards walk.
			header = newHeader
		}

		return nil
	})
}

// ChainTip returns the best known block header and height for the
// blockHeaderStore.
//
// NOTE: Part of the BlockHeaderStore interface.
func (h *blockHeaderStore) ChainTip() (*wire.BlockHeader, uint32, error) {
	// Lock store for read.
	h.mtx.RLock()
	defer h.mtx.RUnlock()

	_, tipHeight, err := h.chainTip()
	if err != nil {
		return nil, 0, fmt.Errorf("unable to fetch chain tip: %v", err)
	}

	latestHeader, err := h.readHeader(tipHeight)
	if err != nil {
		return nil, 0, fmt.Errorf("unable to read block header: %v", err)
	}

	return &latestHeader, tipHeight, nil
}

// FilterHeaderStore defines the interface for storing and retrieving filter
// headers.
type FilterHeaderStore interface {
	// ChainTip returns the hash and height of the latest filter header.
	ChainTip() (*chainhash.Hash, uint32, error)

	// FetchHeader fetches the filter header for a specific block hash.
	FetchHeader(hash *chainhash.Hash) (*chainhash.Hash, error)

	// FetchHeaderAncestors fetches the given number of headers starting
	// from the specified stop hash and working backwards.
	FetchHeaderAncestors(numHeaders uint32,
		stopHash *chainhash.Hash) ([]chainhash.Hash, uint32, error)

	// FetchHeaderByHeight fetches the filter header for a specific block
	// height.
	FetchHeaderByHeight(height uint32) (*chainhash.Hash, error)

	// WriteHeaders writes a set of filter headers to the store.
	WriteHeaders(hdrs ...FilterHeader) error

	// RollbackLastBlock rolls back the last block, returning the new tip
	// after rollback.
	RollbackLastBlock(newTip *chainhash.Hash) (*BlockStamp, error)
}

// filterHeaderStore is an implementation of a fully fledged database for any
// variant of filter headers. The filterHeaderStore combines a flat file to
// store the block headers with a database instance for managing the index into
// the set of flat files.
type filterHeaderStore struct {
	*headerStore
}

// Compile-time assertion to ensure filterHeaderStore implements
// FilterHeaderStore interface.
var _ FilterHeaderStore = (*filterHeaderStore)(nil)

// NewFilterHeaderStore returns a new instance of the FilterHeaderStore based
// on a target file path, filter type, and target net parameters. These
// parameters are required as if this is the initial start up of the
// FilterHeaderStore, then the initial genesis filter header will need to be
// inserted.
//
// If the file and the index disagree on the chain tip, they are reconciled:
// the side that is ahead is cut back to the other. An error wrapping
// ErrHeaderStoreCorrupt is returned if that isn't possible.
func NewFilterHeaderStore(filePath string, db walletdb.DB,
	filterType HeaderType, netParams *chaincfg.Params,
	headerStateAssertion *FilterHeader) (store FilterHeaderStore, err error) {

	fStore, err := newHeaderStore(db, filePath, filterType)
	if err != nil {
		return nil, err
	}

	fhs := &filterHeaderStore{
		fStore,
	}
	defer func() {
		if err != nil {
			_ = fhs.Close()
		}
	}()

	numHeaders, err := fhs.trimPartialRecord()
	if err != nil {
		return nil, err
	}

	// TODO(roasbeef): also reconsile with block header state due to way
	// roll back works atm

	// If the file holds no header, then this means that we haven't yet
	// written the initial genesis header to disk, so we'll do so now.
	if numHeaders == 0 {
		var genesisFilterHash chainhash.Hash
		switch filterType {
		case RegularFilter:
			basicFilter, err := builder.BuildBasicFilter(
				netParams.GenesisBlock, nil,
			)
			if err != nil {
				return nil, err
			}

			genesisFilterHash, err = builder.MakeHeaderForFilter(
				basicFilter,
				netParams.GenesisBlock.BlockHeader().PrevBlock,
			)
			if err != nil {
				return nil, err
			}

		default:
			return nil, fmt.Errorf("unknown filter type: %v", filterType)
		}

		genesisHeader := FilterHeader{
			HeaderHash: *netParams.GenesisHash,
			FilterHash: genesisFilterHash,
			Height:     0,
		}
		if err := fhs.WriteHeaders(genesisHeader); err != nil {
			return nil, err
		}

		return fhs, nil
	}

	// If we have a state assertion then we'll check it now to see if we
	// need to modify our filter header files before we proceed.
	if headerStateAssertion != nil {
		reset, err := fhs.maybeResetHeaderState(
			headerStateAssertion,
		)
		if err != nil {
			return nil, err
		}

		// If the filter header store was reset, we'll re-initialize it
		// to recreate our on-disk state.
		if reset {
			return NewFilterHeaderStore(
				filePath, db, filterType, netParams, nil,
			)
		}
	}

	// As a final initialization step, we'll ensure that the header tip
	// within the flat files, is in sync with out database index.
	if err := fhs.reconcileTip(numHeaders); err != nil {
		return nil, err
	}

	return fhs, nil
}

// reconcileTip brings the file, which holds numHeaders complete filter
// headers, and the index in agreement about the chain tip.
func (f *filterHeaderStore) reconcileTip(numHeaders uint64) error {
	fileHeight, err := lastHeight(numHeaders)
	if err != nil {
		return err
	}

	_, tipHeight, err := f.chainTip()

	// The tip refers to a block the block header index no longer knows,
	// which happens when that index was itself cut back to a shorter file.
	if errors.Is(err, ErrHeightNotFound) {
		return f.reconcileUnknownTip(fileHeight)
	}
	if err != nil {
		return corruptErrf("filter header index has no usable tip: %w",
			err)
	}

	switch {
	// The file got ahead of the index, as a crash can leave the headers
	// appended but not yet indexed. Nothing refers to them, so they go.
	case fileHeight > tipHeight:
		log.Warnf("Filter header file is ahead of its index (file "+
			"height %d, index height %d), truncating the file",
			fileHeight, tipHeight)

		return f.truncateHeaders(fileHeight-tipHeight, f.indexType)

	// The index is durable but the file lost data it had been given, so
	// the tip can only be moved back to the headers that survived.
	case fileHeight < tipHeight:
		log.Warnf("Filter header index is ahead of its file (index "+
			"height %d, file height %d), rolling the tip back",
			tipHeight, fileHeight)

		return f.setTipAtHeight(fileHeight)
	}

	return nil
}

// reconcileUnknownTip handles an index tip that isn't a known block anymore.
// The filter headers can't be ahead of the block headers they were made for, so
// the tip is moved back to the lower of the file and block header tips.
func (f *filterHeaderStore) reconcileUnknownTip(fileHeight uint32) error {
	blockHeight, err := f.blockTipHeight()
	if err != nil {
		return corruptErrf("filter header index tip is not a known "+
			"block and the block header tip is unavailable: %w", err)
	}

	tipHeight := min(fileHeight, blockHeight)
	log.Warnf("Filter header index tip is not a known block, moving it "+
		"back to height %d (file height %d, block header height %d)",
		tipHeight, fileHeight, blockHeight)

	if fileHeight > tipHeight {
		err := f.truncateHeaders(fileHeight-tipHeight, f.indexType)
		if err != nil {
			return err
		}
	}

	return f.setTipAtHeight(tipHeight)
}

// maybeResetHeaderState will reset the header state if the header assertion
// fails, but only if the target height is found. The boolean returned indicates
// that header state was reset.
func (f *filterHeaderStore) maybeResetHeaderState(
	headerStateAssertion *FilterHeader) (bool, error) {

	// First, we'll attempt to locate the header at this height. If no such
	// header is found, then we'll exit early.
	assertedHeader, err := f.FetchHeaderByHeight(
		headerStateAssertion.Height,
	)
	if _, ok := err.(*ErrHeaderNotFound); ok {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	// If our on disk state and the provided header assertion don't match,
	// then we'll purge this state so we can sync it anew once we fully
	// start up.
	if *assertedHeader != headerStateAssertion.FilterHash {
		// Close the file before removing it. This is required by some
		// OS, e.g., Windows.
		if err := f.file.Close(); err != nil {
			return true, err
		}
		if err := os.Remove(f.file.Name()); err != nil {
			return true, err
		}
		return true, nil
	}

	return false, nil
}

// FetchHeader returns the filter header that corresponds to the passed block
// height.
//
// NOTE: Part of the FilterHeaderStore interface.
func (f *filterHeaderStore) FetchHeader(
	hash *chainhash.Hash) (*chainhash.Hash, error) {

	// Lock store for read.
	f.mtx.RLock()
	defer f.mtx.RUnlock()

	height, err := f.heightFromHash(hash)
	if err != nil {
		return nil, err
	}

	return f.readHeader(height)
}

// FetchHeaderByHeight returns the filter header for a particular block height.
//
// NOTE: Part of the FilterHeaderStore interface.
func (f *filterHeaderStore) FetchHeaderByHeight(
	height uint32) (*chainhash.Hash, error) {

	// Lock store for read.
	f.mtx.RLock()
	defer f.mtx.RUnlock()

	return f.readHeader(height)
}

// FetchHeaderAncestors fetches the numHeaders filter headers that are the
// ancestors of the target stop block hash. A total of numHeaders+1 headers will be
// returned, as we'll walk back numHeaders distance to collect each header,
// then return the final header specified by the stop hash. We'll also return
// the starting height of the header range as well so callers can compute the
// height of each header without knowing the height of the stop hash.
//
// NOTE: Part of the FilterHeaderStore interface.
func (f *filterHeaderStore) FetchHeaderAncestors(numHeaders uint32,
	stopHash *chainhash.Hash) ([]chainhash.Hash, uint32, error) {

	// First, we'll find the final header in the range, this will be the
	// ending height of our scan.
	endHeight, err := f.heightFromHash(stopHash)
	if err != nil {
		return nil, 0, err
	}
	if numHeaders > endHeight {
		return nil, 0, fmt.Errorf("cannot fetch %d header ancestors ending at height %d", numHeaders, endHeight)
	}
	startHeight := endHeight - numHeaders

	headers, err := f.readHeaderRange(startHeight, endHeight)
	if err != nil {
		return nil, 0, err
	}

	return headers, startHeight, nil
}

// FilterHeader represents a filter header (basic or extended). The filter
// header itself is coupled with the block height and hash of the filter's
// block.
type FilterHeader struct {
	// HeaderHash is the hash of the block header that this filter header
	// corresponds to.
	HeaderHash chainhash.Hash

	// FilterHash is the filter header itself.
	FilterHash chainhash.Hash

	// Height is the block height of the filter header in the main chain.
	Height uint32
}

// toIndexEntry converts the filter header into a index entry to be stored
// within the database.
func (f *FilterHeader) toIndexEntry() headerEntry {
	return headerEntry{
		hash:   f.HeaderHash,
		height: f.Height,
	}
}

// WriteHeaders writes a batch of filter headers to persistent storage. The
// headers themselves are appended to the flat file, and then the index updated
// to reflect the new entries.
//
// NOTE: Part of the FilterHeaderStore interface.
func (f *filterHeaderStore) WriteHeaders(hdrs ...FilterHeader) error {
	// Lock store for write.
	f.mtx.Lock()
	defer f.mtx.Unlock()

	// If there are 0 headers to be written, return immediately. This
	// prevents the newTip assignment from panicking because of an index
	// of -1.
	if len(hdrs) == 0 {
		return nil
	}

	// First, we'll grab a buffer from the write buffer pool so we an
	// reduce our total number of allocations, and also write the headers
	// in a single swoop.
	headerBuf := headerBufPool.Get().(*bytes.Buffer)
	headerBuf.Reset()
	defer headerBufPool.Put(headerBuf)

	// Next, we'll write out all the passed headers in series into the
	// buffer we just extracted from the pool.
	for _, header := range hdrs {
		if _, err := headerBuf.Write(header.FilterHash[:]); err != nil {
			return err
		}
	}

	// With all the headers written to the buffer, we'll now write out the
	// entire batch in a single write call.
	if err := f.appendRaw(headerBuf.Bytes()); err != nil {
		return err
	}

	// As the block headers should already be written, we only need to
	// update the tip pointer for this particular header type.
	newTip := hdrs[len(hdrs)-1].toIndexEntry().hash

	// Attempt to add the headers to the database. If this fails, we'll need
	// to roll back any changes to the header file to maintain consistency.
	// The rollback process bases on the number of header serialized. If
	// both the initial operation and the rollback fail, we return
	// a detailed error explaining both failures to aid in debugging.
	if err := f.truncateIndices(&newTip, nil, false); err != nil {
		// Since the probability of failing to write to the database is
		// very low, it is mostly worth the cost of file sync operation
		// to make sure truncate headers does it correctly.
		if err := f.file.Sync(); err != nil {
			return fmt.Errorf("failed to sync filter headers "+
				"file: %v", err)
		}

		headersToRollback := len(hdrs)
		truncateErr := f.truncateHeaders(
			uint32(headersToRollback), f.indexType,
		)
		if truncateErr != nil {
			return fmt.Errorf("failed to rollback filter headers "+
				"binary file to previous valid state: %v, "+
				"error writing to database: %v", truncateErr,
				err)
		}
		return fmt.Errorf("failed to add filter headers to db: %v", err)
	}

	return nil
}

// ChainTip returns the latest filter header and height known to the
// FilterHeaderStore.
//
// NOTE: Part of the FilterHeaderStore interface.
func (f *filterHeaderStore) ChainTip() (*chainhash.Hash, uint32, error) {
	// Lock store for read.
	f.mtx.RLock()
	defer f.mtx.RUnlock()

	_, tipHeight, err := f.chainTip()
	if err != nil {
		return nil, 0, fmt.Errorf("unable to fetch chain tip: %v", err)
	}

	latestHeader, err := f.readHeader(tipHeight)
	if err != nil {
		return nil, 0, fmt.Errorf("unable to read filter header: %v", err)
	}

	return latestHeader, tipHeight, nil
}

// RollbackLastBlock rollsback both the index, and on-disk header file by a
// _single_ filter header. This method is meant to be used in the case of
// re-org which disconnects the latest filter header from the end of the main
// chain. The information about the latest header tip after truncation is
// returned.
//
// NOTE: Part of the FilterHeaderStore interface.
func (f *filterHeaderStore) RollbackLastBlock(
	newTip *chainhash.Hash) (*BlockStamp, error) {

	// Lock store for write.
	f.mtx.Lock()
	defer f.mtx.Unlock()

	// First, we'll obtain the latest height that the index knows of.
	_, chainTipHeight, err := f.chainTip()
	if err != nil {
		return nil, err
	}

	// With this height obtained, we'll use it to read what will be the new
	// chain tip from disk.
	newHeightTip := chainTipHeight - 1
	newHeaderTip, err := f.readHeader(newHeightTip)
	if err != nil {
		return nil, err
	}

	// Now that we have the information we need to return from this
	// function, we can now truncate both the header file and the index.
	if err := f.truncateHeaders(1, f.indexType); err != nil {
		return nil, err
	}

	err = f.truncateIndices(newTip, []*chainhash.Hash{}, false)
	if err != nil {
		return nil, err
	}

	// TODO(roasbeef): return chain hash also?
	return &BlockStamp{
		Height: int32(newHeightTip),
		Hash:   *newHeaderTip,
	}, nil
}
