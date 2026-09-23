//go:build integration

package integration_test

import (
	"context"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/vendors"
	_ "github.com/ogen-app/ogen/src/infra/vendors/llm" // register the model vendors
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// inMemModelConfig is a DB-free modelconfig.Source so the flow integration
// tests can drive the CON-308 resolver without a migrated flow_model_config
// table. reconcile seeds one global row per catalog slot from the Defaults.
type inMemModelConfig struct{ rows []models.FlowModelConfig }

func (s *inMemModelConfig) List(context.Context) ([]models.FlowModelConfig, error) {
	return append([]models.FlowModelConfig(nil), s.rows...), nil
}

func (s *inMemModelConfig) Upsert(_ context.Context, c *models.FlowModelConfig) error {
	s.rows = append(s.rows, *c)
	return nil
}

// initModelConfig points every flow slot at the given models via the resolver,
// so the flows (which now resolve models through modelconfig, CON-308) get a
// real model in the harness. Call it once the desired model ids are known.
func initModelConfig(ctx context.Context, generation, planning string) {
	modelconfig.Init(ctx, &inMemModelConfig{}, modelconfig.Defaults{
		Generation: generation,
		Quality:    generation,
		Planning:   planning,
		Embed:      "gemini-embedding-2",
	}, vendors.VendorOf, tenantctx.TierFrom)
}
