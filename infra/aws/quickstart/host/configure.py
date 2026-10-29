#!/usr/bin/env python3
"""Render local container configuration without putting credentials in Terraform."""
import json
import os
from pathlib import Path
import secrets
import subprocess


DIAGNOSTICS = {
    "DIAGNOSTIC_CHUNK_BYTES": "65536",
    "DIAGNOSTIC_SOURCE_BYTES": "4194304",
    "DIAGNOSTIC_SOURCE_RECORDS": "1024",
    "DIAGNOSTIC_ENVIRONMENT_BYTES": "16777216",
    "DIAGNOSTIC_ENVIRONMENT_RECORDS": "4096",
    "DIAGNOSTIC_QUEUE_BYTES": "67108864",
    "DIAGNOSTIC_QUEUE_RECORDS": "16384",
    "DIAGNOSTIC_DB_MAX_CONNECTIONS": "2",
}


def merge_environment(defaults, extra, required):
    conflicts = required.keys() & extra.keys()
    if conflicts:
        raise ValueError("Use the Quickstart inputs for: " + ", ".join(sorted(conflicts)))
    return defaults | extra | required


def compose_config(config, credentials, platform, data_dir="/var/lib/helmr", config_dir="/etc/helmr"):
    """Return native Compose data. The caller escapes literal dollars on disk."""
    url = lambda user: f"postgres://{user}:{credentials[user]}@127.0.0.1:5432/helmr?sslmode=require"
    databases = {
        "DATABASE_URL": url("helmr_app"),
        "DATABASE_MIGRATION_URL": url("helmr_migration"),
        "DATABASE_AUDIT_MAINTENANCE_URL": url("helmr_audit"),
    }
    common = {
        "APP_ORIGIN": config["app_origin"],
        "SLACK_TRANSPORT": config["slack_transport"],
        "CLICKHOUSE_URL": "http://127.0.0.1:8123",
        "DATABASE_URL": databases["DATABASE_URL"],
        "ENCRYPTION_KEY": platform["encryption_key"],
        "COMPUTER_FENCING_KEY": platform["computer_fencing_key"],
    }
    control = merge_environment(DIAGNOSTICS | {
        "AUDIT_RETENTION_SECONDS": "2592000",
        "DEPLOYMENT_UPLOAD_PROCESS_CONCURRENCY": "4",
        "DEPLOYMENT_UPLOAD_ORGANIZATION_CONCURRENCY": "4",
        "DEPLOYMENT_UPLOAD_IDLE_TIMEOUT": "2m",
        "DEPLOYMENT_UPLOAD_MAX_DURATION": "2h",
        "ENVIRONMENT_MAX_RESIDENT_COMPUTERS": "10",
        "ENVIRONMENT_MAX_CPU_MILLIS": "4000",
        "ENVIRONMENT_MAX_MEMORY_BYTES": "8589934592",
        "ENVIRONMENT_MAX_RESERVED_STORAGE_BYTES": "1099511627776",
        "ENVIRONMENT_MAX_OUTSTANDING_ADMISSIONS": "20",
        "ENVIRONMENT_MAX_CAUSAL_DEPTH": "8",
        "ENVIRONMENT_ADMISSION_RATE_PER_SECOND": "5",
        "ENVIRONMENT_ADMISSION_BURST": "10",
        "ENVIRONMENT_PREPARATION_TIMEOUT_MS": "600000",
    }, config["controlplane_environment"], common | {
        "CONTROL_PLANE_ADDR": "127.0.0.1:8080",
        "CONTROL_PLANE_HTTP_IDLE_TIMEOUT": "270s",
        "DEPLOYMENT_MODE": "self-hosted",
        "DEPLOYMENT_RUNTIME_DESCRIPTOR_PATH": "/usr/local/share/helmr/runtime.descriptor.json",
        "CAS_URI": config["cas_uri"],
        "PLATFORM_STORE_URI": config["platform_store_uri"],
        "AWS_REGION": config["aws_region"],
        "CONTROL_PLANE_NETWORK_PRIVATE_BUSINESS_CIDRS": json.dumps(config["private_business_cidrs"]),
        "CONTROL_PLANE_NETWORK_BLOCKED_IPV4_CIDRS": json.dumps(config["blocked_ipv4_cidrs"]),
        "GITHUB_OAUTH_CLIENT_ID": config["github_oauth_client_id"],
        "GITHUB_OAUTH_CLIENT_SECRET": platform.get("github_oauth_client_secret", ""),
        "WORKER_HOST_CREDENTIAL_SIGNING_KEY": platform["worker_host_credential_signing_key"],
        "AUTH_KEY": platform["auth_key"],
        "COMPUTER_WRAPPING_KEY_ID": "quickstart-v1",
        "COMPUTER_WRAPPING_KEY": platform["computer_wrapping_key"],
        "CAPACITY_TOKEN": platform["capacity_token"],
        "BOOTSTRAP_ENABLED": "1",
        "BOOTSTRAP_REGION_ID": config["region_id"],
        "BOOTSTRAP_REGION_DISPLAY_NAME": config["region_id"],
        "BOOTSTRAP_WORKER_GROUP_NAME": config["worker_group_name"],
        "BOOTSTRAP_WORKER_TOKEN": platform["worker_enrollment_token"],
        "CLICKHOUSE_USER": "helmr_reader",
        "CLICKHOUSE_PASSWORD": credentials["helmr_reader"],
    })
    dispatcher = merge_environment(DIAGNOSTICS | {
        "AUDIT_RETENTION_SECONDS": "2592000",
        "DIAGNOSTIC_EXPORT_BATCH_BYTES": "4194304",
        "DIAGNOSTIC_EXPORT_BATCH_RECORDS": "1024",
    }, config["dispatcher_environment"], common | {
        "CONTROL_PLANE_URL": config["app_origin"],
        "CLICKHOUSE_USER": "helmr_ingester",
        "CLICKHOUSE_PASSWORD": credentials["helmr_ingester"],
    })
    service = lambda image, **values: {
        "image": image, "platform": "linux/amd64", "network_mode": "host", "restart": "unless-stopped",
        "logging": {"driver": "json-file", "options": {"max-size": "10m", "max-file": "3"}},
        **values,
    }
    task = lambda command, env: service(config["controlplane_image"],
        entrypoint=["/usr/local/bin/control-plane"], command=command, environment=env,
        restart="no", profiles=["setup"])
    services = {
        "postgres": service(config["postgres_image"],
            environment={"POSTGRES_USER": "postgres", "POSTGRES_DB": "helmr", "POSTGRES_PASSWORD": credentials["postgres"], "POSTGRES_INITDB_ARGS": "--auth-host=scram-sha-256"},
            command=["postgres", "-c", "listen_addresses=127.0.0.1", "-c", "ssl=on",
                     "-c", "ssl_cert_file=/tls/server.crt", "-c", "ssl_key_file=/tls/server.key"],
            volumes=[f"{data_dir}/postgres:/var/lib/postgresql", f"{config_dir}/postgres-tls:/tls:ro"],
            healthcheck={"test": ["CMD", "pg_isready", "-h", "127.0.0.1", "-U", "postgres", "-d", "helmr"], "interval": "5s", "timeout": "3s", "retries": 30}),
        "clickhouse": service(config["clickhouse_image"],
            environment={"CLICKHOUSE_USER": "helmr_admin", "CLICKHOUSE_PASSWORD": credentials["helmr_admin"], "CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT": "1"},
            volumes=[f"{data_dir}/clickhouse:/var/lib/clickhouse", f"{config_dir}/clickhouse.xml:/etc/clickhouse-server/config.d/docker_related_config.xml:ro"],
            healthcheck={"test": ["CMD", "wget", "--spider", "--quiet", "http://127.0.0.1:8123/ping"], "interval": "5s", "timeout": "3s", "retries": 30}),
        "controlplane": service(config["controlplane_image"], entrypoint=["/usr/local/bin/control-plane"], environment=control),
        "dispatcher": service(config["controlplane_image"], entrypoint=["/usr/local/bin/dispatcher"], environment=dispatcher),
        "caddy": service(config["caddy_image"], volumes=[f"{config_dir}/Caddyfile:/etc/caddy/Caddyfile:ro", f"{data_dir}/caddy:/data"]),
        "database-bootstrap": task(["database-bootstrap"], databases | {
            "DATABASE_ADMIN_HOST": "127.0.0.1", "DATABASE_ADMIN_USERNAME": "postgres",
            "DATABASE_ADMIN_PASSWORD": credentials["postgres"], "DATABASE_NAME": "helmr"}),
        "clickhouse-bootstrap": task(["clickhouse-bootstrap"], {
            "CLICKHOUSE_URL": "http://127.0.0.1:8123",
            "CLICKHOUSE_BOOTSTRAP_USER": "helmr_admin", "CLICKHOUSE_BOOTSTRAP_PASSWORD": credentials["helmr_admin"],
            **{f"CLICKHOUSE_{role.upper()}_{field.upper()}": value
               for role in ("reader", "ingester", "migration")
               for field, value in (("user", "helmr_" + role), ("password", credentials["helmr_" + role]))},
        }),
        "migrate": task(["migrate", "up"], databases | {
            "CLICKHOUSE_URL": "http://127.0.0.1:8123", "CLICKHOUSE_USER": "helmr_migration",
            "CLICKHOUSE_PASSWORD": credentials["helmr_migration"]}),
        "audit-purge": task(["audit", "purge"], {
            "DATABASE_AUDIT_MAINTENANCE_URL": databases["DATABASE_AUDIT_MAINTENANCE_URL"],
            "AUDIT_RETENTION_SECONDS": control["AUDIT_RETENTION_SECONDS"],
            "AUDIT_MAINTENANCE_TIMEOUT_SECONDS": "300", "AUDIT_PURGE_MAX_BATCHES": "50"}),
    }
    return {"name": "helmr", "services": services}


