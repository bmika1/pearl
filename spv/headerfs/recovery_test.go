package headerfs

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/pearl-research-labs/pearl/node/chaincfg"
	"github.com/pearl-research-labs/pearl/node/chaincfg/chainhash"
	"github.com/pearl-research-labs/pearl/node/wire"
	"github.com/pearl-research-labs/pearl/wallet/walletdb"
	"github.com/stretchr/testify/require"
)

// genesisFilterHash is the filter header stored at height 0 of a fresh
// filter store.
func genesisFilterHash(t *testing.T, fhs FilterHeaderStore) chainhash.Hash {
	t.Helper()

	hash, err := fhs.FetchHeaderByHeight(0)
	require.NoError(t, err)

	return *hash
}

func headerFilePath(dir string, hType HeaderType) string {
	switch hType {
	case Block:
		return filepath.Join(dir, "block_headers.bin")
	default:
		return filepath.Join(dir, "reg_filter_headers.bin")
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()

	info, err := os.Stat(path)
	require.NoError(t, err)

	return info.Size()
}

// dropFileTail removes the last numBytes of the file, which is what a crash
// leaves behind when appended data never made it to disk.
func dropFileTail(t *testing.T, path string, numBytes int64) {
	t.Helper()

	require.NoError(t, os.Truncate(path, fileSize(t, path)-numBytes))
}

// appendGarbage adds numBytes of junk to the file, simulating a torn append.
func appendGarbage(t *testing.T, path string, numBytes int) {
	t.Helper()

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	require.NoError(t, err)
	defer f.Close()

	junk := make([]byte, numBytes)
	for i := range junk {
		junk[i] = 0xab
	}
	_, err = f.Write(junk)
	require.NoError(t, err)
}

func preloadBlockIndex(t *testing.T, db walletdb.DB, chain []FilterHeader) {
	t.Helper()

	err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		rootBucket := tx.ReadWriteBucket(indexBucket)

		genesis := headerEntry{hash: *chaincfg.SimNetParams.GenesisHash}
		if err := putHeaderEntry(rootBucket, genesis); err != nil {
			return err
		}

		for _, header := range chain {
			entry := headerEntry{
				hash:   header.HeaderHash,
				height: header.Height,
			}
			if err := putHeaderEntry(rootBucket, entry); err != nil {
				return err
			}
		}

		return nil
	})
	require.NoError(t, err)
}

func blockHeaderAt(chain []BlockHeader, height uint32) wire.BlockHeader {
	if height == 0 {
		return *chaincfg.SimNetParams.GenesisBlock.BlockHeader()
	}

	return *chain[height-1].BlockHeader
}

// TestBlockHeaderStoreIndexAheadOfFile covers a crash that leaves the durable
// index ahead of a header file that lost its unsynced tail.
func TestBlockHeaderStoreIndexAheadOfFile(t *testing.T) {
	const numHeaders = 10

	for _, lost := range []uint32{1, 3, numHeaders} {
		lost := lost
		t.Run(fmt.Sprintf("lost_%d", lost), func(t *testing.T) {
			cleanUp, db, dir, bhs, err := createTestBlockHeaderStore()
			require.NoError(t, err)
			defer cleanUp()

			chain := createTestBlockHeaderChain(numHeaders)
			require.NoError(t, bhs.WriteHeaders(chain...))

			path := headerFilePath(dir, Block)
			dropFileTail(t, path, int64(lost)*BlockHeaderSize)

			store, err := NewBlockHeaderStore(
				dir, db, &chaincfg.SimNetParams,
			)
			require.NoError(t, err)

			wantHeight := uint32(numHeaders) - lost
			wantHeader := blockHeaderAt(chain, wantHeight)

			tip, tipHeight, err := store.ChainTip()
			require.NoError(t, err)
			require.Equal(t, wantHeight, tipHeight)
			require.Equal(t, wantHeader, *tip)

			// The file must not have been touched any further.
			require.EqualValues(
				t, int64(wantHeight+1)*BlockHeaderSize,
				fileSize(t, path),
			)

			// Index entries above the file tip are gone, entries up
			// to it remain.
			for _, header := range chain {
				hash := header.BlockHash()
				height, err := store.HeightFromHash(&hash)
				if header.Height > wantHeight {
					require.ErrorIs(t, err, ErrHashNotFound)
					continue
				}
				require.NoError(t, err)
				require.Equal(t, header.Height, height)
			}

			if wantHeight > 0 {
				require.NoError(
					t, store.(*blockHeaderStore).CheckConnectivity(),
				)
			}

			// The store keeps working: the lost headers can be
			// written again.
			require.NoError(t, store.WriteHeaders(
				chain[wantHeight:]...,
			))
			_, tipHeight, err = store.ChainTip()
			require.NoError(t, err)
			require.EqualValues(t, numHeaders, tipHeight)
			require.NoError(
				t, store.(*blockHeaderStore).CheckConnectivity(),
			)
		})
	}
}

