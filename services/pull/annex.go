// Copyright 2025 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package pull

import (
	"context"
	"io"
	"sync"

	"forgejo.org/modules/annex"
	"forgejo.org/modules/git/pipeline"
)

// AnnexPush copies all annexed files referenced in new commits from the head repository to the base repository
func AnnexPush(ctx context.Context, tmpBasePath, mergeHeadSHA, mergeBaseSHA string) error {
	// Initialize the temporary repository with git-annex
	if err := annex.PrivateInit(ctx, tmpBasePath); err != nil {
		return err
	}

	revListReader, revListWriter := io.Pipe()
	shasToCheckReader, shasToCheckWriter := io.Pipe()
	catFileCheckReader, catFileCheckWriter := io.Pipe()
	shasToBatchReader, shasToBatchWriter := io.Pipe()
	lookupKeyBatchReader, lookupKeyBatchWriter := io.Pipe()
	errChan := make(chan error, 1)
	wg := sync.WaitGroup{}
	wg.Add(6)
	// Create the go-routines in reverse order.

	// 6. Take the referenced keys and copy their data from the head repository to
	// the base repository
	go annex.CopyFromToBatch(ctx, "head_repo", "origin", lookupKeyBatchReader, &wg, tmpBasePath)

	// 5. Take the shas of the blobs and resolve them to annex keys, git-annex
	// should filter out anything that doesn't reference a key
	go annex.LookupKeyBatch(ctx, shasToBatchReader, lookupKeyBatchWriter, &wg, tmpBasePath)

	// 4. From the provided objects restrict to blobs <=32KiB
	go pipeline.BlobsLessThanOrEqual32KiBFromCatFileBatchCheck(catFileCheckReader, shasToBatchWriter, &wg)

	// 3. Run batch-check on the objects retrieved from rev-list
	go pipeline.CatFileBatchCheck(ctx, shasToCheckReader, catFileCheckWriter, &wg, tmpBasePath)

	// 2. Check each object retrieved rejecting those without names as they will be commits or trees
	go pipeline.BlobsFromRevListObjects(revListReader, shasToCheckWriter, &wg)

	// 1. Run rev-list objects from mergeHead to mergeBase
	go pipeline.RevListObjects(ctx, revListWriter, &wg, tmpBasePath, mergeHeadSHA, mergeBaseSHA, errChan)

	wg.Wait()
	select {
	case err, has := <-errChan:
		if has {
			return err
		}
	default:
	}
	return nil
}
