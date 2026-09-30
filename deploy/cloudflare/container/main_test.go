package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestLocalMigrations(t *testing.T) {
	if os.Getenv("DEPLOY_TEST_POSTGRES") != "1" {
		t.Skip("requires disposable local deployment_test database")
	}
	db := map[string]any{"host": "127.0.0.1", "port": "5432", "user": "postgres", "password": "fixture", "db_name": "deployment_test", "ssl_mode": "disable", "max_open_conns": 5, "max_idle_conns": 1}
	for i := 0; i < 2; i++ {
		if err := migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	// The runtime identity must start against the migrated schema without DDL.
	conn, err := sql.Open("pgx", "host=127.0.0.1 user=postgres password=fixture dbname=deployment_test sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	const role = "deployment_runtime_fixture"
	if _, err := conn.Exec("CREATE ROLE " + role + " LOGIN"); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec("DROP ROLE " + role)
	defer conn.Exec("DROP OWNED BY " + role)
	for _, grant := range []string{
		"GRANT CONNECT ON DATABASE deployment_test TO " + role,
		"GRANT USAGE ON SCHEMA public TO " + role,
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO " + role,
		"GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO " + role,
	} {
		if _, err := conn.Exec(grant); err != nil {
			t.Fatal(err)
		}
	}
	db["user"] = role
	db["matview_refresh_interval"] = "off"
	if err := migrate(db); err != nil {
		t.Fatal("restricted runtime schema startup failed:", err)
	}
}

func TestDatabaseRequiresNeonTLSAndCorrectPool(t *testing.T) {
	good := "postgresql://fixture:fixture-password@ep-test-pooler.us-east-2.aws.neon.tech/test?sslmode=verify-full"
	db, err := database(good, false)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(db)
	if strings.Contains(string(data), "fixture-password") {
		t.Fatal("password written to config")
	}
	for _, raw := range []string{"", "file:test.db", strings.Replace(good, "verify-full", "require", 1), strings.Replace(good, "neon.tech", "evil.example", 1), good + "&sslrootcert=evil", strings.Replace(good, "-pooler", "", 1)} {
		if _, err := database(raw, false); err == nil {
			t.Fatalf("accepted invalid database configuration")
		}
	}
	if _, err := database(good, true); err == nil {
		t.Fatal("migration accepted pooled connection")
	}
	if _, err := database(strings.Replace(good, "-pooler", "", 1), true); err != nil {
		t.Fatal(err)
	}
}

func TestConfigurationFailsClosedAndUsesOnlySecretReferences(t *testing.T) {
	if _, err := configuration(nil); err == nil {
		t.Fatal("accepted missing secrets")
	}
	for _, key := range []string{"BIFROST_ENCRYPTION_KEY", "BIFROST_ADMIN_PASSWORD", "HEADROOM_PROXY_TOKEN", "HEADROOM_SCOPE_KEY", "HEADROOM_METRICS_TOKEN", "MODAL_TOKEN_ID", "MODAL_TOKEN_SECRET"} {
		t.Setenv(key, strings.Repeat("fixture-secret", 4))
	}
	t.Setenv("HEADROOM_ENDPOINT", "https://fixture.modal.run")
	config, err := configuration(map[string]any{"ssl_mode": "verify-full"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(config)
	if strings.Contains(string(data), os.Getenv("BIFROST_ENCRYPTION_KEY")) || strings.Contains(string(data), "sqlite") {
		t.Fatal("secret or SQLite config")
	}
	for _, endpoint := range []string{"http://fixture.modal.run", "https://evil.example", "https://fixture.modal.run/path", "https://user:pass@fixture.modal.run"} {
		t.Setenv("HEADROOM_ENDPOINT", endpoint)
		if _, err := configuration(nil); err == nil {
			t.Fatal("accepted unsafe compressor origin")
		}
	}
}