// TestBlockHeaderStorePartialTrailingRecord ensures a torn final append is cut
// off, since any later append would otherwise be misaligned.
func TestBlockHeaderStorePartialTrailingRecord(t *testing.T) {
	const numHeaders = 10

	cleanUp, db, dir, bhs, err := createTestBlockHeaderStore()
	require.NoError(t, err)
	defer cleanUp()

	chain := createTestBlockHeaderChain(numHeaders + 2)
	require.NoError(t, bhs.WriteHeaders(chain[:numHeaders]...))

	path := headerFilePath(dir, Block)
	appendGarbage(t, path, BlockHeaderSize/2)

	store, err := NewBlockHeaderStore(dir, db, &chaincfg.SimNetParams)
	require.NoError(t, err)

	require.EqualValues(
		t, (numHeaders+1)*BlockHeaderSize, fileSize(t, path),
	)

	_, tipHeight, err := store.ChainTip()
	require.NoError(t, err)
	require.EqualValues(t, numHeaders, tipHeight)

	require.NoError(t, store.WriteHeaders(chain[numHeaders:]...))
	for _, header := range chain {
		got, err := store.FetchHeaderByHeight(header.Height)
		require.NoError(t, err)
		require.Equal(t, *header.BlockHeader, *got)
	}
}

// TestBlockHeaderStorePartialRecordAndIndexAhead combines a torn append with a
// file that is also shorter than the index.
func TestBlockHeaderStorePartialRecordAndIndexAhead(t *testing.T) {
	const (
		numHeaders = 10
		lost       = 2
	)

	cleanUp, db, dir, bhs, err := createTestBlockHeaderStore()
	require.NoError(t, err)
	defer cleanUp()

	chain := createTestBlockHeaderChain(numHeaders)
	require.NoError(t, bhs.WriteHeaders(chain...))

	path := headerFilePath(dir, Block)
	dropFileTail(t, path, lost*BlockHeaderSize-BlockHeaderSize/3)

	store, err := NewBlockHeaderStore(dir, db, &chaincfg.SimNetParams)
	require.NoError(t, err)

	tip, tipHeight, err := store.ChainTip()
	require.NoError(t, err)
	require.EqualValues(t, numHeaders-lost, tipHeight)
	require.Equal(t, blockHeaderAt(chain, numHeaders-lost), *tip)
	require.EqualValues(
		t, (numHeaders-lost+1)*BlockHeaderSize, fileSize(t, path),
	)
}

