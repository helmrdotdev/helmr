package computer_test

import (
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/networkpolicy"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func TestComputerNetworkPolicyRetainsCreationDefinition(t *testing.T) {
	f := sessiontest.New(t)
	host := workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
	lease := computer.LeaseIdentity{EnvironmentID: f.Environment, ComputerID: f.Computer, InstanceID: f.Computer, Epoch: 1}
	policy, err := computer.ReadNetworkPolicy(t.Context(), f.Pool, host, lease)
	if err != nil || policy.Kind != networkpolicy.Ordinary {
		t.Fatalf("initial policy=%+v err=%v", policy, err)
	}
	newDeployment := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `
 INSERT INTO deployments(environment_id,id,bundle_digest) VALUES($1,$3,$4);
 INSERT INTO template_definitions(environment_id,template_id,deployment_id,image_spec_id,cpu_millis,memory_bytes,network_policy,prepare_network_policy)
 SELECT environment_id,template_id,$3,image_spec_id,cpu_millis,memory_bytes,'{"kind":"block"}','{"kind":"block"}' FROM template_definitions WHERE environment_id=$1 AND deployment_id=$2;
 UPDATE environments SET current_deployment_id=$3 WHERE id=$1;`, pgx.QueryExecModeSimpleProtocol, f.Environment, f.Deployment, newDeployment, "sha256:"+strings.Repeat("2", 64))
	policy, err = computer.ReadNetworkPolicy(t.Context(), f.Pool, host, lease)
	if err != nil || policy.Kind != networkpolicy.Ordinary {
		t.Fatalf("publication replaced old Computer policy: %+v %v", policy, err)
	}
	foreign := host
	foreign.HostID = uuid.NewV7()
	if _, err := computer.ReadNetworkPolicy(t.Context(), f.Pool, foreign, lease); err == nil {
		t.Fatal("foreign host read policy through execution boundary")
	}
	stale := lease
	stale.Epoch++
	if _, err := computer.ReadNetworkPolicy(t.Context(), f.Pool, host, stale); err == nil {
		t.Fatal("stale allocation read policy")
	}
}
