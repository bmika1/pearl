package neutrino

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pearl-research-labs/pearl/node/chaincfg"
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
