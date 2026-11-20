package multidb

import (
	"fmt"

	"github.com/shinession/gophant/internal/config"
	"github.com/shinession/gophant/internal/pg"
)

// DatabaseConfig returns the configuration of one database: the settings of cfg, with the connection
// strings pointing at the database and a replication slot named after it. The stages cannot tell it
// from the configuration of a single database, which is what lets them be reused unchanged.
//
// The publication keeps its name because a publication belongs to one database anyway, and so does
// the state of the migration (the gophant_meta schema of the database on s3).
func DatabaseConfig(cfg *config.Config, db string) (*config.Config, error) {
	out := *cfg
	var err error
	if out.Source.DSN, err = pg.WithDatabase(cfg.Source.DSN, db); err != nil {
		return nil, fmt.Errorf("source.dsn: %w", err)
	}
	if cfg.Source.ReplicationDSN != "" {
		if out.Source.ReplicationDSN, err = pg.WithDatabase(cfg.Source.ReplicationDSN, db); err != nil {
			return nil, fmt.Errorf("source.replication_dsn: %w", err)
		}
	}
	if out.Target.DSN, err = pg.WithDatabase(cfg.Target.DSN, db); err != nil {
		return nil, fmt.Errorf("target.dsn: %w", err)
	}
	out.Source.SlotName = SlotName(cfg.Source.SlotName, db)
	return &out, nil
}
