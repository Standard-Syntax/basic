//go:build provider_smoke

package gateway

import (
	"context"
	"os"
	"testing"

	"github.com/Standard-Syntax/basic/go/internal/reasoning/contracts"
)

func TestProviderSmoke(t *testing.T) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	model := os.Getenv("ANTHROPIC_MODEL")
	if apiKey == "" || model == "" {
		t.Fatal("provider smoke requires ANTHROPIC_API_KEY and ANTHROPIC_MODEL")
	}
	credentials := credentialSourceFunc(func(context.Context) (string, error) {
		return apiKey, nil
	})

	implementation, implementationManifest, implementationRequest :=
		anthropicImplementationFixture(t, nil)
	implementation.runtime.credentials = credentials
	implementation.runtime.models = StaticCapabilityModels{
		implementationManifest.Model.CapabilityClass: model,
	}
	implementationStore := implementation.runtime.artifacts.(*memoryArtifactStore)
	implementationPrompt, err := implementationStore.Put(t.Context(), []byte(
		`Return a valid implementation proposal for the supplied request. `+
			`Cover every acceptance criterion with one update to the supplied repository `+
			`file, use its exact original SHA-256, request only an available check, `+
			`and use null when no scope change is needed.`,
	))
	if err != nil {
		t.Fatal(err)
	}
	implementationManifest.Prompt.ArtifactURI = implementationPrompt.URI
	implementationManifest.Prompt.SHA256 = implementationPrompt.SHA256
	implementationResult, err := implementation.ProposeImplementation(
		t.Context(), implementationManifest, implementationRequest,
	)
	if err != nil {
		t.Fatalf("live implementation invocation: %v", err)
	}
	if implementationResult.MalformedOutput != nil {
		t.Fatalf("live implementation output malformed: %+v", implementationResult.MalformedOutput)
	}
	mappedImplementation, err := contracts.MapImplementationRequest(implementationRequest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contracts.MapImplementationProposal(
		implementationResult.Proposal, mappedImplementation,
	); err != nil {
		t.Fatalf("live implementation proposal failed kernel validation: %v", err)
	}

	review, reviewManifest, reviewRequest := anthropicReviewFixture(t, nil)
	review.runtime.credentials = credentials
	review.runtime.models = StaticCapabilityModels{
		reviewManifest.Model.CapabilityClass: model,
	}
	reviewStore := review.runtime.artifacts.(*memoryArtifactStore)
	reviewPrompt, err := reviewStore.Put(t.Context(), []byte(
		`Return an independent review proposal for the supplied evidence. `+
			`Use only evidence IDs present in the request. Recommend advisory_accept `+
			`with empty findings and actions when no material issue is established.`,
	))
	if err != nil {
		t.Fatal(err)
	}
	reviewManifest.Prompt.ArtifactURI = reviewPrompt.URI
	reviewManifest.Prompt.SHA256 = reviewPrompt.SHA256
	reviewResult, err := review.ProposeReview(t.Context(), reviewManifest, reviewRequest)
	if err != nil {
		t.Fatalf("live review invocation: %v", err)
	}
	if reviewResult.MalformedOutput != nil {
		t.Fatalf("live review output malformed: %+v", reviewResult.MalformedOutput)
	}
	mappedReview, err := contracts.MapReviewRequest(reviewRequest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contracts.MapReviewProposal(reviewResult.Proposal, mappedReview); err != nil {
		t.Fatalf("live review proposal failed kernel validation: %v", err)
	}
}