// TestBlockHeaderStoreEmptyFileNonEmptyIndex covers a header file that lost
// all of its contents while the index still describes a long chain.
func TestBlockHeaderStoreEmptyFileNonEmptyIndex(t *testing.T) {
	const numHeaders = 10

	cleanUp, db, dir, bhs, err := createTestBlockHeaderStore()
	require.NoError(t, err)
	defer cleanUp()

	chain := createTestBlockHeaderChain(numHeaders)
	require.NoError(t, bhs.WriteHeaders(chain...))

	path := headerFilePath(dir, Block)
	require.NoError(t, os.Truncate(path, 0))

	store, err := NewBlockHeaderStore(dir, db, &chaincfg.SimNetParams)
	require.NoError(t, err)

	tip, tipHeight, err := store.ChainTip()
	require.NoError(t, err)
	require.Zero(t, tipHeight)
	require.Equal(t, blockHeaderAt(chain, 0), *tip)
	require.EqualValues(t, BlockHeaderSize, fileSize(t, path))

	for _, header := range chain {
		hash := header.BlockHash()
		_, err := store.HeightFromHash(&hash)
		require.ErrorIs(t, err, ErrHashNotFound)
	}

	require.NoError(t, store.WriteHeaders(chain...))
	_, tipHeight, err = store.ChainTip()
	require.NoError(t, err)
	require.EqualValues(t, numHeaders, tipHeight)
}

// TestBlockHeaderStoreReopenUnchanged makes sure a store that is consistent on
// disk is left exactly as it was.
func TestBlockHeaderStoreReopenUnchanged(t *testing.T) {
	const numHeaders = 10

	cleanUp, db, dir, bhs, err := createTestBlockHeaderStore()
	require.NoError(t, err)
	defer cleanUp()

	chain := createTestBlockHeaderChain(numHeaders)
	require.NoError(t, bhs.WriteHeaders(chain...))

	path := headerFilePath(dir, Block)
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	store, err := NewBlockHeaderStore(dir, db, &chaincfg.SimNetParams)
	require.NoError(t, err)

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after)

	tip, tipHeight, err := store.ChainTip()
	require.NoError(t, err)
	require.EqualValues(t, numHeaders, tipHeight)
	require.Equal(t, *chain[numHeaders-1].BlockHeader, *tip)
	require.NoError(t, store.(*blockHeaderStore).CheckConnectivity())
}

// TestFilterHeaderStoreIndexAheadOfFile is the filter header counterpart of
// the block header index-ahead test.
func TestFilterHeaderStoreIndexAheadOfFile(t *testing.T) {
	const numHeaders = 10

	for _, lost := range []uint32{1, 3, numHeaders} {
		lost := lost
		t.Run(fmt.Sprintf("lost_%d", lost), func(t *testing.T) {
			cleanUp, db, dir, fhs, err := createTestFilterHeaderStore()
			require.NoError(t, err)
			defer cleanUp()

			chain := createTestFilterHeaderChain(numHeaders)
			preloadBlockIndex(t, db, chain)
			require.NoError(t, fhs.WriteHeaders(chain...))
			genesis := genesisFilterHash(t, fhs)

			path := headerFilePath(dir, RegularFilter)
			dropFileTail(
				t, path, int64(lost)*RegularFilterHeaderSize,
			)

			store, err := NewFilterHeaderStore(
				dir, db, RegularFilter, &chaincfg.SimNetParams,
				nil,
			)
			require.NoError(t, err)

			wantHeight := uint32(numHeaders) - lost
			wantFilter := genesis
			if wantHeight > 0 {
				wantFilter = chain[wantHeight-1].FilterHash
			}

			tip, tipHeight, err := store.ChainTip()
			require.NoError(t, err)
			require.Equal(t, wantHeight, tipHeight)
			require.Equal(t, wantFilter, *tip)
			require.EqualValues(
				t, int64(wantHeight+1)*RegularFilterHeaderSize,
				fileSize(t, path),
			)

			// The lost filter headers can be written again.
			require.NoError(t, store.WriteHeaders(
				chain[wantHeight:]...,
			))
			tip, tipHeight, err = store.ChainTip()
			require.NoError(t, err)
			require.EqualValues(t, numHeaders, tipHeight)
			require.Equal(t, chain[numHeaders-1].FilterHash, *tip)
		})
	}
}

