package authority

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/dev/computer-block-proof/internal/generation"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/jackc/pgx/v5"
)

const inspectionObjects = 1000
const inspectionBytes = 16 << 20

func digestString(d [32]byte) string { return hex.EncodeToString(d[:]) }
func inspectedDescriptor(o generation.InspectedObject) object {
	return object{digestString(o.Digest), o.Kind, o.Rank, o.Size, o.Keys}
}
func inspectedManifest(i generation.Inspection) (manifest, error) {
	raw, err := json.Marshal(i.Root)
	if err != nil {
		return manifest{}, err
	}
	// The sealed request identity includes the entire locator, not merely its pack
	// digest. Two roots/pages in one pack must not share a publication identity.
	return manifest{string(raw), digestString(i.Root.Pack.Digest), i.Root.Offset, i.Capacity}, nil
}

// This local bridge owns verification-to-SQL ordering. Caller auth, provisioned
// keys and immutable object retention remain fixture premises. Do not expose the
// low-level SQL fixture operations as an untrusted caller's certification API.
func (s store) publishLocal(ctx context.Context, p publication, m manifest, c *generation.Codec, local *generation.Local) (string, error) {
	// A successful receipt is authoritative even if its original source store is
	// no longer accessible. Check exact identity before reopening or re-certifying.
	var receipt string
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		status, err := samePublication(ctx, tx, p)
		if err != nil {
			return err
		}
		if err = sameManifest(ctx, tx, p, m); err != nil {
			return err
		}
		if status == "published" {
			return tx.QueryRow(ctx, `SELECT result_id FROM computer_publications WHERE environment_id=$1 AND id=$2`, p.Env, p.ID).Scan(&receipt)
		}
		if status != "registered" {
			return errConflict
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if receipt != "" {
		return receipt, nil
	}
	// Caller keys remain pre-provisioned fixture inputs, but their scope and
	// active writer must match the durable publication owner.
	var org string
	if err := s.pool.QueryRow(ctx, `SELECT org_id FROM environments WHERE id=$1`, p.Env).Scan(&org); err != nil {
		return "", err
	}
	scope, err := computer.EncryptionScope(org, p.Env, p.Computer)
	if err != nil {
		return "", err
	}
	var writeKey string
	if err := s.pool.QueryRow(ctx, `SELECT write_key_id FROM computer_publications WHERE environment_id=$1 AND id=$2`, p.Env, p.ID).Scan(&writeKey); err != nil {
		return "", err
	}
	if c.Scope != scope || c.ActiveKey != writeKey {
		return "", errConflict
	}

	root, data, packs, err := local.Reopen(c, inspectionObjects, inspectionBytes)
	if err != nil {
		return "", err
	}
	verified, err := generation.Inspect(c, data, packs, root, inspectionObjects, inspectionBytes)
	if err != nil {
		return "", err
	}
	bound, err := inspectedManifest(verified)
	if err != nil {
		return "", err
	}
	if m != bound {
		return "", errConflict
	}
	// Only byte-derived edges reach SQL. The inventory used for admission grants
	// no certification and is not consulted for the dependency list here.
	for _, o := range verified.Objects {
		children := make([]string, len(o.Children))
		for i, d := range o.Children {
			children[i] = digestString(d)
		}
		if err = s.certify(ctx, p, inspectedDescriptor(o), children); err != nil {
			return "", err
		}
	}
	return s.publish(ctx, p, bound)
}

type encryptedFixture struct {
	codec       *generation.Codec
	data, packs *generation.Store
	root        blockformat.Locator
	inventory   generation.Inspection
	local       *generation.Local
	dir         string
}

func encrypted(t *testing.T) *encryptedFixture {
	t.Helper()
	key := bytes.Repeat([]byte{0x37}, 32)
	scope, err := computer.EncryptionScope("org", "env", "computer")
	if err != nil {
		t.Fatal(err)
	}
	c, err := generation.NewCodec(scope, "1", map[string][]byte{"1": key})
	if err != nil {
		t.Fatal(err)
	}
	data, packs := generation.NewStore(), generation.NewStore()
	root, err := generation.NewPacked(c, packs, 128*blockformat.BlockSize, 64, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	root, err = generation.CapturePacked(c, data, packs, root, map[uint64][]byte{0: bytes.Repeat([]byte{7}, blockformat.BlockSize), 65: bytes.Repeat([]byte{9}, blockformat.BlockSize)}, 1<<20, true)
	if err != nil {
		t.Fatal(err)
	}
	// Author-side inventory is used only to register descriptors before installing
	// objects. The publication bridge independently inspects the installed bytes.
	inventory, err := generation.Inspect(c, data, packs, root, inspectionObjects, inspectionBytes)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	local, err := generation.OpenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	return &encryptedFixture{c, data, packs, root, inventory, local, dir}
}
func (e *encryptedFixture) install(t *testing.T) {
	t.Helper()
	if err := e.local.Commit(e.codec, e.data, e.packs, e.root, inspectionObjects, inspectionBytes); err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) registerEncrypted(p publication, e *encryptedFixture, skip string) manifest {
	f.t.Helper()
	for _, o := range e.inventory.Objects {
		if digestString(o.Digest) != skip {
			f.ok(f.admit(f.ctx, p, inspectedDescriptor(o)))
		}
	}
	m, err := inspectedManifest(e.inventory)
	f.ok(err)
	f.ok(f.seal(f.ctx, p, m))
	return m
}
func (e *encryptedFixture) objectPath(kind string) string {
	for _, o := range e.inventory.Objects {
		if o.Kind == kind {
			return filepath.Join(e.dir, "objects", digestString(o.Digest))
		}
	}
	panic("fixture missing object kind " + kind)
}

func generationCases(test func(string, func(*fixture))) {
	test("encrypted generation publishes byte-derived graph and restores", func(f *fixture) {
		p, _ := f.candidate("encrypted")
		e := encrypted(f.t)
		m := f.registerEncrypted(p, e, "")
		e.install(f.t)
		result, err := f.publishLocal(f.ctx, p, m, e.codec, e.local)
		f.ok(err)
		f.assertCount(len(e.inventory.Objects), `SELECT count(*) FROM computer_objects WHERE environment_id='env' AND digest<>'seed-root' AND certified`)
		edges := 0
		for _, o := range e.inventory.Objects {
			edges += len(o.Children)
		}
		f.assertCount(edges, `SELECT count(*) FROM computer_object_edges WHERE environment_id='env'`)
		for _, o := range e.inventory.Objects {
			for _, child := range o.Children {
				f.assertCount(1, `SELECT count(*) FROM computer_object_edges WHERE environment_id='env' AND parent_digest=$1 AND child_digest=$2`, digestString(o.Digest), digestString(child))
			}
		}

		var stored manifest
		f.ok(f.pool.QueryRow(f.ctx, `SELECT p.manifest,r.digest,r.page_offset,r.capacity FROM computer_version_roots r JOIN computer_publications p ON p.environment_id=r.environment_id AND p.result_id=r.version_id WHERE r.environment_id='env' AND r.version_id=$1`, result).Scan(&stored.ID, &stored.Root, &stored.Offset, &stored.Capacity))
		if stored != m {
			f.t.Fatal("persisted selection differs")
		}
		root, data, packs, err := e.local.Reopen(e.codec, inspectionObjects, inspectionBytes)
		f.ok(err)
		var selected blockformat.Locator
		f.ok(json.Unmarshal([]byte(stored.ID), &selected))
		if selected != root {
			f.t.Fatal("stored locator differs")
		}
		for block, want := range map[uint64]byte{0: 7, 65: 9, 1: 0} {
			got, err := generation.ReadPacked(e.codec, data, packs, selected, block)
			f.ok(err)
			if !bytes.Equal(got, bytes.Repeat([]byte{want}, blockformat.BlockSize)) {
				f.t.Fatalf("restored block %d", block)
			}
		}
		// A later generation shares physical children, while the old retained root
		// still restores its own bytes after the head has moved.
		next := p
		next.ID = "encrypted-next"
		next.Source = result
		f.ok(f.begin(f.ctx, next))
		nextRoot, err := generation.CapturePacked(e.codec, e.data, e.packs, e.root, map[uint64][]byte{0: bytes.Repeat([]byte{4}, blockformat.BlockSize)}, 1<<20, true)
		f.ok(err)
		e.root = nextRoot
		e.inventory, err = generation.Inspect(e.codec, e.data, e.packs, e.root, inspectionObjects, inspectionBytes)
		f.ok(err)
		nm := f.registerEncrypted(next, e, "")
		e.install(f.t)
		_, err = f.publishLocal(f.ctx, next, nm, e.codec, e.local)
		f.ok(err)
		old, err := generation.ReadPacked(e.codec, e.data, e.packs, selected, 0)
		f.ok(err)
		if old[0] != 7 {
			f.t.Fatal("old generation changed")
		}
		current, err := generation.ReadPacked(e.codec, e.data, e.packs, nextRoot, 0)
		f.ok(err)
		if current[0] != 4 {
			f.t.Fatal("new generation missing change")
		}
		f.sql(`UPDATE computers SET epoch=2 WHERE environment_id='env'`)
		f.ok(os.Remove(filepath.Join(e.dir, "root")))
		replay, err := f.publishLocal(f.ctx, p, m, e.codec, e.local)
		f.ok(err)
		if replay != result {
			f.t.Fatal("verified publication replay changed receipt")
		}
		changed := m
		changed.Offset++
		if _, err = f.publishLocal(f.ctx, p, changed, e.codec, e.local); !errors.Is(err, errConflict) {
			f.t.Fatalf("changed receipt replay: %v", err)
		}

	})
	for _, mode := range []string{"missing segment", "corrupt segment", "missing index", "corrupt root", "wrong key", "wrong scope", "invalid locator", "wrong capacity", "unregistered child", "abandoned"} {
		test("encrypted publication rejects "+mode, func(f *fixture) {
			p, _ := f.candidate("encrypted")
			e := encrypted(f.t)
			skip := ""
			if mode == "unregistered child" {
				for _, o := range e.inventory.Objects {
					if o.Kind == "segment" {
						skip = digestString(o.Digest)
						break
					}
				}
			}
			m := f.registerEncrypted(p, e, skip)
			e.install(f.t)
			switch mode {
			case "missing segment":
				f.ok(os.Remove(e.objectPath("segment")))
			case "missing index":
				f.ok(os.Remove(e.objectPath("index")))
			case "corrupt segment", "corrupt root":
				kind := "segment"
				if mode == "corrupt root" {
					kind = "root"
				}
				path := e.objectPath(kind)
				raw, err := os.ReadFile(path)
				f.ok(err)
				raw[len(raw)-1] ^= 1
				f.ok(os.WriteFile(path, raw, 0600))
			case "wrong key":
				var err error
				e.codec, err = generation.NewCodec("env", "1", map[string][]byte{"1": bytes.Repeat([]byte{0x38}, 32)})
				f.ok(err)
			case "wrong scope":
				var err error
				e.codec, err = generation.NewCodec("other", "1", map[string][]byte{"1": bytes.Repeat([]byte{0x37}, 32)})
				f.ok(err)
			case "invalid locator":
				bad := e.root
				bad.Offset++
				raw, err := json.Marshal(bad)
				f.ok(err)
				f.ok(os.WriteFile(filepath.Join(e.dir, "root"), raw, 0600))
			case "wrong capacity":
				// Model a request sealed with a false capacity, not just a changed replay.
				f.sql(`UPDATE computer_publications SET capacity=capacity*2 WHERE environment_id=$1 AND id=$2`, p.Env, p.ID)
				m.Capacity *= 2
			case "abandoned":
				f.ok(f.abandon(f.ctx, p))
			}
			result, err := f.publishLocal(f.ctx, p, m, e.codec, e.local)
			if err == nil || result != "" {
				f.t.Fatalf("invalid generation published: %s %v", result, err)
			}
			if mode == "wrong capacity" && !errors.Is(err, errConflict) {
				f.t.Fatalf("capacity binding: %v", err)
			}
			f.assertCount(1, `SELECT count(*) FROM computers WHERE environment_id='env' AND head_id='version-seed'`)
			f.assertCount(0, `SELECT count(*) FROM computer_versions WHERE id='version-encrypted'`)
			f.assertCount(0, `SELECT count(*) FROM computer_publications WHERE id='encrypted' AND status='published'`)
			if mode != "unregistered child" {
				f.assertCount(0, `SELECT count(*) FROM computer_objects WHERE digest<>'seed-root' AND certified`)
			}
		})
	}
}
