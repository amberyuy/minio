/*
 * MinIO Cloud Storage, (C) 2019 MinIO, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"context"
	"runtime"
	"sync"
	"time"

	"github.com/minio/minio/cmd/logger"
	"github.com/minio/minio/pkg/madmin"
)

const (
	bgHealingUUID = "0000-0000-0000-0000"
)

// NewBgHealSequence creates a background healing sequence
// operation which crawls all objects and heal them.
func newBgHealSequence() *healSequence {
	reqInfo := &logger.ReqInfo{API: "BackgroundHeal"}
	ctx, cancelCtx := context.WithCancel(logger.SetReqInfo(GlobalContext, reqInfo))

	hs := madmin.HealOpts{
		// Remove objects that do not have read-quorum
		Remove:   true,
		ScanMode: madmin.HealNormalScan,
	}

	return &healSequence{
		sourceCh:    make(chan healSource, runtime.GOMAXPROCS(0)),
		respCh:      make(chan healResult, runtime.GOMAXPROCS(0)),
		startTime:   UTCNow(),
		clientToken: bgHealingUUID,
		// run-background heal with reserved bucket
		bucket:   minioReservedBucket,
		settings: hs,
		currentStatus: healSequenceStatus{
			Summary:      healNotStartedStatus,
			HealSettings: hs,
		},
		cancelCtx:          cancelCtx,
		ctx:                ctx,
		reportProgress:     false,
		scannedItemsMap:    make(map[madmin.HealItemType]int64),
		healedItemsMap:     make(map[madmin.HealItemType]int64),
		healFailedItemsMap: make(map[string]int64),
	}
}

func getLocalBackgroundHealStatus() (madmin.BgHealState, bool) {
	if globalBackgroundHealState == nil {
		return madmin.BgHealState{}, false
	}

	bgSeq, ok := globalBackgroundHealState.getHealSequenceByToken(bgHealingUUID)
	if !ok {
		return madmin.BgHealState{}, false
	}

	var healDisksMap = map[string]struct{}{}
	for _, ep := range getLocalDisksToHeal() {
		healDisksMap[ep.String()] = struct{}{}
	}

	for _, ep := range globalBackgroundHealState.getHealLocalDisks() {
		if _, ok := healDisksMap[ep.String()]; !ok {
			healDisksMap[ep.String()] = struct{}{}
		}
	}

	var healDisks []string
	for disk := range healDisksMap {
		healDisks = append(healDisks, disk)
	}

	return madmin.BgHealState{
		ScannedItemsCount: bgSeq.getScannedItemsCount(),
		LastHealActivity:  bgSeq.lastHealActivity,
		HealDisks:         healDisks,
		NextHealRound:     UTCNow(),
	}, true
}

// healErasureSet lists and heals all objects in a specific erasure set
func healErasureSet(ctx context.Context, prefix string, setIndex int, maxIO int, maxSleep time.Duration, buckets []BucketInfo, disks []StorageAPI) error {
	// Get background heal sequence to send elements to heal
	var bgSeq *healSequence
	var ok bool
	for {
		bgSeq, ok = globalBackgroundHealState.getHealSequenceByToken(bgHealingUUID)
		if ok {
			break
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
			continue
		}
	}

	obj := newObjectLayerFn()
	if obj == nil {
		return errServerNotInitialized
	}

	setDriveCount := obj.SetDriveCount()

	// Try to pro-actively heal backend-encrypted file.
	bgSeq.sourceCh <- healSource{
		bucket: minioMetaBucket,
		object: backendEncryptedFile,
	}

	// Heal config prefix.
	bgSeq.sourceCh <- healSource{
		bucket: pathJoin(minioMetaBucket, minioConfigPrefix),
	}

	bgSeq.sourceCh <- healSource{
		bucket: pathJoin(minioMetaBucket, bucketConfigPrefix),
	}

	// Heal all buckets with all objects
	var wg sync.WaitGroup
	for _, bucket := range buckets {
		wg.Add(1)
		go func(bucket BucketInfo, disks []StorageAPI) {
			defer wg.Done()

			// Heal current bucket
			bgSeq.sourceCh <- healSource{
				bucket: bucket.Name,
			}

			var entryChs []FileInfoVersionsCh
			var mu sync.Mutex
			var wwg sync.WaitGroup
			for _, disk := range disks {
				wwg.Add(1)
				go func(disk StorageAPI) {
					defer wwg.Done()
					if disk == nil {
						// disk is nil and not available.
						return
					}
					entryCh, err := disk.WalkVersions(ctx, bucket.Name, prefix, "", true, ctx.Done())
					if err != nil {
						// Disk walk returned error, ignore it.
						return
					}
					mu.Lock()
					entryChs = append(entryChs, FileInfoVersionsCh{
						Ch:       entryCh,
						SetIndex: setIndex,
					})
					mu.Unlock()
				}(disk)
			}
			wwg.Wait()

			entriesValid := make([]bool, len(entryChs))
			entries := make([]FileInfoVersions, len(entryChs))

			for {
				entry, quorumCount, ok := lexicallySortedEntryVersions(entryChs, entries, entriesValid)
				if !ok {
					logger.Info("Healing finished for bucket '%s' on erasure set %d", bucket.Name, setIndex+1)
					// We are finished with this bucket return.
					return
				}

				if quorumCount == setDriveCount {
					continue
				}

				for _, version := range entry.Versions {
					hsrc := healSource{
						bucket:    bucket.Name,
						object:    version.Name,
						versionID: version.VersionID,
					}
					hsrc.throttle.maxIO = maxIO
					hsrc.throttle.maxSleep = maxSleep
					if err := bgSeq.queueHealTask(ctx, hsrc, madmin.HealItemObject); err != nil {
						if !isErrObjectNotFound(err) && !isErrVersionNotFound(err) {
							logger.LogIf(ctx, err)
						}
					}
				}
			}
		}(bucket, disks)
	}
	wg.Wait()

	return nil
}

// deepHealObject heals given object path in deep to fix bitrot.
func deepHealObject(bucket, object, versionID string) {
	// Get background heal sequence to send elements to heal
	bgSeq, ok := globalBackgroundHealState.getHealSequenceByToken(bgHealingUUID)
	if ok {
		bgSeq.sourceCh <- healSource{
			bucket:    bucket,
			object:    object,
			versionID: versionID,
			opts:      &madmin.HealOpts{ScanMode: madmin.HealDeepScan},
		}
	}
}
