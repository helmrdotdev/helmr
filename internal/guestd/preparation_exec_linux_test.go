//go:build linux

package guestd

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/definition"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	productversion "github.com/helmrdotdev/helmr/internal/version"
)

func TestPreparationExecutesAuthoredCodeAndJoinsDescendants(t *testing.T) {
	root := os.Getenv("HELMR_PREPARATION_TEST_ROOT")
	if root == "" || os.Getenv("HELMR_PRIVILEGED_PROGRAM_TEST") != "1" {
		t.Skip("requires disposable privileged Linux preparation fixture")
	}
	if os.Geteuid() != 0 {
		t.Fatal("preparation fixture requires root")
	}
	if _, err := os.Stat(processCgroupRoot); errors.Is(err, os.ErrNotExist) {
		prepareProgramTestCgroup(t)
	} else if err != nil {
		t.Fatal(err)
	}
	flags, err := artifact.NodeProgramFlags(productversion.Node())
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := artifact.CanonicalRuntimeMetadata(artifact.RuntimeMetadata{ModulePolicyDigest: "sha256:" + strings.Repeat("a", 64), Architecture: definition.ArchitectureX8664, FormatVersion: artifact.RuntimeMetadataFormatVersion, NodeVersion: productversion.Node(), ProgramNodeFlags: flags, RuntimeContract: definition.RuntimeContract})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(managedRuntimeMetadata, metadata, 0644); err != nil {
		t.Fatal(err)
	}
	program := bootProgramMounts().Artifact
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(program, "package.json"), `{"type":"module"}`)
	write(filepath.Join(program, "helmr/definition-index.json"), `{"apiVersion":"helmr.definition-index.v1","agents":[],"computers":[{"id":"repo","modulePath":"helmr/app/entry-0.mjs","exportName":"repo","throughAgent":false}]}`)
	write(filepath.Join(program, "helmr/app/entry-0.mjs"), `import fs from 'node:fs';import {spawn} from 'node:child_process';
export const repo={kind:'computer',id:'repo',async prepare(build){
 await build.exec([process.execPath,'-e',"require('node:fs').writeFileSync('/workspace/prepared',process.env.BUILD_TOKEN)"]);
 fs.writeFileSync('/workspace/prepared-file',fs.readFileSync('/etc/helmr-preparation-token'));
 const child=spawn(process.execPath,['-e','setInterval(()=>{},1000)'],{detached:true,stdio:'inherit'});
 fs.writeFileSync('/workspace/descendant.pid',String(child.pid));child.unref();console.log('preparation-complete');
 if(process.env.MODE==='expiry') await new Promise(()=>{setInterval(()=>{},1000)});
 if(process.env.MODE==='failure') await build.exec([process.execPath,'-e','process.exit(7)']);
}};`)
	for _, mode := range []string{"success", "large secret", "expiry", "failure"} {
		t.Run(mode, func(t *testing.T) {
			registry, entry, request := preparationGuestFixture(t)
			entry.imageRoot = root
			entry.computerMount = defaultRuntimeWorkdir
			entry.computerRoot = filepath.Join(root, "workspace")
			entry.runtimeUser = &resolvedRuntimeUser{UID: 1001, GID: 1001, Home: defaultRuntimeWorkdir}
			request.ExpiresAtUnixNano = time.Now().Add(10 * time.Second).UnixNano()
			if mode == "expiry" {
				request.ExpiresAtUnixNano = time.Now().Add(time.Second).UnixNano()
			}
			secretValue := "prepared-content"
			if mode == "large secret" {
				secretValue = strings.Repeat("A", 80<<10)
			}
			request.Start = &computerv0.PreparationStart{LogLimits: &computerv0.PreparationLogLimits{ChunkBytes: 1024, BufferBytes: 2048, BufferRecords: 4}, ComputerDefinitionId: "repo", Secrets: []*computerv0.ComputerSecretDelivery{{PlacementKind: "env", PlacementTarget: "BUILD_TOKEN", Value: []byte(secretValue)}, {PlacementKind: "env", PlacementTarget: "MODE", Value: []byte(mode)}, {PlacementKind: "file", PlacementTarget: "/etc/helmr-preparation-token", Value: []byte(secretValue)}}}
			if _, err := registry.controlPreparation(t.Context(), request, (*computerMountEntry).executePreparation); err != nil {
				t.Fatal(err)
			}
			request.Start = nil
			select {
			case <-entry.preparation.done:
			case <-time.After(12 * time.Second):
				t.Fatal("authored preparation did not terminate")
			}
			result, err := registry.controlPreparation(t.Context(), request, (*computerMountEntry).executePreparation)
			if err != nil {
				t.Fatal(err)
			}
			want := "succeeded"
			if mode != "success" && mode != "large secret" {
				want = "failed"
			}
			if result.State != want {
				t.Logf("preparation output: %v", result.Log)
				t.Fatalf("preparation result: %s %s", result.State, result.ErrorCode)
			}
			actual, err := os.ReadFile(filepath.Join(root, "workspace/prepared"))
			if err != nil || string(actual) != secretValue {
				t.Fatalf("prepared disk bytes: %v", err)
			}
			fileValue, err := os.ReadFile(filepath.Join(root, "workspace/prepared-file"))
			if err != nil || string(fileValue) != secretValue {
				t.Fatalf("preparation file delivery: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "etc/helmr-preparation-token")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("Secret file target survived cleanup: %v", err)
			}
			rawPID, err := os.ReadFile(filepath.Join(root, "workspace/descendant.pid"))
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(string(rawPID))
			if err != nil {
				t.Fatal(err)
			}
			if status, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status")); err == nil && !strings.Contains(string(status), "State:\tZ") {
				t.Fatal("detached descendant survived preparation success")
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			leaf, err := execCgroupLeafName("preparation:" + entry.computerID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(processCgroupRoot, leaf)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("preparation cgroup retained: %v", err)
			}
		})
	}

}
