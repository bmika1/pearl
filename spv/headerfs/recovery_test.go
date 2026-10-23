package headerfs

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/pearl-research-labs/pearl/node/chaincfg"
	"github.com/pearl-research-labs/pearl/wallet/walletdb"
	"github.com/stretchr/testify/require"
)

func fileSize(t *testing.T, path string) int64 {
	t.Helper()

	info, err := os.Stat(path)
	require.NoError(t, err)

	return info.Size()
}

// resize cuts -delta bytes off the end of the file, or appends delta bytes of
// junk.
func resize(t *testing.T, path string, delta int64) {
	t.Helper()

	if delta <= 0 {
		require.NoError(t, os.Truncate(path, fileSize(t, path)+delta))
		return
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	defer f.Close()
	_, err = f.Write(bytes.Repeat([]byte{0xab}, int(delta)))
	require.NoError(t, err)
}

// preloadBlockIndex adds the block index entries the filter headers refer to,
// as the block header store would.
func preloadBlockIndex(t *testing.T, db walletdb.DB, chain []FilterHeader) {
	t.Helper()

	batch := headerBatch{{hash: *chaincfg.SimNetParams.GenesisHash}}
	for _, header := range chain {
		batch = append(batch, header.toIndexEntry())
	}
	blockIndex := &headerIndex{db: db, indexType: Block}
	require.NoError(t, blockIndex.addHeaders(batch))
}

// TestBlockHeaderStoreReconcile reopens block header stores whose file and
// index disagree the ways a crash leaves them.
func TestBlockHeaderStoreReconcile(t *testing.T) {
	const numHeaders = 10

	tests := []struct {
		name       string
		delta      int64
		wantHeight uint32
	}{
		{"consistent", 0, numHeaders},
		{"index ahead of file", -3 * BlockHeaderSize, numHeaders - 3},
		{"file empty", -(numHeaders + 1) * BlockHeaderSize, 0},
		{"partial trailing record", BlockHeaderSize / 2, numHeaders},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cleanUp, db, dir, bhs, err := createTestBlockHeaderStore()
			require.NoError(t, err)
			defer cleanUp()

			chain := createTestBlockHeaderChain(numHeaders + 2)
			require.NoError(t, bhs.WriteHeaders(chain[:numHeaders]...))
			require.NoError(t, bhs.Close())

			path := filepath.Join(dir, BlockHeadersFileName)
			resize(t, path, test.delta)

			store, err := NewBlockHeaderStore(
				dir, db, &chaincfg.SimNetParams,
			)
			require.NoError(t, err)

			_, tipHeight, err := store.ChainTip()
			require.NoError(t, err)
			require.Equal(t, test.wantHeight, tipHeight)
			require.EqualValues(
				t, (test.wantHeight+1)*BlockHeaderSize,
				fileSize(t, path),
			)

			for _, header := range chain[test.wantHeight:] {
				hash := header.BlockHash()
				_, err := store.HeightFromHash(&hash)
				require.ErrorIs(t, err, ErrHashNotFound)
			}

			// The store carries on from its tip.
			require.NoError(t, store.WriteHeaders(
				chain[test.wantHeight:]...,
			))
			_, tipHeight, err = store.ChainTip()
			require.NoError(t, err)
			require.EqualValues(t, numHeaders+2, tipHeight)
			require.NoError(
				t, store.(*blockHeaderStore).CheckConnectivity(),
			)
		})
	}
}

