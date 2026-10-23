// Copyright (c) 2025-2026 The Pearl Research Labs
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/pearl-research-labs/pearl/spv/headerfs"
	"github.com/pearl-research-labs/pearl/wallet/chain"
	"github.com/pearl-research-labs/pearl/wallet/internal/cfgutil"
	"github.com/pearl-research-labs/pearl/wallet/netparams"
	"github.com/pearl-research-labs/pearl/wallet/waddrmgr"
	"github.com/pearl-research-labs/pearl/wallet/wallet"
	"github.com/pearl-research-labs/pearl/wallet/walletdb"
	"github.com/stretchr/testify/require"
)

var silenceLogsOnce sync.Once

// silenceLogs gives the logger a pipe to write to, as it panics without one.
func silenceLogs() {
	silenceLogsOnce.Do(func() {
		pr, pw := io.Pipe()
		go func() { _, _ = io.Copy(io.Discard, pr) }()
		logRotatorPipe = pw
	})
}

// useTestChain points the global config at a simnet wallet directory and
// returns its network directory.
func useTestChain(t *testing.T, appData string,
	dbTimeout time.Duration) string {

	t.Helper()
	silenceLogs()

	prevCfg, prevNet := cfg, activeNet
	cfg = &config{
		UseSPV:     true,
		DBTimeout:  dbTimeout,
		AppDataDir: cfgutil.NewExplicitString(appData),
	}
	activeNet = &netparams.SimNetParams
	t.Cleanup(func() { cfg, activeNet = prevCfg, prevNet })

	netDir := networkDir(appData, activeNet.Params)
	require.NoError(t, os.MkdirAll(netDir, 0o700))

	return netDir
}

// openSyncFiles lists the chain sync files in dir this process holds open.
func openSyncFiles(t *testing.T, dir string) []string {
	t.Helper()

	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("needs /proc to list open files")
	}

	var open []string
	for _, fd := range fds {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name()))
		if err == nil && filepath.Dir(target) == dir &&
			slices.Contains(syncDataFiles, filepath.Base(target)) {

			open = append(open, filepath.Base(target))
		}
	}

	return open
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()

	for name, content := range files {
		err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600)
		require.NoError(t, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(path)
	require.NoError(t, err)

	return string(content)
}

func TestMoveAsideSyncData(t *testing.T) {
	netDir := t.TempDir()
	files := map[string]string{
		neutrinoDBName:                      "neutrino",
		headerfs.BlockHeadersFileName:       "blocks",
		headerfs.RegFilterHeadersFileName:   "filters",
		"wallet.db":                         "wallet",
		"wallet.db.bak":                     "backup",
		"peers.json":                        "peers",
		"neutrino.db.corrupt-1":             "older",
		headerfs.BlockHeadersFileName + "~": "editor leftover",
	}
	writeFiles(t, netDir, files)

	moved, err := moveAsideSyncData(
		netDir, time.Unix(1700000000, 0), os.Rename,
	)
	require.NoError(t, err)

	const suffix = ".corrupt-1700000000"
	var want []movedFile
	for _, name := range []string{
		headerfs.BlockHeadersFileName, headerfs.RegFilterHeadersFileName,
		neutrinoDBName,
	} {
		want = append(want, movedFile{
			from: filepath.Join(netDir, name),
			to:   filepath.Join(netDir, name+suffix),
		})

		require.NoFileExists(t, filepath.Join(netDir, name))
		require.Equal(t, files[name], readFile(t, filepath.Join(netDir, name+suffix)))
	}
	require.Equal(t, want, moved)

	for _, name := range []string{
		"wallet.db", "wallet.db.bak", "peers.json", "neutrino.db.corrupt-1",
		headerfs.BlockHeadersFileName + "~",
	} {
		require.Equal(t, files[name], readFile(t, filepath.Join(netDir, name)))
	}

	entries, err := os.ReadDir(netDir)
	require.NoError(t, err)
	require.Len(t, entries, len(files))
}

