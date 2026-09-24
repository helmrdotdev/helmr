package computer

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"os"
)

// GenerationReuse confirms an already certified object with the exact inspection
// in the same retained Computer scope. Missing local bytes alone never establish
// remote durability. Implementations must reject absent or uncertified objects,
// validate source authority and retain references through publication.
type GenerationReuse interface {
	Reuse(context.Context, blockformat.ObjectInspection) error
}

func generationObjectLocal(ctx context.Context, local *cas.File, digest [32]byte, size int64) (bool, error) {
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
func publishGeneration(ctx context.Context, local *cas.File, source blockformat.RangeSource, scope string, keys map[string][]byte, root blockformat.Locator, capacity int64, maxObjects int, publisher GenerationPublication, reuse GenerationReuse) (blockformat.Locator, error) {
	fail := blockformat.Locator{}
	if publisher == nil || maxObjects <= 0 || maxObjects > 1<<20 {
		return fail, errors.New("bounded generation publisher required")
	}
	packs := make(map[blockformat.PackRef]bool)
	segments := make(map[blockformat.Ref]bool)
	remaining := maxObjects
	charge := func() error {
		remaining--
		if remaining < 0 {
			return errors.New("generation publication object budget exceeded")
		}
		return ctx.Err()
	}
	publish := func(e blockformat.ObjectInspection, digest [32]byte, size int64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := publisher.Register(ctx, e); err != nil {
			return err
		}
		descriptor := cas.Descriptor{Digest: sha256sum.FormatDigest(digest[:]), SizeBytes: size, MediaType: "application/octet-stream"}
		file, err := local.OpenImmutable(ctx, descriptor)
		if err != nil {
			return err
		}
		object, uploadErr := publisher.Upload(ctx, descriptor, file)
		err = errors.Join(uploadErr, file.Close())
		if err != nil {
			return err
		}
		if object.Digest != descriptor.Digest || object.SizeBytes != descriptor.SizeBytes || object.MediaType != descriptor.MediaType {
			return errors.New("uploaded generation object differs from candidate")
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		return publisher.Certify(ctx, e)
	}
	var visit func(blockformat.PackRef, []blockformat.NodeReference) error
	visit = func(ref blockformat.PackRef, expected []blockformat.NodeReference) error {
		// Authenticate each incoming position even for an already published pack.
		inspected, err := blockformat.InspectPack(ctx, source, scope, keys, ref)
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
		if reuse != nil {
			exists, err := generationObjectLocal(ctx, local, ref.Digest, ref.Size)
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
				if reuse != nil {
					exists, err := generationObjectLocal(ctx, local, segment.Digest, segment.Size)
					if err != nil {
						return err
					}
					if !exists {
						if err = reuse.Reuse(ctx, blockformat.ObjectInspection{Segment: &segment}); err != nil {
							return err
						}
						segments[segment] = true
						continue
					}
				}
				if err = blockformat.InspectSegment(ctx, local, scope, keys[segment.Key], segment); err != nil {
					return err
				}
				if err = publish(blockformat.ObjectInspection{Segment: &segment}, segment.Digest, segment.Size); err != nil {
					return err
				}
				segments[segment] = true
			}
			for _, child := range page.Children {
				if child.Locator.Pack.Rank >= ref.Rank {
					return errors.New("invalid generation dependency rank")
				}
				children[child.Locator.Pack] = append(children[child.Locator.Pack], child)
			}
		}
		for child, links := range children {
			if err = visit(child, links); err != nil {
				return err
			}
		}

		if err = publish(blockformat.ObjectInspection{Pack: &inspected}, ref.Digest, ref.Size); err != nil {
			return err
		}
		packs[ref] = true
		return nil
	}
	if err := visit(root.Pack, nil); err != nil {
		return fail, err
	}
	if err := ctx.Err(); err != nil {
		return fail, err
	}
	return root, nil
}
