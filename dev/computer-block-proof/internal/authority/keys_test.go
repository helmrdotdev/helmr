package authority

import (
	"bytes"
	"errors"

	"github.com/helmrdotdev/helmr/dev/computer-block-proof/internal/generation"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/jackc/pgx/v5"
)

func (f *fixture) addComputer(id string) {
	f.sql(`INSERT INTO computers(environment_id,id,epoch) VALUES('env',$1,1)`, id)
	f.sql(`INSERT INTO computer_keys(environment_id,computer_id,id,wrapped_key) VALUES('env',$1,'1',decode('01','hex'));`, id)
	f.sql(`UPDATE computers SET write_key_id='1' WHERE environment_id='env' AND id=$1`, id)
}
func (f *fixture) rotate(computer, key string) {
	f.ok(f.transaction(f.ctx, func(tx pgx.Tx) error {
		if err := exec(f.ctx, tx, `INSERT INTO computer_keys(environment_id,computer_id,id,wrapped_key) VALUES('env',$1,$2,decode('02','hex'))`, computer, key); err != nil {
			return err
		}
		return exec(f.ctx, tx, `UPDATE computers SET write_key_id=$2 WHERE environment_id='env' AND id=$1`, computer, key)
	}))
}

func computerKeyCases(test func(string, func(*fixture))) {
	test("same environment cannot attach another Computer's retained state", func(f *fixture) {
		f.addComputer("other")
		queries := []string{
			`UPDATE computers SET head_id='version-seed' WHERE environment_id='env' AND id='other'`,
			`INSERT INTO computer_versions VALUES('env','other','bad','version-seed')`,
			`INSERT INTO attempts(environment_id,computer_id,id,base_version) VALUES('env','other','bad','version-seed')`,
			`INSERT INTO waits(environment_id,computer_id,id,base_version,resume_version) VALUES('env','other','bad','version-seed','version-seed')`,
			`INSERT INTO checkpoints(environment_id,id,computer_id,epoch,capture_id,scratch,memory,runtime,metadata,version_id) VALUES('env','bad','other',1,'capture','s','m','r','i','version-seed')`,
		}
		for _, q := range queries {
			_, err := f.pool.Exec(f.ctx, q)
			requireState(f.t, err, "23503")
		}
		p := publication{Env: "env", Computer: "other", ID: "other-candidate", Epoch: 1}
		f.ok(f.begin(f.ctx, p))
		_, err := f.pool.Exec(f.ctx, `INSERT INTO computer_publication_objects VALUES('env','other','other-candidate','seed-root')`)
		requireState(f.t, err, "23503")
		f.ok(f.admit(f.ctx, p, object{"other-root", "root", 2, 128, []string{"1"}}))
		_, err = f.pool.Exec(f.ctx, `INSERT INTO computer_object_edges(environment_id,computer_id,parent_digest,child_digest,parent_rank,child_rank) VALUES('env','other','other-root','seed-root',2,1)`)
		requireState(f.t, err, "23503")
		// Even reuse of an identifier in another Computer cannot select the foreign payload.
		f.sql(`INSERT INTO computer_versions VALUES('env','other','version-seed',NULL)`)
		_, err = f.pool.Exec(f.ctx, `INSERT INTO computer_version_roots(environment_id,computer_id,version_id,digest,page_offset,capacity) VALUES('env','other','version-seed','seed-root',0,4096)`)
		requireState(f.t, err, "23503")
	})
	test("same-environment object owners and keys are independently retained", func(f *fixture) {
		f.addComputer("other")
		a, _ := f.candidate("first")
		b := publication{Env: "env", Computer: "other", ID: "second", Epoch: 1}
		f.ok(f.begin(f.ctx, b))
		o := object{"shared-digest", "segment", 0, 64, []string{"1"}}
		for _, p := range []publication{a, b} {
			f.ok(f.admit(f.ctx, p, o))
			f.ok(f.certify(f.ctx, p, o, nil))
		}
		f.ok(f.abandon(f.ctx, a))
		f.ok(f.collect(f.ctx, o.Digest, nil))
		f.assertCount(0, `SELECT count(*) FROM computer_objects WHERE environment_id='env' AND computer_id='computer' AND digest='shared-digest'`)
		f.assertCount(1, `SELECT count(*) FROM computer_objects WHERE environment_id='env' AND computer_id='other' AND digest='shared-digest' AND certified`)
		f.assertCount(1, `SELECT count(*) FROM computer_object_keys WHERE digest='shared-digest'`)
		f.ok(f.abandon(f.ctx, b))
		f.ok(f.collect(f.ctx, o.Digest, nil))
		f.assertCount(0, `SELECT count(*) FROM computer_object_keys WHERE digest='shared-digest'`)
		f.assertCount(1, `SELECT count(*) FROM cas_object_lifetimes WHERE digest='shared-digest' AND NOT available`)
	})

	test("Computer context rejects ciphertext even with identical fixture key bytes", func(f *fixture) {
		e := encrypted(f.t)
		e.install(f.t)
		scope, err := computer.EncryptionScope("org", "env", "other")
		f.ok(err)
		wrong, err := generation.NewCodec(scope, e.codec.ActiveKey, e.codec.Keys)
		f.ok(err)
		if _, _, _, err = e.local.Reopen(wrong, inspectionObjects, inspectionBytes); err == nil {
			f.t.Fatal("foreign Computer ciphertext restored")
		}
	})
	test("candidate pins key before ciphertext and survives rotation", func(f *fixture) {
		f.addComputer("empty")
		p := publication{Env: "env", Computer: "empty", ID: "before", Epoch: 1}
		f.ok(f.begin(f.ctx, p))
		f.rotate("empty", "2")
		f.ok(f.retireKey(f.ctx, "env", "empty", "1"))
		f.assertCount(1, `SELECT count(*) FROM computer_keys WHERE computer_id='empty' AND id='1' AND available`)
		f.assertCount(0, `SELECT count(*) FROM computer_objects WHERE computer_id='empty'`)
		// The already-admitted writer can still register its first ciphertext.
		o := object{"delayed", "segment", 0, 64, []string{"1"}}
		f.ok(f.admit(f.ctx, p, o))
		f.ok(f.certify(f.ctx, p, o, nil))
		next := p
		next.ID = "after"
		f.ok(f.begin(f.ctx, next))
		if err := f.admit(f.ctx, next, o); !errors.Is(err, errConflict) {
			f.t.Fatalf("new writer borrowed unretained key: %v", err)
		}
		f.ok(f.abandon(f.ctx, p))
		f.ok(f.retireKey(f.ctx, "env", "empty", "1"))
		f.assertCount(1, `SELECT count(*) FROM computer_keys WHERE computer_id='empty' AND id='1' AND available`)
		f.ok(f.collect(f.ctx, "delayed", nil))
		f.ok(f.retireKey(f.ctx, "env", "empty", "1"))
		f.assertCount(1, `SELECT count(*) FROM computer_keys WHERE computer_id='empty' AND id='1' AND NOT available AND wrapped_key IS NULL`)
		_, err := f.pool.Exec(f.ctx, `UPDATE computers SET write_key_id='1' WHERE id='empty'`)
		requireState(f.t, err, "23503")
		// Audit identity can outlive material; no insert can reuse the retired identifier.
		_, err = f.pool.Exec(f.ctx, `INSERT INTO computer_keys(environment_id,computer_id,id,wrapped_key) VALUES('env','empty','1',decode('01','hex'))`)
		requireState(f.t, err, "23505")
	})
	test("current writer key cannot retire without any ciphertext", func(f *fixture) {
		f.addComputer("empty")
		f.ok(f.retireKey(f.ctx, "env", "empty", "1"))
		f.assertCount(1, `SELECT count(*) FROM computer_keys WHERE computer_id='empty' AND available`)
		_, err := f.pool.Exec(f.ctx, `UPDATE computer_keys SET retired_at=now(),wrapped_key=NULL WHERE computer_id='empty'`)
		requireState(f.t, err, "23503")
	})
	test("key retirement waits for candidate abandonment transaction", func(f *fixture) {
		f.addComputer("empty")
		p := publication{Env: "env", Computer: "empty", ID: "waiting", Epoch: 1}
		f.ok(f.begin(f.ctx, p))
		f.rotate("empty", "2")
		tx := f.hold(`SELECT * FROM computer_keys WHERE computer_id='empty' AND id='1' FOR UPDATE`)
		f.ok(abandon(f.ctx, tx, p))
		// A raw competing retirement reaches the real FK barrier and cannot observe
		// candidate release before its transaction commits.
		done := runAsync(func() error {
			_, err := f.pool.Exec(f.ctx, `UPDATE computer_keys SET retired_at=now(),wrapped_key=NULL WHERE computer_id='empty' AND id='1'`)
			return err
		})
		f.waitBlocked(tx)
		f.ok(tx.Commit(f.ctx))
		f.ok(<-done)
		f.ok(f.retireKey(f.ctx, "env", "empty", "1"))
	})
	test("key dependencies cannot cross Computer identity", func(f *fixture) {
		f.addComputer("other")
		f.rotate("other", "foreign")
		_, err := f.pool.Exec(f.ctx, `INSERT INTO computer_object_keys(environment_id,computer_id,digest,key_id) VALUES('env','computer','seed-root','foreign')`)
		requireState(f.t, err, "23503")
		p, _ := f.candidate("unknown-key")
		if err = f.admit(f.ctx, p, object{"bad", "segment", 0, 64, []string{"foreign"}}); !errors.Is(err, errConflict) {
			f.t.Fatalf("foreign key admitted: %v", err)
		}
	})
	test("registered key set must match independently verified ciphertext", func(f *fixture) {
		p, _ := f.candidate("encrypted")
		e := encrypted(f.t)
		// Add an otherwise valid same-Computer key dependency to a declaration. The
		// actual byte inspection must reject even this conservative-looking mismatch.
		f.sql(`INSERT INTO computer_keys(environment_id,computer_id,id,wrapped_key) VALUES('env','computer','extra',decode('02','hex'))`)
		m := f.registerEncrypted(p, e, "")
		e.install(f.t)
		f.sql(`INSERT INTO computer_object_keys(environment_id,computer_id,digest,key_id) VALUES('env','computer',$1,'extra')`, digestString(e.inventory.Objects[0].Digest))
		if _, err := f.publishLocal(f.ctx, p, m, e.codec, e.local); !errors.Is(err, errConflict) {
			f.t.Fatalf("unverified key set published: %v", err)
		}
	})
	test("rotation preserves old generation and dependencies of new encrypted graph", func(f *fixture) {
		p, _ := f.candidate("encrypted")
		e := encrypted(f.t)
		m := f.registerEncrypted(p, e, "")
		e.install(f.t)
		source, err := f.publishLocal(f.ctx, p, m, e.codec, e.local)
		f.ok(err)
		oldRoot := e.root
		f.rotate("computer", "2")
		keys := map[string][]byte{"1": e.codec.Keys["1"], "2": bytes.Repeat([]byte{0x42}, 32)}
		e.codec, err = generation.NewCodec(e.codec.Scope, "2", keys)
		f.ok(err)
		e.root, err = generation.CapturePacked(e.codec, e.data, e.packs, e.root, map[uint64][]byte{1: bytes.Repeat([]byte{11}, blockformat.BlockSize)}, 1<<20, true)
		f.ok(err)
		e.inventory, err = generation.Inspect(e.codec, e.data, e.packs, e.root, inspectionObjects, inspectionBytes)
		f.ok(err)
		next := publication{Env: "env", Computer: "computer", ID: "rotated", Epoch: 1, Source: source}
		f.ok(f.begin(f.ctx, next))
		nm := f.registerEncrypted(next, e, "")
		e.install(f.t)
		_, err = f.publishLocal(f.ctx, next, nm, e.codec, e.local)
		f.ok(err)
		f.ok(f.retireKey(f.ctx, "env", "computer", "1"))
		f.assertCount(2, `SELECT count(*) FROM computer_keys WHERE environment_id='env' AND computer_id='computer' AND available`)
		got, err := generation.ReadPacked(e.codec, e.data, e.packs, oldRoot, 0)
		f.ok(err)
		if !bytes.Equal(got, bytes.Repeat([]byte{7}, blockformat.BlockSize)) {
			f.t.Fatal("old generation changed after rotation")
		}
		got, err = generation.ReadPacked(e.codec, e.data, e.packs, e.root, 1)
		f.ok(err)
		if !bytes.Equal(got, bytes.Repeat([]byte{11}, blockformat.BlockSize)) {
			f.t.Fatal("new generation lost write")
		}
		f.checkReadKeySummaries()
		f.assertCount(2, `SELECT count(DISTINCT key_id) FROM computer_object_keys WHERE environment_id='env' AND computer_id='computer'`)
	})
}
