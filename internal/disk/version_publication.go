package disk

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"golang.org/x/sync/errgroup"
	"os"
	"sort"
)

// Bound concurrent local file handles and provider requests. The SDK owns its
// connection pool, retries and throttling; no custom retry layer is added.
const versionUploadConcurrency = 8

// VersionReuse confirms an already certified object with the exact inspection
// in the same retained Computer scope. Missing local bytes alone never establish
// remote durability. Implementations must reject absent or uncertified objects,
// validate source authority and retain references through publication.
type VersionReuse interface {
	Reuse(context.Context, blockformat.ObjectInspection) error
}

func versionObjectLocal(ctx context.Context, local *cas.File, digest [32]byte, size int64) (bool, error) {
	file, err := local.OpenImmutable(ctx, cas.Descriptor{Digest: sha256sum.FormatDigest(digest[:]), SizeBytes: size, MediaType: "application/octet-stream"})
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, file.Close()
}

// Visit local dependencies child-first, authenticating every incoming pack
// position. A retained certified subtree can be reused at its boundary without
// reading its data or uploading it again. Publication never advances a head.
func publishVersion(ctx context.Context, local *cas.File, source blockformat.RangeSource, scope string, keys map[string][]byte, root blockformat.Locator, capacity int64, maxObjects int, publisher VersionPublication, reuse VersionReuse) (blockformat.Locator, error) {
	return publishVersionEvidence(ctx, local, source, scope, keys, root, capacity, maxObjects, publisher, reuse, nil)
}

