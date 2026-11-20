package cli

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shinession/gophant/internal/config"
	"github.com/shinession/gophant/internal/pg"
)

// pools holds the connection pools of s1 and s3 that a command works with.
type pools struct {
	src *pgxpool.Pool
	dst *pgxpool.Pool
}

// openPools connects to both databases. The application name shows up in pg_stat_activity.
func openPools(ctx context.Context, cfg *config.Config, appName string) (*pools, error) {
	src, err := pg.NewPool(ctx, cfg.Source.DSN, appName, 4)
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	dst, err := pg.NewPool(ctx, cfg.Target.DSN, appName, 4)
	if err != nil {
		src.Close()
		return nil, fmt.Errorf("target: %w", err)
	}
	return &pools{src: src, dst: dst}, nil
}

// Close releases both pools.
func (p *pools) Close() {
	p.src.Close()
	p.dst.Close()
}
