// Copyright 2022 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Unlike modules/lfs, which operates mainly on git.Blobs, this operates on git.TreeEntrys.
// The motivation for this is that TreeEntrys have an easy pointer to the on-disk repo path,
// while blobs do not (in fact, if building with TAGS=gogit, blobs might exist only in a mock
// filesystem, living only in process RAM). We must have the on-disk path to do anything
// useful with git-annex because all of its interesting data is on-disk under .git/annex/.

package annex

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"forgejo.org/modules/git"
	"forgejo.org/modules/setting"
)

// ErrBlobIsNotAnnexed occurs if a blob does not contain a valid annex key
var ErrBlobIsNotAnnexed = errors.New("not a git-annex pointer")

func LookupKey(blob *git.Blob) (string, error) {
	stdout, _, err := git.NewCommand(git.DefaultContext, "annex", "lookupkey", "--ref").AddDynamicArguments(blob.ID.String()).RunStdString(&git.RunOpts{Dir: blob.Repo().Path})
	if err != nil {
		return "", ErrBlobIsNotAnnexed
	}
	key := strings.TrimSpace(stdout)
	return key, nil
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

// IsAnnexRepo determines if repo is a git-annex enabled repository
func IsAnnexRepo(repo *git.Repository) bool {
	_, _, err := git.NewCommand(repo.Ctx, "config", "annex.uuid").RunStdString(&git.RunOpts{Dir: repo.Path})
	return err == nil
}

var repoConfigFileRe = regexp.MustCompile("[^/]+/[^/]+.git/config$")

var (
	uuid2repoPathCache = make(map[string]string)
	repoPath2uuidCache = make(map[string]string)
)

func updateUUID2RepoPathCache() error {
	return filepath.WalkDir(setting.RepoRootPath, func(path string, d fs.DirEntry, err error) error {
		if err == nil && repoConfigFileRe.MatchString(path) {
			thisRepoPath := strings.TrimSuffix(path, "/config")
			_, ok := repoPath2uuidCache[thisRepoPath]
			if ok {
				return nil
			}
			stdout, _, err := git.NewCommand(git.DefaultContext, "config", "annex.uuid").RunStdString(&git.RunOpts{Dir: thisRepoPath})
			if err != nil {
				return nil
			}
			repoUUID := strings.TrimSpace(stdout)
			if repoUUID != "" {
				uuid2repoPathCache[repoUUID] = thisRepoPath
				repoPath2uuidCache[thisRepoPath] = repoUUID
			}
		}
		return nil
	})
}

func UUID2RepoPath(uuid string) (string, error) {
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
