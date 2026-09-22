package configstore

import (
	"context"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/codex"
	"github.com/maximhq/bifrost/framework/migrator"
	"gorm.io/gorm"
)

func migrationAddCodexConnections(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	return migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID:      "add_codex_connections",
		Migrate: func(tx *gorm.DB) error { return tx.WithContext(ctx).AutoMigrate(&codex.Connection{}) },
		// Explicit rollback removes subscription credentials. A binary downgrade
		// should instead leave this additive table intact for a later upgrade.
		Rollback: func(tx *gorm.DB) error { return tx.WithContext(ctx).Migrator().DropTable(&codex.Connection{}) },
	}}).Migrate()
}
