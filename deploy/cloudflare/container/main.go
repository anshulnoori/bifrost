// The launcher writes only environment references, never credentials, to scratch.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/logstore"
)

var invalidConfig = errors.New("deployment configuration rejected")

// Errors deliberately omit URLs, parser errors, database details and secrets.
func database(raw string, migration bool) (map[string]any, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Scheme != "postgresql" || u.User == nil || u.Fragment != "" {
		return nil, invalidConfig
	}
	password, ok := u.User.Password()
	if !ok || password == "" || u.User.Username() == "" || !regexp.MustCompile(`^[a-z0-9.-]+\.neon\.tech$`).MatchString(u.Hostname()) || (u.Port() != "" && u.Port() != "5432") {
		return nil, invalidConfig
	}
	if strings.Contains(u.Hostname(), "-pooler.") == migration {
		return nil, invalidConfig
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil || query.Get("sslmode") != "verify-full" {
		return nil, invalidConfig
	}
	for name := range query {
		if name != "sslmode" && name != "channel_binding" {
			return nil, invalidConfig
		}
	}
	name := strings.TrimPrefix(u.Path, "/")
	if name == "" || strings.Contains(name, "/") {
		return nil, invalidConfig
	}
	for key, value := range map[string]string{"DEPLOY_PG_HOST": u.Hostname(), "DEPLOY_PG_USER": u.User.Username(), "DEPLOY_PG_PASSWORD": password, "DEPLOY_PG_DATABASE": name} {
		if err := os.Setenv(key, value); err != nil {
			return nil, invalidConfig
		}
	}
	os.Setenv("PGCONNECT_TIMEOUT", "10")
	return map[string]any{"host": "env.DEPLOY_PG_HOST", "port": "5432", "user": "env.DEPLOY_PG_USER", "password": "env.DEPLOY_PG_PASSWORD", "db_name": "env.DEPLOY_PG_DATABASE", "ssl_mode": "verify-full", "max_open_conns": 5, "max_idle_conns": 1, "conn_max_lifetime": "5m", "conn_max_idle_time": "30s"}, nil
}

func configuration(db map[string]any) (map[string]any, error) {
	for _, key := range []string{"BIFROST_ENCRYPTION_KEY", "BIFROST_ADMIN_PASSWORD", "HEADROOM_PROXY_TOKEN", "HEADROOM_SCOPE_KEY", "HEADROOM_METRICS_TOKEN"} {
		if len(os.Getenv(key)) < 32 {
			return nil, invalidConfig
		}
	}
	endpoint := os.Getenv("HEADROOM_ENDPOINT")
	u, err := url.Parse(endpoint)
	if err != nil || u == nil || u.Scheme != "https" || !strings.HasSuffix(u.Hostname(), ".modal.run") || u.Port() != "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, invalidConfig
	}
	if os.Getenv("MODAL_TOKEN_ID") == "" || os.Getenv("MODAL_TOKEN_SECRET") == "" {
		return nil, invalidConfig
	}
	logs := make(map[string]any)
	for k, v := range db {
		logs[k] = v
	}
	logs["matview_refresh_interval"] = "off"
	return map[string]any{
		"encryption_key": "env.BIFROST_ENCRYPTION_KEY",
		"client":         map[string]any{"enforce_auth_on_inference": true, "enable_logging": true, "disable_content_logging": true, "allow_per_request_content_storage_override": false, "allow_per_request_raw_override": false},
		"config_store":   map[string]any{"enabled": true, "type": "postgres", "config": db},
		"logs_store":     map[string]any{"enabled": true, "type": "postgres", "config": logs, "retention_days": 7},
		"governance":     map[string]any{"auth_config": map[string]any{"is_enabled": true, "admin_username": "owner", "admin_password": "env.BIFROST_ADMIN_PASSWORD"}},
		"plugins": []any{map[string]any{"name": "headroom", "path": "/app/headroom.so", "enabled": true, "placement": "post_builtin", "config": map[string]any{
			"enabled": false, "scope": "gateway", "endpoint": endpoint, "token_env": "HEADROOM_PROXY_TOKEN", "scope_key_env": "HEADROOM_SCOPE_KEY",
			"modal_key_env": "MODAL_TOKEN_ID", "modal_secret_env": "MODAL_TOKEN_SECRET", "failure_policy": "open", "timeout_ms": 2000, "min_text_bytes": 4096,
			"ccr": false, "metrics_address": "127.0.0.1:9909", "metrics_token_env": "HEADROOM_METRICS_TOKEN", "retention_seconds": 900,
		}}},
	}, nil
}

type quiet struct{}

func (quiet) Debug(string, ...any)                   {}
func (quiet) Info(string, ...any)                    {}
func (quiet) Warn(string, ...any)                    {}
func (quiet) Error(string, ...any)                   {}
func (quiet) Fatal(string, ...any)                   { os.Exit(1) }
func (quiet) SetLevel(schemas.LogLevel)              {}
func (quiet) SetOutputType(schemas.LoggerOutputType) {}
func (quiet) LogHTTPRequest(schemas.LogLevel, string) schemas.LogEventBuilder {
	return schemas.NoopLogEvent
}

func migrate(db map[string]any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	data, _ := json.Marshal(map[string]any{"enabled": true, "type": "postgres", "config": db})
	var config configstore.Config
	if json.Unmarshal(data, &config) != nil {
		return invalidConfig
	}
	store, err := configstore.NewConfigStore(ctx, &config, quiet{})
	if err != nil {
		return invalidConfig
	}
	defer store.Close(ctx)
	var logs logstore.Config
	if json.Unmarshal(data, &logs) != nil {
		return invalidConfig
	}
	logDB, err := logstore.NewLogStore(ctx, &logs, quiet{})
	if err != nil {
		return invalidConfig
	}
	defer logDB.Close(ctx)
	return nil
}

func run() error {
	migration := len(os.Args) == 2 && os.Args[1] == "--migrate"
	if len(os.Args) > 1 && !migration {
		return invalidConfig
	}
	db, err := database(os.Getenv("NEON_DATABASE_URL"), migration)
	if err != nil {
		return err
	}
	os.Unsetenv("NEON_DATABASE_URL")
	if migration {
		return migrate(db)
	}
	config, err := configuration(db)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "bifrost-")
	if err != nil {
		return invalidConfig
	}
	defer os.RemoveAll(dir)
	data, _ := json.Marshal(config)
	if os.WriteFile(dir+"/config.json", data, 0600) != nil {
		return invalidConfig
	}
	cmd := exec.Command("/app/bifrost", "-app-dir", dir, "-host", "0.0.0.0", "-port", "8080", "-log-level", "error", "-log-style", "json")
	// Bifrost can include database/HTTP errors in its logs. Until every upstream
	// formatter is audited, export metadata via PostgreSQL and drop process output.
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if cmd.Start() != nil {
		return invalidConfig
	}
	fmt.Println(`{"event":"gateway_starting"}`)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	result := make(chan error, 1)
	go func() { result <- cmd.Wait() }()
	select {
	case err := <-result:
		if err != nil {
			return invalidConfig
		}
	case <-signals:
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-result:
		case <-time.After(30 * time.Second):
			_ = cmd.Process.Kill()
			<-result
		}
	}
	fmt.Println(`{"event":"gateway_stopped"}`)
	return nil
}

func main() {
	if run() != nil {
		fmt.Fprintln(os.Stderr, `{"event":"deployment_failed"}`)
		os.Exit(1)
	}
}