func TestMoveAsideSyncDataSkipsMissingFiles(t *testing.T) {
	netDir := t.TempDir()
	writeFiles(t, netDir, map[string]string{
		neutrinoDBName: "neutrino",
		"wallet.db":    "wallet",
	})

	moved, err := moveAsideSyncData(netDir, time.Unix(5, 0), os.Rename)
	require.NoError(t, err)
	require.Len(t, moved, 1)
	require.Equal(t, filepath.Join(netDir, neutrinoDBName), moved[0].from)
	require.Equal(t, "wallet", readFile(t, filepath.Join(netDir, "wallet.db")))

	moved, err = moveAsideSyncData(t.TempDir(), time.Unix(5, 0), os.Rename)
	require.NoError(t, err)
	require.Empty(t, moved)
}

// TestMoveAsideSyncDataStopsAtFirstFailure checks the order the files are
// moved in, which keeps the directory loadable when a move fails.
func TestMoveAsideSyncDataStopsAtFirstFailure(t *testing.T) {
	netDir := t.TempDir()
	files := map[string]string{
		neutrinoDBName:                    "neutrino",
		headerfs.BlockHeadersFileName:     "blocks",
		headerfs.RegFilterHeadersFileName: "filters",
	}
	writeFiles(t, netDir, files)

	failOn := filepath.Join(netDir, headerfs.RegFilterHeadersFileName)
	rename := func(oldpath, newpath string) error {
		if oldpath == failOn {
			return errors.New("file in use")
		}

		return os.Rename(oldpath, newpath)
	}

	moved, err := moveAsideSyncData(netDir, time.Unix(7, 0), rename)
	require.ErrorContains(t, err, "file in use")
	require.Len(t, moved, 1)
	require.Equal(t, headerfs.BlockHeadersFileName+".corrupt-7",
		filepath.Base(moved[0].to))

	require.NoFileExists(t, filepath.Join(netDir, headerfs.BlockHeadersFileName))
	require.Equal(t, "filters", readFile(t, failOn))
	require.Equal(t, "neutrino", readFile(t, filepath.Join(netDir, neutrinoDBName)))
}

func TestMoveAsideSyncDataKeepsEarlierQuarantine(t *testing.T) {
	netDir := t.TempDir()
	writeFiles(t, netDir, map[string]string{
		neutrinoDBName:                  "new",
		neutrinoDBName + ".corrupt-9":   "first",
		neutrinoDBName + ".corrupt-9-1": "second",
	})

	moved, err := moveAsideSyncData(netDir, time.Unix(9, 0), os.Rename)
	require.NoError(t, err)
	require.Len(t, moved, 1)
	require.Equal(t, filepath.Join(netDir, neutrinoDBName+".corrupt-9-2"), moved[0].to)

	require.Equal(t, "new", readFile(t, moved[0].to))
	require.Equal(t, "first", readFile(t, filepath.Join(netDir, neutrinoDBName+".corrupt-9")))
	require.Equal(t, "second", readFile(t, filepath.Join(netDir, neutrinoDBName+".corrupt-9-1")))
}

func TestIsSyncDataCorrupt(t *testing.T) {
	corrupt := fmt.Errorf("%w: tip mismatch", headerfs.ErrHeaderStoreCorrupt)

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"header store corrupt", corrupt, true},
		{"wrapped header store corrupt", fmt.Errorf("start: %w", corrupt), true},
		{
			"header not found",
			fmt.Errorf("start: %w", &headerfs.ErrHeaderNotFound{}),
			true,
		},
		{
			"truncate failure",
			&fs.PathError{Op: "truncate", Path: "block_headers.bin", Err: errors.New("The parameter is incorrect.")},
			true,
		},
		{
			"other file failure",
			&fs.PathError{Op: "open", Path: "block_headers.bin", Err: fs.ErrPermission},
			false,
		},
		{"database timeout", errors.New("timeout"), false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, isSyncDataCorrupt(test.err))
		})
	}
}