func TestFilterHeaderStorePartialTrailingRecord(t *testing.T) {
	const numHeaders = 10

	cleanUp, db, dir, fhs, err := createTestFilterHeaderStore()
	require.NoError(t, err)
	defer cleanUp()

	chain := createTestFilterHeaderChain(numHeaders)
	preloadBlockIndex(t, db, chain)
	require.NoError(t, fhs.WriteHeaders(chain...))

	path := headerFilePath(dir, RegularFilter)
	appendGarbage(t, path, RegularFilterHeaderSize/2)

	store, err := NewFilterHeaderStore(
		dir, db, RegularFilter, &chaincfg.SimNetParams, nil,
	)
	require.NoError(t, err)

	require.EqualValues(
		t, (numHeaders+1)*RegularFilterHeaderSize, fileSize(t, path),
	)
	tip, tipHeight, err := store.ChainTip()
	require.NoError(t, err)
	require.EqualValues(t, numHeaders, tipHeight)
	require.Equal(t, chain[numHeaders-1].FilterHash, *tip)
}

func TestFilterHeaderStoreEmptyFileNonEmptyIndex(t *testing.T) {
	const numHeaders = 10

	cleanUp, db, dir, fhs, err := createTestFilterHeaderStore()
	require.NoError(t, err)
	defer cleanUp()

	chain := createTestFilterHeaderChain(numHeaders)
	preloadBlockIndex(t, db, chain)
	require.NoError(t, fhs.WriteHeaders(chain...))
	genesis := genesisFilterHash(t, fhs)

	path := headerFilePath(dir, RegularFilter)
	require.NoError(t, os.Truncate(path, 0))

	store, err := NewFilterHeaderStore(
		dir, db, RegularFilter, &chaincfg.SimNetParams, nil,
	)
	require.NoError(t, err)

	tip, tipHeight, err := store.ChainTip()
	require.NoError(t, err)
	require.Zero(t, tipHeight)
	require.Equal(t, genesis, *tip)
	require.EqualValues(
		t, RegularFilterHeaderSize, fileSize(t, path),
	)
}

func TestFilterHeaderStoreReopenUnchanged(t *testing.T) {
	const numHeaders = 10

	cleanUp, db, dir, fhs, err := createTestFilterHeaderStore()
	require.NoError(t, err)
	defer cleanUp()

	chain := createTestFilterHeaderChain(numHeaders)
	preloadBlockIndex(t, db, chain)
	require.NoError(t, fhs.WriteHeaders(chain...))

	path := headerFilePath(dir, RegularFilter)
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	store, err := NewFilterHeaderStore(
		dir, db, RegularFilter, &chaincfg.SimNetParams, nil,
	)
	require.NoError(t, err)

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after)

	tip, tipHeight, err := store.ChainTip()
	require.NoError(t, err)
	require.EqualValues(t, numHeaders, tipHeight)
	require.Equal(t, chain[numHeaders-1].FilterHash, *tip)
}

func filterChainFor(blocks []BlockHeader) []FilterHeader {
	filters := make([]FilterHeader, len(blocks))
	for i, block := range blocks {
		filters[i] = FilterHeader{
			HeaderHash: block.BlockHash(),
			FilterHash: chainhash.DoubleHashH([]byte{byte(i)}),
			Height:     block.Height,
		}
	}

	return filters
}

// openHeaderFiles lists the flat header files this process holds open under
// dir.
func openHeaderFiles(t *testing.T, dir string) []string {
	t.Helper()

	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("needs /proc to list open files")
	}

	var open []string
	for _, fd := range fds {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name()))
		if err != nil {
			continue
		}
		if filepath.Dir(target) == dir && filepath.Ext(target) == ".bin" {
			open = append(open, filepath.Base(target))
		}
	}

	return open
}