def escape_compose(value):
    # Compose interpolates every string value, including credentials.
    if isinstance(value, str):
        return value.replace("$", "$$")
    if isinstance(value, list):
        return [escape_compose(item) for item in value]
    if isinstance(value, dict):
        return {key: escape_compose(item) for key, item in value.items()}
    return value


def main():
    os.umask(0o077)
    config = json.loads(Path("/etc/helmr/quickstart.json").read_text())
    credentials_file = Path("/var/lib/helmr/database-credentials.json")
    credentials_file.parent.mkdir(parents=True, exist_ok=True)
    if not credentials_file.exists():
        # Database credentials survive service restarts with their local data.
        credentials = {user: secrets.token_hex(32) for user in (
            "postgres", "helmr_app", "helmr_migration", "helmr_audit", "helmr_admin", "helmr_reader", "helmr_ingester")}
        with credentials_file.open("x") as file:
            json.dump(credentials, file)
    credentials = json.loads(credentials_file.read_text())
    platform = {key: subprocess.check_output([
        "aws", "secretsmanager", "get-secret-value", "--region", config["aws_region"],
        "--secret-id", arn, "--query", "SecretString", "--output", "text"], text=True).rstrip("\n")
        for key, arn in config["secret_arns"].items()}
    compose = compose_config(config, credentials, platform)
    output = Path("/run/helmr/compose.json")
    output.parent.mkdir(parents=True, exist_ok=True)
    temporary = output.with_suffix(".tmp")
    temporary.write_text(json.dumps(escape_compose(compose)))
    temporary.replace(output)


if __name__ == "__main__":
    main()
