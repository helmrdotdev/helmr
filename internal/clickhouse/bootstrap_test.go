package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
)

func testBootstrapConfig(endpoint string) BootstrapConfig {
	return BootstrapConfig{
		URL:          endpoint,
		Admin:        BootstrapCredential{"default", "Admin-Only-Test-Password-1"},
		Reader:       BootstrapCredential{"telemetry_reader", "Reader-Only-Test-Password-2"},
		Ingester:     BootstrapCredential{"telemetry_ingester", "Ingester-Only-Test-Password-3"},
		Migration:    BootstrapCredential{"telemetry_migration", "Migration-Only-Test-Password-4"},
		ReaderLimits: ReaderLimits{MaxRowsToRead: 100000, MaxBytesToRead: 10000000, MaxMemoryUsage: 100000000},
	}
}

func TestBootstrapConfigurationAndErrorsProtectCredentials(t *testing.T) {
	valid := testBootstrapConfig("https://clickhouse.example.test")
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*BootstrapConfig){
		"embedded password": func(cfg *BootstrapConfig) { cfg.URL = "https://secret:password@example.test" },
		"plaintext remote":  func(cfg *BootstrapConfig) { cfg.URL = "http://example.test" },
		"missing username":  func(cfg *BootstrapConfig) { cfg.Reader.User = "" },
		"default reader":    func(cfg *BootstrapConfig) { cfg.Reader.User = "default" },
		"duplicate user":    func(cfg *BootstrapConfig) { cfg.Reader.User = cfg.Ingester.User },
		"duplicate secret":  func(cfg *BootstrapConfig) { cfg.Reader.Password = cfg.Admin.Password },
		"unsafe identifier": func(cfg *BootstrapConfig) { cfg.Reader.User = "reader`; DROP USER default" },
		"zero limit":        func(cfg *BootstrapConfig) { cfg.ReaderLimits.MaxRowsToRead = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			change(&cfg)
			if cfg.Validate() == nil {
				t.Fatal("invalid bootstrap configuration accepted")
			}
		})
	}
	for _, err := range []error{errors.New("secret SQL literal or URL"), &ch.Exception{Code: 497, Message: "secret SQL literal or URL"}} {
		got := bootstrapError("create user", err).Error()
		if strings.Contains(got, "secret") || strings.Contains(got, "literal") {
			t.Fatalf("unsafe error: %s", got)
		}
	}
}