// TestFilterHeaderStoreFollowsBlockHeaderRollback covers a crash that cuts
// both files short by different amounts: the block header index is rolled back
// first, which leaves the filter header tip pointing at a block that is gone.
func TestFilterHeaderStoreFollowsBlockHeaderRollback(t *testing.T) {
	const (
		numHeaders  = 10
		lostBlocks  = 4
		lostFilters = 1
	)

	cleanUp, db, dir, bhs, err := createTestBlockHeaderStore()
	require.NoError(t, err)
	defer cleanUp()

	blocks := createTestBlockHeaderChain(numHeaders)
	require.NoError(t, bhs.WriteHeaders(blocks...))

	fhs, err := NewFilterHeaderStore(
		dir, db, RegularFilter, &chaincfg.SimNetParams, nil,
	)
	require.NoError(t, err)
	filters := filterChainFor(blocks)
	require.NoError(t, fhs.WriteHeaders(filters...))
	genesis := genesisFilterHash(t, fhs)

	dropFileTail(
		t, headerFilePath(dir, Block), lostBlocks*BlockHeaderSize,
	)
	dropFileTail(
		t, headerFilePath(dir, RegularFilter),
		lostFilters*RegularFilterHeaderSize,
	)

	blockStore, err := NewBlockHeaderStore(dir, db, &chaincfg.SimNetParams)
	require.NoError(t, err)
	filterStore, err := NewFilterHeaderStore(
		dir, db, RegularFilter, &chaincfg.SimNetParams, nil,
	)
	require.NoError(t, err)

	const wantHeight = numHeaders - lostBlocks

	_, blockHeight, err := blockStore.ChainTip()
	require.NoError(t, err)
	require.EqualValues(t, wantHeight, blockHeight)

	tip, filterHeight, err := filterStore.ChainTip()
	require.NoError(t, err)
	require.EqualValues(t, wantHeight, filterHeight)
	require.Equal(t, filters[wantHeight-1].FilterHash, *tip)
	require.EqualValues(
		t, (wantHeight+1)*RegularFilterHeaderSize,
		fileSize(t, headerFilePath(dir, RegularFilter)),
	)
	require.NotEqual(t, genesis, *tip)

	// Both stores can be extended again from the surviving tip.
	require.NoError(t, blockStore.WriteHeaders(blocks[wantHeight:]...))
	require.NoError(t, filterStore.WriteHeaders(filters[wantHeight:]...))
	_, filterHeight, err = filterStore.ChainTip()
	require.NoError(t, err)
	require.EqualValues(t, numHeaders, filterHeight)
}

// TestFilterHeaderStoreResetWithBlockHeaders covers block headers that were
// reset to genesis, which leaves nothing for the filter headers to refer to
// but the genesis block.
func TestFilterHeaderStoreResetWithBlockHeaders(t *testing.T) {
	const numHeaders = 10

	cleanUp, db, dir, bhs, err := createTestBlockHeaderStore()
	require.NoError(t, err)
	defer cleanUp()

	blocks := createTestBlockHeaderChain(numHeaders)
	require.NoError(t, bhs.WriteHeaders(blocks...))

	fhs, err := NewFilterHeaderStore(
		dir, db, RegularFilter, &chaincfg.SimNetParams, nil,
	)
	require.NoError(t, err)
	require.NoError(t, fhs.WriteHeaders(filterChainFor(blocks)...))
	genesis := genesisFilterHash(t, fhs)

	require.NoError(t, os.Truncate(headerFilePath(dir, Block), 0))

	_, err = NewBlockHeaderStore(dir, db, &chaincfg.SimNetParams)
	require.NoError(t, err)
	filterStore, err := NewFilterHeaderStore(
		dir, db, RegularFilter, &chaincfg.SimNetParams, nil,
	)
	require.NoError(t, err)

	tip, tipHeight, err := filterStore.ChainTip()
	require.NoError(t, err)
	require.Zero(t, tipHeight)
	require.Equal(t, genesis, *tip)
	require.EqualValues(
		t, RegularFilterHeaderSize,
		fileSize(t, headerFilePath(dir, RegularFilter)),
	)
}

