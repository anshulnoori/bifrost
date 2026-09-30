package configstore

import (
	"context"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/migrator"
	"gorm.io/gorm"
)

func migrationAddDashboardOIDC(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	return migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{
		ID: "add_dashboard_oidc",
		Migrate: func(tx *gorm.DB) error {
			tx = tx.WithContext(ctx)
			if err := tx.AutoMigrate(&tables.OIDCLogin{}); err != nil {
				return err
			}
			for _, column := range []string{"OIDCIssuer", "OIDCSubject"} {
				if !tx.Migrator().HasColumn(&tables.SessionsTable{}, column) {
					if err := tx.Migrator().AddColumn(&tables.SessionsTable{}, column); err != nil {
						return err
					}
				}
			}
			return nil
		},
	}}).Migrate()
}
