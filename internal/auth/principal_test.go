package auth

import (
	"testing"

	"github.com/google/uuid"
)

func TestActorPrincipalFormatsSessionAndAPIKey(t *testing.T) {
	userID := uuid.New()
	userPrincipal, err := ActorPrincipal(Actor{Kind: ActorKindSession, UserID: userID})
	if err != nil {
		t.Fatal(err)
	}
	if userPrincipal != "user:"+userID.String() {
		t.Fatalf("user principal = %q", userPrincipal)
	}

	apiKeyID := uuid.New()
	apiKeyPrincipal, err := ActorPrincipal(Actor{Kind: ActorKindAPIKey, APIKeyID: apiKeyID})
	if err != nil {
		t.Fatal(err)
	}
	if apiKeyPrincipal != "api_key:"+apiKeyID.String() {
		t.Fatalf("api key principal = %q", apiKeyPrincipal)
	}
}

func TestActorPrincipalRequiresConcreteIdentity(t *testing.T) {
	if _, err := ActorPrincipal(Actor{Kind: ActorKindSession}); err == nil {
		t.Fatal("session actor without user id returned no error")
	}
	if _, err := ActorPrincipal(Actor{Kind: ActorKindAPIKey}); err == nil {
		t.Fatal("api key actor without api key id returned no error")
	}
	if _, err := ActorPrincipal(Actor{Kind: ActorKindSystem}); err == nil {
		t.Fatal("system actor returned no error")
	}
}

func TestActorPrincipalAllowSystem(t *testing.T) {
	principal, err := ActorPrincipalAllowSystem(Actor{Kind: ActorKindSystem})
	if err != nil {
		t.Fatal(err)
	}
	if principal != "system" {
		t.Fatalf("system principal = %q", principal)
	}
}
