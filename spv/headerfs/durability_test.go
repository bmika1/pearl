package headerfs

import (
	"errors"
	"os"
	"testing"

	"github.com/pearl-research-labs/pearl/node/chaincfg"
	"github.com/stretchr/testify/require"
)

// TestAppendRawPartialWriteAfterReopen makes sure undoing a failed append
// leaves the existing headers alone when the store was just opened, which is
// when the file position of an append-only file is still at the start.
func TestAppendRawPartialWriteAfterReopen(t *testing.T) {
	cleanUp, db, dir, bhs, err := createTestBlockHeaderStore()
	require.NoError(t, err)
	defer cleanUp()

	chain := createTestBlockHeaderChain(5)
	require.NoError(t, bhs.WriteHeaders(chain...))
	require.NoError(t, bhs.Close())

	store, err := NewBlockHeaderStore(dir, db, &chaincfg.SimNetParams)
	require.NoError(t, err)
	reopened := store.(*blockHeaderStore)

	reopened.file = &mockFile{
		File: reopened.file.(*os.File),
		writeFn: func(p []byte, file File) (int, error) {
			n, err := file.Write(p[:10])
			if err != nil {
				return n, err
			}

			return n, errors.New("disk full")
		},
	}

	more := createTestBlockHeaderChain(6)[5:]
	err = reopened.WriteHeaders(more...)
	require.ErrorContains(t, err, "disk full")
	require.EqualValues(
		t, 6*BlockHeaderSize, fileSize(t, headerFilePath(dir, Block)),
	)
}

// TestWriteHeadersSyncsBeforeIndexCommit makes sure the header file is on disk
// before the index starts to refer to its contents.
func TestWriteHeadersSyncsBeforeIndexCommit(t *testing.T) {
	t.Run("block headers", func(t *testing.T) {
		cleanUp, _, dir, bhs, err := createTestBlockHeaderStore()
		require.NoError(t, err)
		defer cleanUp()

		var syncs int
		bhs.file = &mockFile{
			File: bhs.file.(*os.File),
			syncFn: func() error {
				syncs++

				_, tipHeight, err := bhs.chainTip()
				require.NoError(t, err)
				require.Zero(t, tipHeight)
				require.EqualValues(
					t, 11*BlockHeaderSize,
					fileSize(t, headerFilePath(dir, Block)),
				)

				return nil
			},
		}

		require.NoError(t, bhs.WriteHeaders(
			createTestBlockHeaderChain(10)...,
		))
		require.Equal(t, 1, syncs)
	})

	t.Run("filter headers", func(t *testing.T) {
		cleanUp, db, dir, fhs, err := createTestFilterHeaderStore()
		require.NoError(t, err)
		defer cleanUp()

		chain := createTestFilterHeaderChain(10)
		preloadBlockIndex(t, db, chain)
		store := fhs.(*filterHeaderStore)

		var syncs int
		store.file = &mockFile{
			File: store.file.(*os.File),
			syncFn: func() error {
				syncs++

				_, tipHeight, err := store.chainTip()
				require.NoError(t, err)
				require.Zero(t, tipHeight)
				require.EqualValues(
					t, 11*RegularFilterHeaderSize,
					fileSize(t, headerFilePath(dir, RegularFilter)),
				)

				return nil
			},
		}

		require.NoError(t, store.WriteHeaders(chain...))
		require.Equal(t, 1, syncs)
	})
}

// TestWriteHeadersSyncFailure makes sure a header file that can't be synced is
// rolled back, so it never gets ahead of the index.
func TestWriteHeadersSyncFailure(t *testing.T) {
	cleanUp, _, dir, bhs, err := createTestBlockHeaderStore()
	require.NoError(t, err)
	defer cleanUp()

	bhs.file = &mockFile{
		File:   bhs.file.(*os.File),
		syncFn: func() error { return errors.New("io error") },
	}

	err = bhs.WriteHeaders(createTestBlockHeaderChain(10)...)
	require.ErrorContains(t, err, "io error")

	require.EqualValues(
		t, BlockHeaderSize, fileSize(t, headerFilePath(dir, Block)),
	)
	_, tipHeight, err := bhs.chainTip()
	require.NoError(t, err)
	require.Zero(t, tipHeight)
}
