package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/providers/k3d"
)

func TestK3dLifecycle(t *testing.T) {
	if os.Getenv("INTEGRATION") == "" {
		t.Skip("set INTEGRATION=1 to run real infra tests")
	}

	name := fmt.Sprintf("test-%d", time.Now().UnixNano())
	p := k3d.New()

	// Guarantee cleanup even on panic or test failure.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		exec.CommandContext(ctx, "k3d", "cluster", "delete", name).Run()
	})

	ctx := context.Background()

	// Preflight must pass before provisioning.
	if err := p.PreFlight(ctx, providers.Spec{Name: name}); err != nil {
		t.Fatalf("preflight: %v", err)
	}

	// Provision real cluster.
	if _, err := p.Provision(ctx, providers.Spec{Name: name}); err != nil {
		t.Fatalf("provision: %v", err)
	}

	// Status should report healthy after bootstrap.
	var res providers.StatusResult
	var err error
	for i := 0; i < 30; i++ {
		res, err = p.Status(ctx, providers.Spec{Name: name})
		if err == nil && res.Healthy {
			break
		}
		time.Sleep(5 * time.Second)
	}
	if !res.Healthy {
		t.Fatalf("expected healthy, got: %+v | err: %v", res, err)
	}
}
