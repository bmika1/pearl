// Copyright (c) 2025-2026 The Pearl Research Labs
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pearl-research-labs/pearl/spv/headerfs"
)

// neutrinoDBName is the database of the SPV chain service. It holds the header
// index that belongs to the flat header files.
const neutrinoDBName = "neutrino.db"

// syncDataFiles are the files in a network directory that hold chain data
// downloaded from the network and nothing else. The database goes last: without
// the flat files the header stores restart from genesis on their own, so a
// failure part-way through still leaves a state that loads.
var syncDataFiles = []string{
	headerfs.BlockHeadersFileName,
	headerfs.RegFilterHeadersFileName,
	neutrinoDBName,
}

// movedFile is a file that was renamed by moveAsideSyncData.
type movedFile struct {
	from, to string
}

// isSyncDataCorrupt reports whether err from creating the chain service says
// the stored chain data is unusable, so that discarding it can help.
func isSyncDataCorrupt(err error) bool {
	var (
		notFound *headerfs.ErrHeaderNotFound
		pathErr  *fs.PathError
	)

	return errors.Is(err, headerfs.ErrHeaderStoreCorrupt) ||
		errors.As(err, &notFound) ||
		(errors.As(err, &pathErr) && pathErr.Op == "truncate")
}

// syncDataReset allows the chain sync data to be reset once per process. The
// reset can't help when the cause is something else, which must not turn into
// a loop of resets.
type syncDataReset struct {
	done bool

	// rename is os.Rename unless replaced.
	rename func(oldpath, newpath string) error
}

// try resets the chain sync data in netDir if cause calls for it and it hasn't
// been reset before, and reports whether it did. A reset that failed lost
// nothing, so it may be tried again.
func (r *syncDataReset) try(netDir string, cause error) bool {
	if r.done || !isSyncDataCorrupt(cause) {
		return false
	}

	rename := r.rename
	if rename == nil {
		rename = os.Rename
	}
	if !resetSyncData(netDir, cause, rename) {
		return false
	}
	r.done = true

	return true
}

// moveAsideSyncData renames the chain sync files in netDir to
// <name>.corrupt-<unix time> instead of deleting them. Missing files are
// skipped and the first failure stops the move, so later files are left as
// they were. No other file in netDir is touched, in particular not the wallet
// database.
func moveAsideSyncData(netDir string, now time.Time,
	rename func(oldpath, newpath string) error) ([]movedFile, error) {

	var moved []movedFile
	for _, name := range syncDataFiles {
		from := filepath.Join(netDir, name)
		if _, err := os.Lstat(from); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return moved, err
		}

		to, err := quarantinePath(from, now)
		if err != nil {
			return moved, err
		}
		if err := rename(from, to); err != nil {
			return moved, err
		}

		moved = append(moved, movedFile{from: from, to: to})
	}

	return moved, nil
}

// quarantinePath returns the unused path to move path to.
func quarantinePath(path string, now time.Time) (string, error) {
	base := fmt.Sprintf("%s.corrupt-%d", path, now.Unix())

	candidate := base
	for i := 1; ; i++ {
		_, err := os.Lstat(candidate)
		if errors.Is(err, fs.ErrNotExist) {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}

		candidate = fmt.Sprintf("%s-%d", base, i)
	}
}

// resetSyncData moves the chain sync files in netDir aside so the next chain
// service starts from scratch. It reports whether that worked.
func resetSyncData(netDir string, cause error,
	rename func(oldpath, newpath string) error) bool {

	log.Warnf("The chain sync data in %s is corrupt: %v", netDir, cause)
	log.Warnf("Resetting the chain sync data: %s are moved aside and the "+
		"chain is synced again from scratch. The wallet database is not "+
		"touched.", strings.Join(syncDataFiles, ", "))

	moved, err := moveAsideSyncData(netDir, time.Now(), rename)
	for _, file := range moved {
		log.Infof("Moved %s to %s", file.from, file.to)
	}
	if err != nil {
		log.Errorf("Unable to reset the chain sync data: %v", err)
		return false
	}

	return true
}