// TestBlockHeaderStoreLegacyIndexLayout makes sure entries kept directly in the
// root bucket, as older versions did, are rolled back too.
func TestBlockHeaderStoreLegacyIndexLayout(t *testing.T) {
	const (
		numHeaders = 10
		lost       = 3
	)

	cleanUp, db, dir, bhs, err := createTestBlockHeaderStore()
	require.NoError(t, err)
	defer cleanUp()

	chain := createTestBlockHeaderChain(numHeaders)
	require.NoError(t, bhs.WriteHeaders(chain...))

	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		rootBucket := tx.ReadWriteBucket(indexBucket)

		var entries []headerEntry
		err := forEachEntry(rootBucket, func(hash []byte, h uint32) error {
			entry := headerEntry{height: h}
			copy(entry.hash[:], hash)
			entries = append(entries, entry)

			return nil
		})
		if err != nil {
			return err
		}

		for _, entry := range entries {
			prefix := entry.hash[:numSubBucketBytes]
			if rootBucket.NestedReadBucket(prefix) != nil {
				err := rootBucket.DeleteNestedBucket(prefix)
				if err != nil {
					return err
				}
			}
		}
		for _, entry := range entries {
			var height [4]byte
			binary.BigEndian.PutUint32(height[:], entry.height)
			err := rootBucket.Put(entry.hash[:], height[:])
			if err != nil {
				return err
			}
		}

		return nil
	})
	require.NoError(t, err)

	dropFileTail(t, headerFilePath(dir, Block), lost*BlockHeaderSize)

	store, err := NewBlockHeaderStore(dir, db, &chaincfg.SimNetParams)
	require.NoError(t, err)

	_, tipHeight, err := store.ChainTip()
	require.NoError(t, err)
	require.EqualValues(t, numHeaders-lost, tipHeight)

	for _, header := range chain {
		hash := header.BlockHash()
		_, err := store.HeightFromHash(&hash)
		if header.Height > numHeaders-lost {
			require.ErrorIs(t, err, ErrHashNotFound)
			continue
		}
		require.NoError(t, err)
	}
}

// TestBlockHeaderStoreCorrupt covers states that can't be repaired in place
// and must be reported as such.
func TestBlockHeaderStoreCorrupt(t *testing.T) {
	const numHeaders = 10

	tests := []struct {
		name   string
		break_ func(t *testing.T, db walletdb.DB, dir string)
	}{
		{
			name: "index lost, file kept",
			break_: func(t *testing.T, db walletdb.DB, _ string) {
				err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
					return tx.DeleteTopLevelBucket(indexBucket)
				})
				require.NoError(t, err)
			},
		},
		{
			name: "tip header differs from the file",
			break_: func(t *testing.T, _ walletdb.DB, dir string) {
				path := headerFilePath(dir, Block)
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				data[len(data)-1] ^= 0xff
				require.NoError(t, os.WriteFile(path, data, 0o644))
			},
		},
		{
			name: "file tail zeroed after index moved ahead",
			break_: func(t *testing.T, _ walletdb.DB, dir string) {
				path := headerFilePath(dir, Block)
				data, err := os.ReadFile(path)
				require.NoError(t, err)

				keep := len(data) - 2*BlockHeaderSize
				for i := keep - BlockHeaderSize; i < keep; i++ {
					data[i] = 0
				}
				require.NoError(
					t, os.WriteFile(path, data[:keep], 0o644),
				)
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			cleanUp, db, dir, bhs, err := createTestBlockHeaderStore()
			require.NoError(t, err)
			defer cleanUp()

			chain := createTestBlockHeaderChain(numHeaders)
			require.NoError(t, bhs.WriteHeaders(chain...))
			require.NoError(t, bhs.Close())

			test.break_(t, db, dir)

			store, err := NewBlockHeaderStore(
				dir, db, &chaincfg.SimNetParams,
			)
			require.Nil(t, store)
			require.ErrorIs(t, err, ErrHeaderStoreCorrupt)

			// A failed open must not keep the file open, or it
			// can't be moved aside on Windows.
			require.Empty(t, openHeaderFiles(t, dir))
		})
	}
}

