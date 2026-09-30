package configstore

import (
	"context"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/chatgpt"
	"github.com/maximhq/bifrost/framework/migrator"
	"gorm.io/gorm"
)

func migrationAddChatGPTConnections(ctx context.Context, db *gorm.DB, logger schemas.Logger) error {
	return migrator.New(db, migrator.DefaultOptions, []*migrator.Migration{{ID: "add_chatgpt_connections", Migrate: func(tx *gorm.DB) error { return tx.WithContext(ctx).AutoMigrate(chatgpt.MigrationModels()...) }, Rollback: func(tx *gorm.DB) error {
		return tx.WithContext(ctx).Migrator().DropTable("chatgpt_oauth_attempts", "chatgpt_oauth_hosts", &chatgpt.Connection{})
	}}}).Migrate()
}
