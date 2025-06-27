// Copyright 2022 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Unlike modules/lfs, which operates mainly on git.Blobs, this operates on git.TreeEntrys.
// The motivation for this is that TreeEntrys have an easy pointer to the on-disk repo path,
// while blobs do not (in fact, if building with TAGS=gogit, blobs might exist only in a mock
// filesystem, living only in process RAM). We must have the on-disk path to do anything
// useful with git-annex because all of its interesting data is on-disk under .git/annex/.

package annex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"forgejo.org/modules/git"
	"forgejo.org/modules/log"
	"forgejo.org/modules/setting"
	"forgejo.org/modules/typesniffer"

	"gopkg.in/ini.v1" //nolint:depguard // This import is forbidden in favor of using the setting module, but we need ini parsing for something other than Forgejo settings
)

// ErrBlobIsNotAnnexed occurs if a blob does not contain a valid annex key
var ErrBlobIsNotAnnexed = errors.New("not a git-annex pointer")

func PrivateInit(ctx context.Context, repoPath string) error {
	if _, _, err := git.NewCommand(ctx, "config", "annex.private", "true").RunStdString(&git.RunOpts{Dir: repoPath}); err != nil {
		return err
	}
	if _, _, err := git.NewCommand(ctx, "annex", "init").RunStdString(&git.RunOpts{Dir: repoPath}); err != nil {
		return err
	}
	return nil
}

func LookupKey(blob *git.Blob) (string, error) {
	stdout, _, err := git.NewCommand(git.DefaultContext, "annex", "lookupkey", "--ref").AddDynamicArguments(blob.ID.String()).RunStdString(&git.RunOpts{Dir: blob.Repo().Path})
	if err != nil {
		return "", ErrBlobIsNotAnnexed
	}
	key := strings.TrimSpace(stdout)
	return key, nil
}

// LookupKeyBatch runs git annex lookupkey --batch --ref
func LookupKeyBatch(ctx context.Context, shasToBatchReader *io.PipeReader, lookupKeyBatchWriter *io.PipeWriter, wg *sync.WaitGroup, repoPath string) {
	defer wg.Done()
	defer shasToBatchReader.Close()
	defer lookupKeyBatchWriter.Close()

	stderr := new(bytes.Buffer)
	var errbuf strings.Builder
	if err := git.NewCommand(ctx, "annex", "lookupkey", "--batch", "--ref").Run(&git.RunOpts{
		Dir:    repoPath,
		Stdout: lookupKeyBatchWriter,
		Stdin:  shasToBatchReader,
		Stderr: stderr,
	}); err != nil {
		_ = lookupKeyBatchWriter.CloseWithError(fmt.Errorf("git annex lookupkey --batch --ref [%s]: %w - %s", repoPath, err, errbuf.String()))
	}
}

// CopyFromToBatch runs git -c annex.hardlink=true annex copy --batch-keys --from <remote> --to <remote>
func CopyFromToBatch(ctx context.Context, from, to string, keysToCopyReader *io.PipeReader, wg *sync.WaitGroup, repoPath string) {
	defer wg.Done()
	defer keysToCopyReader.Close()

	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	var errbuf strings.Builder
	if err := git.NewCommand(ctx, "-c", "annex.hardlink=true", "annex", "copy", "--batch-keys", "--from").AddDynamicArguments(from).AddArguments("--to").AddDynamicArguments(to).Run(&git.RunOpts{
		Dir:    repoPath,
		Stdout: stdout,
		Stdin:  keysToCopyReader,
		Stderr: stderr,
	}); err != nil {
		_ = keysToCopyReader.CloseWithError(fmt.Errorf("git annex copy --batch-keys --from <remote> --to <remote> [%s]: %w - %s", repoPath, err, errbuf.String()))
	}
}

func ContentLocationFromKey(repoPath, key string) (string, error) {
	contentLocation, _, err := git.NewCommandContextNoGlobals(git.DefaultContext, "annex", "contentlocation").AddDynamicArguments(key).RunStdString(&git.RunOpts{Dir: repoPath})
	if err != nil {
		return "", fmt.Errorf("in %s: %s does not seem to be a valid annexed file: %w", repoPath, key, err)
	}
	contentLocation = strings.TrimSpace(contentLocation)
	contentLocation = path.Clean("/" + contentLocation)[1:] // prevent directory traversals
	contentLocation = path.Join(repoPath, contentLocation)

	return contentLocation, nil
}