func TestSyncDataResetOnlyOnce(t *testing.T) {
	silenceLogs()

	netDir := t.TempDir()
	writeSyncFiles := func() {
		writeFiles(t, netDir, map[string]string{
			neutrinoDBName:                    "neutrino",
			headerfs.BlockHeadersFileName:     "blocks",
			headerfs.RegFilterHeadersFileName: "filters",
		})
	}
	writeSyncFiles()

	corrupt := fmt.Errorf("%w: tip mismatch", headerfs.ErrHeaderStoreCorrupt)
	var reset syncDataReset

	// Other causes don't call for a reset.
	require.False(t, reset.try(netDir, errors.New("timeout")))
	require.FileExists(t, filepath.Join(netDir, neutrinoDBName))

	require.True(t, reset.try(netDir, corrupt))
	require.NoFileExists(t, filepath.Join(netDir, neutrinoDBName))

	// The files that show up again are left alone.
	writeSyncFiles()
	require.False(t, reset.try(netDir, corrupt))
	require.FileExists(t, filepath.Join(netDir, neutrinoDBName))
}

// TestSyncDataResetRetriedAfterFailure checks that a reset that couldn't move
// the files doesn't use up the one reset, since nothing was lost by it.
func TestSyncDataResetRetriedAfterFailure(t *testing.T) {
	silenceLogs()

	netDir := t.TempDir()
	writeFiles(t, netDir, map[string]string{
		neutrinoDBName:                "neutrino",
		headerfs.BlockHeadersFileName: "blocks",
	})

	corrupt := fmt.Errorf("%w: tip mismatch", headerfs.ErrHeaderStoreCorrupt)
	reset := syncDataReset{
		rename: func(string, string) error { return errors.New("file in use") },
	}

	require.False(t, reset.try(netDir, corrupt))
	require.FileExists(t, filepath.Join(netDir, neutrinoDBName))
	require.FileExists(t, filepath.Join(netDir, headerfs.BlockHeadersFileName))

	reset.rename = nil
	require.True(t, reset.try(netDir, corrupt))
	require.NoFileExists(t, filepath.Join(netDir, neutrinoDBName))
	require.False(t, reset.try(netDir, corrupt))
}

// TestStartSPVChainReleasesResources makes sure the chain database can be
// opened again right after the chain is closed, which is what happens when
// the next attempt follows.
func TestStartSPVChainReleasesResources(t *testing.T) {
	netDir := useTestChain(t, t.TempDir(), 500*time.Millisecond)

	for range 2 {
		client, spv, err := startSPVChain(context.Background(), netDir)
		require.NoError(t, err)
		require.NotNil(t, client)

		spv.close()
		spv.close()

		require.Empty(t, openSyncFiles(t, netDir))
	}
}

func TestStartSPVChainFailureReleasesResources(t *testing.T) {
	netDir := useTestChain(t, t.TempDir(), 500*time.Millisecond)

	// Headers without a database that knows about them can't be used.
	writeFiles(t, netDir, map[string]string{
		headerfs.BlockHeadersFileName: string(make([]byte, 5*headerfs.BlockHeaderSize)),
	})

	client, spv, err := startSPVChain(context.Background(), netDir)
	require.Nil(t, client)
	require.Nil(t, spv)
	require.ErrorIs(t, err, headerfs.ErrHeaderStoreCorrupt)
	require.True(t, isSyncDataCorrupt(err))

	require.Empty(t, openSyncFiles(t, netDir))
}

func runConnectLoop(ctx context.Context, loader *wallet.Loader) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		rpcClientConnectLoop(ctx, nil, loader)
	}()

	return done
}

func waitStopped(t *testing.T, done <-chan struct{}) {
	t.Helper()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("connect loop did not stop")
	}
}