// TestFilterHeaderStoreReconcile is the filter header counterpart of
// TestBlockHeaderStoreReconcile.
func TestFilterHeaderStoreReconcile(t *testing.T) {
	const numHeaders = 10

	tests := []struct {
		name       string
		delta      int64
		wantHeight uint32
	}{
		{"consistent", 0, numHeaders},
		{"index ahead of file", -3 * RegularFilterHeaderSize, numHeaders - 3},
		{"file empty", -(numHeaders + 1) * RegularFilterHeaderSize, 0},
		{"partial trailing record", RegularFilterHeaderSize / 2, numHeaders},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cleanUp, db, dir, fhs, err := createTestFilterHeaderStore()
			require.NoError(t, err)
			defer cleanUp()

			chain := createTestFilterHeaderChain(numHeaders)
			preloadBlockIndex(t, db, chain)
			require.NoError(t, fhs.WriteHeaders(chain...))
			genesis, err := fhs.FetchHeaderByHeight(0)
			require.NoError(t, err)
			require.NoError(t, fhs.(*filterHeaderStore).Close())

			path := filepath.Join(dir, RegFilterHeadersFileName)
			resize(t, path, test.delta)

			store, err := NewFilterHeaderStore(
				dir, db, RegularFilter, &chaincfg.SimNetParams, nil,
			)
			require.NoError(t, err)

			wantTip := *genesis
			if test.wantHeight > 0 {
				wantTip = chain[test.wantHeight-1].FilterHash
			}
			tip, tipHeight, err := store.ChainTip()
			require.NoError(t, err)
			require.Equal(t, test.wantHeight, tipHeight)
			require.Equal(t, wantTip, *tip)
			require.EqualValues(
				t, (test.wantHeight+1)*RegularFilterHeaderSize,
				fileSize(t, path),
			)

			require.NoError(t, store.WriteHeaders(
				chain[test.wantHeight:]...,
			))
			tip, _, err = store.ChainTip()
			require.NoError(t, err)
			require.Equal(t, chain[numHeaders-1].FilterHash, *tip)
		})
	}
}

// TestHeaderStoreCorrupt covers states that can't be reconciled.
func TestHeaderStoreCorrupt(t *testing.T) {
	newBlockStore := func(t *testing.T) (walletdb.DB, string) {
		cleanUp, db, dir, bhs, err := createTestBlockHeaderStore()
		require.NoError(t, err)
		t.Cleanup(cleanUp)

		require.NoError(t, bhs.WriteHeaders(createTestBlockHeaderChain(5)...))
		require.NoError(t, bhs.Close())

		return db, dir
	}

	t.Run("block header index lost", func(t *testing.T) {
		db, dir := newBlockStore(t)
		err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
			return tx.DeleteTopLevelBucket(indexBucket)
		})
		require.NoError(t, err)

		_, err = NewBlockHeaderStore(dir, db, &chaincfg.SimNetParams)
		require.ErrorIs(t, err, ErrHeaderStoreCorrupt)
	})

	t.Run("block header tip differs from the file", func(t *testing.T) {
		db, dir := newBlockStore(t)
		path := filepath.Join(dir, BlockHeadersFileName)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		data[len(data)-1] ^= 0xff
		require.NoError(t, os.WriteFile(path, data, 0o644))

		_, err = NewBlockHeaderStore(dir, db, &chaincfg.SimNetParams)
		require.ErrorIs(t, err, ErrHeaderStoreCorrupt)
	})

	t.Run("filter header tip lost", func(t *testing.T) {
		cleanUp, db, dir, fhs, err := createTestFilterHeaderStore()
		require.NoError(t, err)
		defer cleanUp()

		chain := createTestFilterHeaderChain(5)
		preloadBlockIndex(t, db, chain)
		require.NoError(t, fhs.WriteHeaders(chain...))
		require.NoError(t, fhs.(*filterHeaderStore).Close())
		err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
			return tx.ReadWriteBucket(indexBucket).Delete(regFilterTip)
		})
		require.NoError(t, err)

		_, err = NewFilterHeaderStore(
			dir, db, RegularFilter, &chaincfg.SimNetParams, nil,
		)
		require.ErrorIs(t, err, ErrHeaderStoreCorrupt)
	})
}

// TestWriteHeadersSyncsBeforeIndexCommit makes sure the header file is on disk
// before the index refers to its contents.
func TestWriteHeadersSyncsBeforeIndexCommit(t *testing.T) {
	cleanUp, _, _, bhs, err := createTestBlockHeaderStore()
	require.NoError(t, err)
	defer cleanUp()

	syncs := 0
	bhs.file = &mockFile{
		File: bhs.file.(*os.File),
		syncFn: func() error {
			syncs++
			_, tipHeight, err := bhs.chainTip()
			require.NoError(t, err)
			require.Zero(t, tipHeight)

			return nil
		},
	}

	require.NoError(t, bhs.WriteHeaders(createTestBlockHeaderChain(10)...))
	require.Equal(t, 1, syncs)
}