// return the absolute path of the content pointed to by the annex pointer stored in the git object
// errors if the content is not found in this repo
func ContentLocation(blob *git.Blob) (string, error) {
	key, err := LookupKey(blob)
	if err != nil {
		return "", err
	}
	return ContentLocationFromKey(blob.Repo().Path, key)
}

// returns a stream open to the annex content
func Content(blob *git.Blob) (*os.File, error) {
	contentLocation, err := ContentLocation(blob)
	if err != nil {
		return nil, err
	}

	return os.Open(contentLocation)
}

// whether the object appears to be a valid annex pointer
// does *not* verify if the content is actually in this repo;
// for that, use ContentLocation()
func IsAnnexed(blob *git.Blob) (bool, error) {
	if !setting.Annex.Enabled {
		return false, nil
	}

	// LookupKey is written to only return well-formed keys
	// so the test is just to see if it errors
	_, err := LookupKey(blob)
	if err != nil {
		if errors.Is(err, ErrBlobIsNotAnnexed) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// PathIsAnnexRepo determines if repoPath is a git-annex enabled repository
func PathIsAnnexRepo(repoPath string) bool {
	_, _, err := git.NewCommand(git.DefaultContext, "config", "annex.uuid").RunStdString(&git.RunOpts{Dir: repoPath})
	return err == nil
}

// IsAnnexRepo determines if repo is a git-annex enabled repository
func IsAnnexRepo(repo *git.Repository) bool {
	_, _, err := git.NewCommand(repo.Ctx, "config", "annex.uuid").RunStdString(&git.RunOpts{Dir: repo.Path})
	return err == nil
}

var uuid2repoPathCache = make(map[string]string)

func Init() error {
	if !setting.Annex.Enabled {
		return nil
	}
	if !setting.Annex.DisableP2PHTTP {
		log.Info("Populating the git-annex UUID cache with existing repositories")
		start := time.Now()
		if err := updateUUID2RepoPathCache(); err != nil {
			return err
		}
		log.Info("Populating the git-annex UUID cache took %v", time.Since(start))
	}
	return nil
}

func updateUUID2RepoPathCache() error {
	configFiles, err := filepath.Glob(filepath.Join(setting.RepoRootPath, "*", "*", "config"))
	if err != nil {
		return err
	}
	for _, configFile := range configFiles {
		repoPath := strings.TrimSuffix(configFile, "/config")
		config, err := ini.Load(configFile)
		if err != nil {
			continue
		}
		repoUUID := config.Section("annex").Key("uuid").Value()
		if repoUUID != "" {
			uuid2repoPathCache[repoUUID] = repoPath
		}
	}
	return nil
}

func repoPathFromUUIDCache(uuid string) (string, error) {
	if repoPath, ok := uuid2repoPathCache[uuid]; ok {
		return repoPath, nil
	}
	// If the cache didn't contain an entry for the UUID then update the cache and try again
	if err := updateUUID2RepoPathCache(); err != nil {
		return "", err
	}
	if repoPath, ok := uuid2repoPathCache[uuid]; ok {
		return repoPath, nil
	}
	return "", fmt.Errorf("no repository known for UUID '%s'", uuid)
}

func checkValidity(uuid, repoPath string) (bool, error) {
	stdout, _, err := git.NewCommand(git.DefaultContext, "config", "annex.uuid").RunStdString(&git.RunOpts{Dir: repoPath})
	if err != nil {
		return false, err
	}
	repoUUID := strings.TrimSpace(stdout)
	return uuid == repoUUID, nil
}

func UUID2RepoPath(uuid string) (string, error) {
	// Get the current cache entry for the UUID
	repoPath, err := repoPathFromUUIDCache(uuid)
	if err != nil {
		return "", err
	}
	// Check if it is still up-to-date
	valid, _ := checkValidity(uuid, repoPath)
	if !valid {
		// If it isn't, remove the cache entry and try again
		delete(uuid2repoPathCache, uuid)
		return UUID2RepoPath(uuid)
	}
	// Otherwise just return the cached entry
	return repoPath, nil
}

// GuessContentType guesses the content type of the annexed blob.
func GuessContentType(blob *git.Blob) (typesniffer.SniffedType, error) {
	r, err := Content(blob)
	if err != nil {
		return typesniffer.SniffedType{}, err
	}
	defer r.Close()

	return typesniffer.DetectContentTypeFromReader(r)
}