func publishVersionEvidence(ctx context.Context, local *cas.File, source blockformat.RangeSource, scope string, keys map[string][]byte, root blockformat.Locator, capacity int64, maxObjects int, publisher VersionPublication, reuse VersionReuse, evidence *versionEvidenceCache) (blockformat.Locator, error) {
	fail := blockformat.Locator{}
	if publisher == nil || maxObjects <= 0 || maxObjects > 1<<20 {
		return fail, errors.New("bounded version publisher required")
	}
	packs := make(map[blockformat.PackRef]bool)
	segments := make(map[blockformat.Ref]bool)
	remaining := maxObjects
	charge := func() error {
		remaining--
		if remaining < 0 {
			return errors.New("version publication object budget exceeded")
		}
		return ctx.Err()
	}
	type pendingObject struct {
		inspection blockformat.ObjectInspection
		descriptor cas.Descriptor
	}
	var levels [7][]pendingObject
	enqueue := func(e blockformat.ObjectInspection, digest [32]byte, size int64) error {
		rank := 0
		if e.Pack != nil {
			rank = e.Pack.Pages[0].Locator.Pack.Rank
		}
		levels[rank] = append(levels[rank], pendingObject{e, cas.Descriptor{Digest: sha256sum.FormatDigest(digest[:]), SizeBytes: size, MediaType: "application/octet-stream"}})
		return nil
	}
	var visit func(blockformat.PackRef, []blockformat.NodeReference) error
	visit = func(ref blockformat.PackRef, expected []blockformat.NodeReference) error {
		// Authenticate each incoming position even for an already published pack.
		inspected, err := evidence.pack(ctx, source, scope, keys, ref, false)
		if err != nil {
			return err
		}
		if expected != nil {
			for _, link := range expected {
				if err = inspected.CheckNode(link); err != nil {
					return err
				}
			}
		} else {
			if err = inspected.CheckRoot(root, capacity); err != nil {
				return err
			}
		}
		if packs[ref] {
			return nil
		}
		if _, remote, ok := evidence.get(ref.Digest); ok && remote {
			packs[ref] = true
			return charge()
		}
		if reuse != nil {
			exists, err := versionObjectLocal(ctx, local, ref.Digest, ref.Size)
			if err != nil {
				return err
			}
			if !exists {
				if err = charge(); err != nil {
					return err
				}
				if err = reuse.Reuse(ctx, blockformat.ObjectInspection{Pack: &inspected}); err != nil {
					return err
				}
				evidence.put(blockformat.ObjectInspection{Pack: &inspected}, true)
				packs[ref] = true
				return nil
			}
		}
		if err = charge(); err != nil {
			return err
		}
		children := make(map[blockformat.PackRef][]blockformat.NodeReference)
		for _, page := range inspected.Pages {
			for _, segment := range page.Segments {
				if segments[segment] {
					continue
				}
				if err = charge(); err != nil {
					return err
				}
				if cached, remote, ok := evidence.get(segment.Digest); ok && remote {
					if cached.Segment == nil || *cached.Segment != segment {
						return errors.New("certified segment reference differs")
					}
					segments[segment] = true
					continue
				}
				if reuse != nil {
					exists, err := versionObjectLocal(ctx, local, segment.Digest, segment.Size)
					if err != nil {
						return err
					}
					if !exists {
						if err = reuse.Reuse(ctx, blockformat.ObjectInspection{Segment: &segment}); err != nil {
							return err
						}
						evidence.put(blockformat.ObjectInspection{Segment: &segment}, true)
						segments[segment] = true
						continue
					}
				}
				if err = evidence.segment(ctx, local, scope, keys[segment.Key], segment, false); err != nil {
					return err
				}
				if err = enqueue(blockformat.ObjectInspection{Segment: &segment}, segment.Digest, segment.Size); err != nil {
					return err
				}
				segments[segment] = true
			}
			for _, child := range page.Children {
				if child.Locator.Pack.Rank >= ref.Rank {
					return errors.New("invalid version dependency rank")
				}
				children[child.Locator.Pack] = append(children[child.Locator.Pack], child)
			}
		}
		for child, links := range children {
			if err = visit(child, links); err != nil {
				return err
			}
		}

		if err = enqueue(blockformat.ObjectInspection{Pack: &inspected}, ref.Digest, ref.Size); err != nil {
			return err
		}
		packs[ref] = true
		return nil
	}
	if err := visit(root.Pack, nil); err != nil {
		return fail, err
	}
	// A failed batch is retried with the same inspected identities. Register
	// and Certify commit atomically; upload successes before a failure remain
	// registered and can be reconciled by conditional PUT on the next attempt.
	for _, level := range levels {
		sort.Slice(level, func(i, j int) bool { return level[i].descriptor.Digest < level[j].descriptor.Digest })
		for len(level) > 0 {
			n, encodedBytes := 0, 0
			for n < len(level) && n < blockformat.MaxPublicationBatchObjects {
				raw, err := json.Marshal(level[n].inspection)
				if err != nil {
					return fail, err
				}
				if len(raw)+1 > blockformat.MaxPublicationBatchBytes {
					return fail, errors.New("inspection exceeds publication batch limit")
				}
				if n > 0 && encodedBytes+len(raw)+1 > blockformat.MaxPublicationBatchBytes {
					break
				}
				encodedBytes += len(raw) + 1
				n++
			}
			batch := level[:n]
			inspections := make([]blockformat.ObjectInspection, n)
			for i := range batch {
				inspections[i] = batch[i].inspection
			}
			if err := publisher.RegisterBatch(ctx, inspections); err != nil {
				return fail, err
			}
			group, uploadCtx := errgroup.WithContext(ctx)
			group.SetLimit(versionUploadConcurrency)
			for _, object := range batch {
				group.Go(func() error {
					file, err := local.OpenImmutable(uploadCtx, object.descriptor)
					if err != nil {
						return err
					}
					uploaded, uploadErr := publisher.Upload(uploadCtx, object.descriptor, file)
					if err = errors.Join(uploadErr, file.Close()); err != nil {
						return err
					}
					if uploaded.Digest != object.descriptor.Digest || uploaded.SizeBytes != object.descriptor.SizeBytes || uploaded.MediaType != object.descriptor.MediaType {
						return errors.New("uploaded version object differs from candidate")
					}
					return nil
				})
			}
			if err := group.Wait(); err != nil {
				return fail, err
			}
			if err := publisher.CertifyBatch(ctx, inspections); err != nil {
				return fail, err
			}
			for _, i := range inspections {
				evidence.put(i, true)
			}
			level = level[n:]
		}
	}
	if err := ctx.Err(); err != nil {
		return fail, err
	}
	return root, nil
}

// ContinuationPublication binds new and reused objects to one live publication owner.
type ContinuationPublication interface {
	VersionPublication
	VersionReuse
}