func TestBootstrapAgainstOwnedClickHouse(t *testing.T) {
	if os.Getenv("HELMR_TEST_CLICKHOUSE_BOOTSTRAP") != "1" {
		t.Skip("set HELMR_TEST_CLICKHOUSE_BOOTSTRAP=1 to start an owned disposable local server")
	}
	server := startClickHouseTestServerWithProfile(t, "<max_result_rows>100</max_result_rows><result_overflow_mode>break</result_overflow_mode>")
	endpoint := server.URL
	cfg := testBootstrapConfig(endpoint)
	admin, err := New(Config{URL: endpoint, User: cfg.Admin.User, Password: cfg.Admin.Password})
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	mustExec := func(client *Client, query string, args ...any) {
		t.Helper()
		if err := client.Exec(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	expectDenied := func(client *Client, query string) {
		t.Helper()
		if err := client.Exec(t.Context(), query); err == nil {
			t.Fatalf("unexpectedly allowed: %s", query)
		}
	}
	mustBootstrap := func() {
		t.Helper()
		if err := Bootstrap(t.Context(), admin, cfg); err != nil {
			t.Fatal(err)
		}
	}
	mustBootstrap()
	mustBootstrap()
	connect := func(identity BootstrapCredential) *Client {
		t.Helper()
		c, err := New(Config{URL: endpoint, User: identity.User, Password: identity.Password})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	reader, ingester, migration := connect(cfg.Reader), connect(cfg.Ingester), connect(cfg.Migration)
	mustExec(migration, "CREATE DATABASE IF NOT EXISTS helmr_telemetry")
	mustExec(migration, "CREATE TABLE helmr_telemetry.bootstrap_probe (value UInt64) ENGINE=ReplacingMergeTree ORDER BY value")
	batch, err := ingester.PrepareBatch(t.Context(), "INSERT INTO helmr_telemetry.bootstrap_probe (value)")
	if err != nil {
		t.Fatal(err)
	}
	if err = batch.Append(uint64(1)); err != nil {
		t.Fatal(err)
	}
	if err = batch.Send(); err != nil {
		t.Fatal(err)
	}
	mustExec(reader, "SELECT value FROM helmr_telemetry.bootstrap_probe FINAL LIMIT 1")
	expectDenied(reader, "INSERT INTO helmr_telemetry.bootstrap_probe VALUES (2)")
	expectDenied(reader, "CREATE TABLE helmr_telemetry.denied (value UInt64) ENGINE=Memory")
	expectDenied(ingester, "SELECT value FROM helmr_telemetry.bootstrap_probe LIMIT 1")
	expectDenied(ingester, "CREATE TABLE helmr_telemetry.denied (value UInt64) ENGINE=Memory")
	expectDenied(migration, "INSERT INTO helmr_telemetry.bootstrap_probe VALUES (2)")
	expectDenied(migration, "SELECT value FROM helmr_telemetry.bootstrap_probe LIMIT 1")
	expectDenied(migration, "CREATE USER denied_user")
	expectDenied(migration, "CREATE DATABASE denied_database")
	mustExec(admin, "CREATE DATABASE other_telemetry")
	mustExec(admin, "CREATE TABLE other_telemetry.records (value UInt64) ENGINE=Memory")
	expectDenied(reader, "SELECT value FROM other_telemetry.records LIMIT 1")
	for _, setting := range []string{"readonly=0", "max_rows_to_read=0", "max_rows_to_read=100001", "max_bytes_to_read=0", "max_bytes_to_read=10000001", "max_memory_usage=0", "max_memory_usage=100000001", "read_overflow_mode='break'", "timeout_overflow_mode='break'", "result_overflow_mode='break'"} {
		expectDenied(reader, "SELECT value FROM helmr_telemetry.bootstrap_probe LIMIT 1 SETTINGS "+setting)
	}
	// No deadline: the driver's deadline augmentation must not mask this probe.
	if err := reader.conn.Exec(context.Background(), "SELECT 1 SETTINGS max_execution_time=0"); err == nil {
		t.Fatal("zero execution limit accepted")
	}
	if err := reader.conn.Exec(context.Background(), "SELECT 1 SETTINGS max_execution_time=36"); err == nil {
		t.Fatal("excess execution limit accepted")
	}
	// A server-configured result cap must fail the entire query even when the
	// inherited default profile would otherwise return partial results.
	mustExec(admin, "INSERT INTO helmr_telemetry.bootstrap_probe SELECT number+2 FROM numbers(200)")
	var resultRows []struct {
		Value uint64 `ch:"value"`
	}
	resultErr := reader.Select(t.Context(), &resultRows, "SELECT value FROM helmr_telemetry.bootstrap_probe LIMIT 200 SETTINGS max_block_size=16, max_threads=1")
	var resultException *ch.Exception
	if !errors.As(resultErr, &resultException) || resultException.Code != 396 {
		var effective []struct {
			Name  string `ch:"name"`
			Value string `ch:"value"`
		}
		_ = reader.Select(t.Context(), &effective, "SELECT name, value FROM system.settings WHERE name IN ('max_result_rows', 'result_overflow_mode') LIMIT 2")
		t.Fatalf("expected inherited result limit error 396, got %v (%d rows, settings %+v)", resultErr, len(resultRows), effective)
	}
	mustExec(admin, "INSERT INTO helmr_telemetry.bootstrap_probe SELECT number+2 FROM numbers(200000)")
	scanQuery := "SELECT sum(value) FROM helmr_telemetry.bootstrap_probe WHERE value>=1 LIMIT 1"
	mustExec(admin, scanQuery)
	scanErr := reader.Exec(t.Context(), scanQuery)
	var scanException *ch.Exception
	if !errors.As(scanErr, &scanException) || scanException.Code != 158 {
		t.Fatalf("expected row scan limit error 158, got %v", scanErr)
	}
	// A partial bootstrap has an existing credential but missing privileges.
	mustExec(admin, "REVOKE INSERT ON helmr_telemetry.* FROM telemetry_ingester")
	mustBootstrap()
	mustExec(ingester, "INSERT INTO helmr_telemetry.bootstrap_probe VALUES (300001)")
	wrong := cfg
	wrong.Migration.Password = "Wrong-Rotation-Password-5"
	wrong.Reader.User = "uncreated_reader"
	if err := Bootstrap(t.Context(), admin, wrong); err == nil {
		t.Fatal("credential mismatch accepted")
	}
	assertNoUser(t, admin, "uncreated_reader")
	mustExec(admin, "GRANT SELECT ON helmr_telemetry.* TO telemetry_migration")
	drift := cfg
	drift.Reader.User = "uncreated_reader"
	if err := Bootstrap(t.Context(), admin, drift); err == nil {
		t.Fatal("privilege drift accepted")
	}
	assertNoUser(t, admin, "uncreated_reader")
	mustExec(admin, "REVOKE SELECT ON helmr_telemetry.* FROM telemetry_migration")
	mustExec(admin, "CREATE ROLE inherited_access")
	mustExec(admin, "GRANT inherited_access TO telemetry_migration")
	if err := Bootstrap(t.Context(), admin, drift); err == nil {
		t.Fatal("role drift accepted")
	}
	assertNoUser(t, admin, "uncreated_reader")
	mustExec(admin, "REVOKE inherited_access FROM telemetry_migration")
	mustExec(admin, "CREATE SETTINGS PROFILE inherited_settings SETTINGS max_threads=1 TO telemetry_reader")
	if err := Bootstrap(t.Context(), admin, cfg); err == nil {
		t.Fatal("externally assigned profile accepted")
	}
	mustExec(admin, "DROP SETTINGS PROFILE inherited_settings")
	// Global profiles must be checked even before creating a missing identity.
	// Result-overflow settings can otherwise yield silently incomplete pages.
	mustExec(admin, "CREATE SETTINGS PROFILE global_settings SETTINGS max_result_rows=1, result_overflow_mode='break' TO ALL")
	if err := Bootstrap(t.Context(), admin, drift); err == nil {
		t.Fatal("TO ALL profile accepted")
	}
	assertNoUser(t, admin, "uncreated_reader")
	mustExec(admin, "DROP SETTINGS PROFILE global_settings")
	mustExec(admin, "CREATE SETTINGS PROFILE global_settings SETTINGS max_result_rows=1, result_overflow_mode='break' TO ALL EXCEPT default")
	if err := Bootstrap(t.Context(), admin, drift); err == nil {
		t.Fatal("applicable ALL EXCEPT profile accepted")
	}
	assertNoUser(t, admin, "uncreated_reader")
	mustExec(admin, "DROP SETTINGS PROFILE global_settings")
	mustExec(admin, "CREATE SETTINGS PROFILE global_settings SETTINGS max_threads=2 TO ALL EXCEPT telemetry_reader, telemetry_ingester, telemetry_migration")
	mustBootstrap()
}

func assertNoUser(t *testing.T, admin *Client, name string) {
	t.Helper()
	var users []struct {
		Name string `ch:"name"`
	}
	if err := admin.Select(t.Context(), &users, "SELECT name FROM system.users WHERE name=? LIMIT 1", name); err != nil {
		t.Fatal(err)
	}
	if len(users) != 0 {
		t.Fatal("bootstrap wrote a user before all existing identities passed preflight")
	}
}

func startClickHouseTestServer(t *testing.T) Config {
	t.Helper()
	return startClickHouseTestServerWithProfile(t, "")
}

func startClickHouseTestServerWithProfile(t *testing.T, profileSettings string) Config {
	t.Helper()
	binary, err := exec.LookPath("clickhouse")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	dir := t.TempDir()
	config := fmt.Sprintf(`<clickhouse><logger><level>error</level><log>%s/server.log</log><errorlog>%s/server.err.log</errorlog></logger><listen_host>127.0.0.1</listen_host><http_port>%d</http_port><path>%s/data/</path><tmp_path>%s/tmp/</tmp_path><max_server_memory_usage>2000000000</max_server_memory_usage><max_thread_pool_size>256</max_thread_pool_size><background_schedule_pool_size>8</background_schedule_pool_size><background_message_broker_schedule_pool_size>8</background_message_broker_schedule_pool_size><background_distributed_schedule_pool_size>8</background_distributed_schedule_pool_size><background_buffer_flush_schedule_pool_size>8</background_buffer_flush_schedule_pool_size><background_pool_size>16</background_pool_size><users><default><password>Admin-Only-Test-Password-1</password><access_management>1</access_management><networks><ip>127.0.0.1</ip></networks><profile>default</profile><quota>default</quota></default></users><profiles><default><max_threads>2</max_threads>%s</default></profiles><quotas><default/></quotas><access_control_path>%s/access/</access_control_path></clickhouse>`, dir, dir, port, dir, dir, profileSettings, dir)
	path := filepath.Join(dir, "config.xml")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	output, err := os.Create(filepath.Join(dir, "process.log"))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "server", "--config-file", path)
	command.Stdout, command.Stderr = output, output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	t.Cleanup(func() {
		_ = command.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = command.Process.Kill()
			<-done
		}
		_ = output.Close()
	})
	endpoint := fmt.Sprintf("http://127.0.0.1:%d", port)
	cfg := testBootstrapConfig(endpoint)
	client, err := New(Config{URL: endpoint, User: cfg.Admin.User, Password: cfg.Admin.Password})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		err := client.Ping(ctx)
		cancel()
		if err == nil {
			return Config{URL: endpoint, User: cfg.Admin.User, Password: cfg.Admin.Password}
		}
		select {
		case err := <-done:
			done <- err
			t.Fatalf("owned ClickHouse server stopped: %v (logs: %s)", err, dir)
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}
	processLog, _ := os.ReadFile(filepath.Join(dir, "process.log"))
	serverLog, _ := os.ReadFile(filepath.Join(dir, "server.err.log"))
	t.Fatalf("owned ClickHouse server did not become ready: %s\n%s", processLog, serverLog)
	return Config{}
}
