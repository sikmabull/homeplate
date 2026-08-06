package runner

import (
	"context"
	"testing"

	"github.com/homeplate-ci/homeplate/internal/ghapi"
)

type tokenOnlyMinter struct{}

func (tokenOnlyMinter) RepoRegistrationToken(ctx context.Context, slug string) (*ghapi.RegistrationToken, error) {
	return &ghapi.RegistrationToken{Token: "t"}, nil
}
func (tokenOnlyMinter) OrgRegistrationToken(ctx context.Context, org string) (*ghapi.RegistrationToken, error) {
	return &ghapi.RegistrationToken{Token: "t"}, nil
}

type jitMinter struct {
	tokenOnlyMinter
	called bool
}

func (j *jitMinter) GenerateJITConfigFor(ctx context.Context, scope, slug string, req ghapi.JITConfigRequest) (*ghapi.JITConfig, error) {
	j.called = true
	if req.RunnerGroupID != 1 {
		return nil, context.DeadlineExceeded
	}
	return &ghapi.JITConfig{EncodedJITConfig: "abc123"}, nil
}

func TestMintJITSkipsNonJitMinters(t *testing.T) {
	e := &Engine{Minter: tokenOnlyMinter{}}
	jit, err := e.mintJIT(context.Background(), Target{Slug: "a/b"}, "name", []string{"homeplate"})
	if err != nil || jit != nil {
		t.Fatalf("expected (nil,nil) for token-only minter, got %v %v", jit, err)
	}
}

func TestMintJITSkipsNamedRunnerGroups(t *testing.T) {
	jm := &jitMinter{}
	e := &Engine{Minter: jm}
	jit, err := e.mintJIT(context.Background(), Target{Slug: "a/b", RunnerGroup: "prod"}, "name", nil)
	if err != nil || jit != nil || jm.called {
		t.Fatalf("named runner group must use token flow (group ID lookup is v2), got %v %v called=%v", jit, err, jm.called)
	}
}

func TestMintJITHappyPath(t *testing.T) {
	jm := &jitMinter{}
	e := &Engine{Minter: jm}
	jit, err := e.mintJIT(context.Background(), Target{Slug: "a/b"}, "name", []string{"homeplate"})
	if err != nil || jit == nil || jit.EncodedJITConfig != "abc123" {
		t.Fatalf("expected JIT config, got %v %v", jit, err)
	}
}