// TestConnectLoopResetsCorruptSyncData runs the connect loop on chain data
// that can't be used, and checks that the loop moves exactly the chain sync
// files aside, starts over from scratch, and lets go of everything when it
// stops.
func TestConnectLoopResetsCorruptSyncData(t *testing.T) {
	netDir := useTestChain(t, t.TempDir(), 5*time.Second)

	zeros := string(make([]byte, 5*headerfs.BlockHeaderSize))
	writeFiles(t, netDir, map[string]string{
		headerfs.BlockHeadersFileName: zeros,
		"wallet.db":                   "wallet data",
		"notes.txt":                   "notes",
	})

	loader := wallet.NewLoader(activeNet.Params, netDir, true, cfg.DBTimeout, 250)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runConnectLoop(ctx, loader)

	// A fresh chain database holds just the genesis header.
	require.Eventually(t, func() bool {
		info, err := os.Stat(filepath.Join(netDir, headerfs.BlockHeadersFileName))

		return err == nil && info.Size() == headerfs.BlockHeaderSize
	}, 20*time.Second, 20*time.Millisecond)

	cancel()
	waitStopped(t, done)

	blocks, err := filepath.Glob(filepath.Join(
		netDir, headerfs.BlockHeadersFileName+".corrupt-*",
	))
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	require.Equal(t, zeros, readFile(t, blocks[0]))

	databases, err := filepath.Glob(filepath.Join(
		netDir, neutrinoDBName+".corrupt-*",
	))
	require.NoError(t, err)
	require.Len(t, databases, 1)

	// Nothing else was moved.
	quarantined, err := filepath.Glob(filepath.Join(netDir, "*.corrupt-*"))
	require.NoError(t, err)
	require.Len(t, quarantined, 2)

	require.Equal(t, "wallet data", readFile(t, filepath.Join(netDir, "wallet.db")))
	require.Equal(t, "notes", readFile(t, filepath.Join(netDir, "notes.txt")))
	require.FileExists(t, filepath.Join(netDir, neutrinoDBName))
	require.Empty(t, openSyncFiles(t, netDir))
}

// TestConnectLoopStopsWhileRetrying makes sure shutdown doesn't have to wait
// for the delay between attempts.
func TestConnectLoopStopsWhileRetrying(t *testing.T) {
	netDir := useTestChain(t, t.TempDir(), 50*time.Millisecond)

	prevMin, prevMax := chainClientRetryMin, chainClientRetryMax
	chainClientRetryMin, chainClientRetryMax = time.Minute, time.Minute
	t.Cleanup(func() { chainClientRetryMin, chainClientRetryMax = prevMin, prevMax })

	// Hold the chain database so every attempt to open it times out.
	held, err := walletdb.Create(
		"bdb", filepath.Join(netDir, neutrinoDBName), true,
		time.Second, false,
	)
	require.NoError(t, err)
	defer held.Close()

	loader := wallet.NewLoader(activeNet.Params, netDir, true, cfg.DBTimeout, 250)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runConnectLoop(ctx, loader)

	// Let the first attempt fail so the loop is waiting to try again.
	time.Sleep(300 * time.Millisecond)

	start := time.Now()
	cancel()
	waitStopped(t, done)
	require.Less(t, time.Since(start), 10*time.Second)
}

// TestConnectLoopReconnectsAfterClientStops makes sure a chain client that goes
// away is replaced by a new one on the same database, which used to fail with
// a timeout because the previous attempt never let go of the database.
func TestConnectLoopReconnectsAfterClientStops(t *testing.T) {
	netDir := useTestChain(t, t.TempDir(), 3*time.Second)

	prevScrypt := waddrmgr.DefaultScryptOptions
	waddrmgr.DefaultScryptOptions = waddrmgr.FastScryptOptions
	t.Cleanup(func() { waddrmgr.DefaultScryptOptions = prevScrypt })

	loader := wallet.NewLoader(activeNet.Params, netDir, true, cfg.DBTimeout, 250)
	w, err := loader.CreateNewWatchingOnlyWallet([]byte("public"), time.Now())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, loader.UnloadWallet()) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runConnectLoop(ctx, loader)

	chainClient := func(not chain.Interface) func() bool {
		return func() bool {
			current := w.ChainClient()

			return current != nil && current != not
		}
	}
	require.Eventually(t, chainClient(nil), 20*time.Second, 20*time.Millisecond)
	first := w.ChainClient()

	first.Stop()

	require.Eventually(t, chainClient(first), 20*time.Second, 20*time.Millisecond)

	cancel()
	waitStopped(t, done)
	require.Empty(t, openSyncFiles(t, netDir))
}