func TestFilterHeaderStoreCorrupt(t *testing.T) {
	const numHeaders = 10

	tests := []struct {
		name   string
		break_ func(t *testing.T, db walletdb.DB)
	}{
		{
			name: "tip key lost, file kept",
			break_: func(t *testing.T, db walletdb.DB) {
				err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
					rootBucket := tx.ReadWriteBucket(indexBucket)

					return rootBucket.Delete(regFilterTip)
				})
				require.NoError(t, err)
			},
		},
		{
			name: "tip unknown and no block header tip",
			break_: func(t *testing.T, db walletdb.DB) {
				err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
					rootBucket := tx.ReadWriteBucket(indexBucket)
					unknown := chainhash.DoubleHashH([]byte("x"))

					return rootBucket.Put(regFilterTip, unknown[:])
				})
				require.NoError(t, err)
			},
		},
		{
			name: "tip unknown and block header tip unknown",
			break_: func(t *testing.T, db walletdb.DB) {
				err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
					rootBucket := tx.ReadWriteBucket(indexBucket)
					unknown := chainhash.DoubleHashH([]byte("x"))
					if err := rootBucket.Put(regFilterTip, unknown[:]); err != nil {
						return err
					}

					return rootBucket.Put(blockTip, unknown[:])
				})
				require.NoError(t, err)
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			cleanUp, db, dir, fhs, err := createTestFilterHeaderStore()
			require.NoError(t, err)
			defer cleanUp()

			chain := createTestFilterHeaderChain(numHeaders)
			preloadBlockIndex(t, db, chain)
			require.NoError(t, fhs.WriteHeaders(chain...))
			require.NoError(t, fhs.(*filterHeaderStore).Close())

			test.break_(t, db)

			store, err := NewFilterHeaderStore(
				dir, db, RegularFilter, &chaincfg.SimNetParams,
				nil,
			)
			require.Nil(t, store)
			require.ErrorIs(t, err, ErrHeaderStoreCorrupt)
			require.Empty(t, openHeaderFiles(t, dir))
		})
	}
}

// TestTruncateHeadersBounds makes sure asking for more headers than the file
// holds is refused before it reaches the truncate syscall.
func TestTruncateHeadersBounds(t *testing.T) {
	cleanUp, _, dir, bhs, err := createTestBlockHeaderStore()
	require.NoError(t, err)
	defer cleanUp()

	path := headerFilePath(dir, Block)
	require.EqualValues(t, BlockHeaderSize, fileSize(t, path))

	require.Error(t, bhs.truncateHeaders(2, Block))
	require.Error(t, bhs.truncateHeaders(math.MaxUint32, Block))
	require.EqualValues(t, BlockHeaderSize, fileSize(t, path))

	require.NoError(t, bhs.truncateHeaders(1, Block))
	require.Zero(t, fileSize(t, path))
}

// TestSetTipAtHeightNeedsOneHeader makes sure a tip is only pointed at a height
// that has exactly one header, as picking either of two would be a guess.
func TestSetTipAtHeightNeedsOneHeader(t *testing.T) {
	cleanUp, hIndex, err := createTestIndex(t)
	require.NoError(t, err)
	defer cleanUp()

	put := func(seed string, height uint32) chainhash.Hash {
		entry := headerEntry{hash: chainhash.HashH([]byte(seed)), height: height}
		err := walletdb.Update(hIndex.db, func(tx walletdb.ReadWriteTx) error {
			return putHeaderEntry(tx.ReadWriteBucket(indexBucket), entry)
		})
		require.NoError(t, err)

		return entry.hash
	}

	only := put("only", 3)
	put("first", 5)
	put("second", 5)

	require.NoError(t, hIndex.setTipAtHeight(3))
	tip, tipHeight, err := hIndex.chainTip()
	require.NoError(t, err)
	require.EqualValues(t, 3, tipHeight)
	require.Equal(t, only, *tip)

	err = hIndex.setTipAtHeight(5)
	require.ErrorIs(t, err, ErrHeaderStoreCorrupt)
	require.ErrorContains(t, err, "multiple headers")

	err = hIndex.setTipAtHeight(9)
	require.ErrorIs(t, err, ErrHeaderStoreCorrupt)
	require.ErrorIs(t, err, ErrHeightNotFound)

	// A failed attempt leaves the tip where it was.
	_, tipHeight, err = hIndex.chainTip()
	require.NoError(t, err)
	require.EqualValues(t, 3, tipHeight)
}
