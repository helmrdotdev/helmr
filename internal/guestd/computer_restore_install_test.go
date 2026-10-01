package guestd

import (
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"google.golang.org/protobuf/proto"
	"testing"
	"time"
)

func restoreInstallFixture(t *testing.T) (*computerOperationRegistry, *waitingRunRegistry, *computerv0.ComputerRestoreInstallation) {
	r, w, _, q := restoreSetFixture(t)
	if _, err := r.materializeRestoredComputerMount(q, w); err != nil {
		t.Fatal(err)
	}
	install := &computerv0.ComputerRestoreInstallation{Envelope: proto.Clone(q.Envelope).(*computerv0.ComputerOperationEnvelope), CheckpointId: q.RestoredCheckpointId, DesiredVersion: 3}
	for _, m := range r.captureRequest.Runs {
		grant := testComputerRunAuthority(time.Now().Add(time.Minute))
		grant.ChannelCredential = q.Envelope.ChannelCredential
		f := grant.Fence
		f.ComputerId = q.Envelope.ComputerId
		f.ComputerInstanceId = q.Envelope.ComputerInstanceId
		f.WriterGeneration = int64(q.Envelope.WriterGeneration)
		f.RunId = m.RunId
		f.AttemptNumber = m.AttemptNumber
		f.RunLeaseId = "new-" + m.RunLeaseId
		install.Grants = append(install.Grants, grant)
	}
	return r, w, install
}
func TestRestoreInstallIsAtomicAndActivationIsSeparate(t *testing.T) {
	r, w, q := restoreInstallFixture(t)
	if err := r.installComputerRestore(w, q, true, time.Now); err == nil {
		t.Fatal("activation before installation accepted")
	}
	bad := proto.Clone(q).(*computerv0.ComputerRestoreInstallation)
	bad.Grants[1].Fence.WriterGeneration--
	if err := r.installComputerRestore(w, bad, false, time.Now); err == nil {
		t.Fatal("invalid peer grant installed")
	}
	for _, c := range r.programClaims {
		if c.authority.Fence.ComputerInstanceId != "source-instance" {
			t.Fatal("partial install")
		}
	}
	for range 2 {
		if err := r.installComputerRestore(w, q, false, time.Now); err != nil {
			t.Fatal(err)
		}
	}
	if !r.captureSealed() {
		t.Fatal("installation opened admission before CP acknowledgement")
	}
	for _, slot := range w.slots {
		if !slot.frozen || slot.granted != nil {
			t.Fatal("installation resumed user code")
		}
	}
	for range 2 {
		if err := r.installComputerRestore(w, q, true, time.Now); err != nil {
			t.Fatal(err)
		}
	}
	if r.captureSealed() {
		t.Fatal("activation did not open admission")
	}
	for _, slot := range w.slots {
		if !slot.frozen || slot.granted != nil {
			t.Fatal("activation delivered individual resume")
		}
	}
	changed := proto.Clone(q).(*computerv0.ComputerRestoreInstallation)
	changed.DesiredVersion++
	if err := r.installComputerRestore(w, changed, true, time.Now); err == nil {
		t.Fatal("changed activation replay accepted")
	}
}
func TestRestoreInstallRejectsIncompleteOrExpiredSet(t *testing.T) {
	for _, name := range []string{"missing", "duplicate", "lease", "expired"} {
		t.Run(name, func(t *testing.T) {
			r, w, q := restoreInstallFixture(t)
			switch name {
			case "missing":
				q.Grants = q.Grants[:1]
			case "duplicate":
				q.Grants[1] = q.Grants[0]
			case "lease":
				q.Grants[1].Fence.RunLeaseId = "lease-b"
			case "expired":
				q.Grants[1].Fence.ExpiresAtUnixNano = time.Now().Add(-time.Second).UnixNano()
			}
			if err := r.installComputerRestore(w, q, false, time.Now); err == nil {
				t.Fatal("unsafe set installed")
			}
			if r.restoreInstallation != nil || !r.captureSealed() {
				t.Fatal("failed installation changed barrier or receipt")
			}
		})
	}
}

func TestEmptyRestoreInstallationAndActivation(t *testing.T) {
	r, w, q := restoreInstallFixture(t)
	r.programClaims = nil
	r.captureRequest.Runs = nil
	w.slots = map[string]*waitingRunSlot{}
	q.Grants = nil
	if err := r.installComputerRestore(w, q, false, time.Now); err != nil {
		t.Fatal(err)
	}
	if !r.captureSealed() {
		t.Fatal("empty installation bypassed acknowledgement")
	}
	if err := r.installComputerRestore(w, q, true, time.Now); err != nil {
		t.Fatal(err)
	}
	if r.captureSealed() {
		t.Fatal("empty activation remained sealed")
	}
}

func TestRestoreActivationRejectsExpiredInstalledGrant(t *testing.T) {
	r, w, q := restoreInstallFixture(t)
	if err := r.installComputerRestore(w, q, false, time.Now); err != nil {
		t.Fatal(err)
	}
	expired := func() time.Time { return time.Unix(0, q.Grants[0].Fence.ExpiresAtUnixNano).Add(time.Second) }
	if err := r.installComputerRestore(w, q, true, expired); err == nil {
		t.Fatal("expired installation activated")
	}
	if !r.captureSealed() || r.restoreActivated {
		t.Fatal("expired installation opened admission")
	}
	q.Grants[0].WriteCapability = "mutated-caller"
	if r.restoreInstallation.Grants[0].WriteCapability == "mutated-caller" {
		t.Fatal("installation receipt aliases caller")
	}
}
