package neutrino

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pearl-research-labs/pearl/node/chaincfg"
	"github.com/pearl-research-labs/pearl/node/chaincfg/chainhash"
	"github.com/pearl-research-labs/pearl/node/wire"
	"github.com/pearl-research-labs/pearl/spv/headerfs"
	"github.com/pearl-research-labs/pearl/wallet/walletdb"
	_ "github.com/pearl-research-labs/pearl/wallet/walletdb/bdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openHeaderFiles lists the flat header files this process holds open in dir.
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

// TestChainServiceReleasesHeaderFiles makes sure the header files are closed
// when the service stops or fails to start, since an open file can't be moved
// aside on Windows.
func TestChainServiceReleasesHeaderFiles(t *testing.T) {
	dir := t.TempDir()
	db, err := walletdb.Create(
		"bdb", filepath.Join(dir, "neutrino.db"), true,
		10*time.Second, false,
	)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	cfg := Config{
		DataDir: dir, Database: db, ChainParams: chaincfg.SimNetParams,
	}

	cs, err := NewChainService(cfg)
	require.NoError(t, err)
	require.NoError(t, cs.Start(context.Background()))
	require.ElementsMatch(
		t, []string{
			headerfs.BlockHeadersFileName,
			headerfs.RegFilterHeadersFileName,
		}, openHeaderFiles(t, dir),
	)

	require.NoError(t, cs.Stop())
	require.Empty(t, openHeaderFiles(t, dir))

	// With the filter header tip gone the filter headers can't be matched
	// to the index, which the block headers, opened before them, mustn't
	// outlive.
	err = walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		rootBucket := tx.ReadWriteBucket([]byte("header-index"))

		return rootBucket.Delete([]byte("regular"))
	})
	require.NoError(t, err)

	_, err = NewChainService(cfg)
	require.ErrorIs(t, err, headerfs.ErrHeaderStoreCorrupt)
	require.Empty(t, openHeaderFiles(t, dir))
}

// TestChainServiceStartsWithShortHeaderFiles is the scenario behind the chain
// service failing to start for good: the header files lost their tail while
// the database kept the full chain.
func TestChainServiceStartsWithShortHeaderFiles(t *testing.T) {
	const (
		numHeaders  = 10
		lostBlocks  = 3
		lostFilters = 1
	)

	dir := t.TempDir()
	db, err := walletdb.Create(
		"bdb", filepath.Join(dir, "neutrino.db"), true,
		10*time.Second, false,
	)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	cfg := Config{
		DataDir: dir, Database: db, ChainParams: chaincfg.SimNetParams,
	}

	blockStore, err := headerfs.NewBlockHeaderStore(
		dir, db, &chaincfg.SimNetParams,
	)
	require.NoError(t, err)
	filterStore, err := headerfs.NewFilterHeaderStore(
		dir, db, headerfs.RegularFilter, &chaincfg.SimNetParams, nil,
	)
	require.NoError(t, err)

	prev := chaincfg.SimNetParams.GenesisBlock.BlockHeader()
	blocks := make([]headerfs.BlockHeader, numHeaders)
	filters := make([]headerfs.FilterHeader, numHeaders)
	for i := range blocks {
		header := &wire.BlockHeader{
			Bits:      uint32(i),
			Timestamp: prev.Timestamp.Add(time.Minute),
			PrevBlock: prev.BlockHash(),
		}
		blocks[i] = headerfs.BlockHeader{
			BlockHeader: header, Height: uint32(i + 1),
		}
		filters[i] = headerfs.FilterHeader{
			HeaderHash: header.BlockHash(),
			FilterHash: chainhash.DoubleHashH([]byte{byte(i)}),
			Height:     uint32(i + 1),
		}
		prev = header
	}
	require.NoError(t, blockStore.WriteHeaders(blocks...))
	require.NoError(t, filterStore.WriteHeaders(filters...))
	require.NoError(t, blockStore.(io.Closer).Close())
	require.NoError(t, filterStore.(io.Closer).Close())

	shrink := func(name string, numBytes int64) {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		require.NoError(t, err)
		require.NoError(t, os.Truncate(path, info.Size()-numBytes))
	}
	shrink(headerfs.BlockHeadersFileName, lostBlocks*headerfs.BlockHeaderSize)
	shrink(
		headerfs.RegFilterHeadersFileName,
		lostFilters*headerfs.RegularFilterHeaderSize,
	)

	cs, err := NewChainService(cfg)
	require.NoError(t, err)
	require.NoError(t, cs.Start(context.Background()))
	t.Cleanup(func() { assert.NoError(t, cs.Stop()) })

	_, blockHeight, err := cs.BlockHeaders.ChainTip()
	require.NoError(t, err)
	require.EqualValues(t, numHeaders-lostBlocks, blockHeight)

	filterTip, filterHeight, err := cs.RegFilterHeaders.ChainTip()
	require.NoError(t, err)
	require.EqualValues(t, numHeaders-lostBlocks, filterHeight)
	require.Equal(t, filters[numHeaders-lostBlocks-1].FilterHash, *filterTip)

	best, err := cs.BestBlock()
	require.NoError(t, err)
	require.EqualValues(t, numHeaders-lostBlocks, best.Height)
}
