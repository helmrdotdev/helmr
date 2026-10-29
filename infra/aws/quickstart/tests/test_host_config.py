#!/usr/bin/env python3
"""Check credentials and localhost boundaries in the rendered host composition."""
import base64
import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("quickstart_config", Path(__file__).parents[1] / "host/configure.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


def fixture():
    config = {
        "app_origin": "https://helmr.example.test", "slack_transport": "socket",
        "aws_region": "us-east-1", "region_id": "us-east-1", "worker_group_name": "default",
        "cas_uri": "s3://test-cas", "platform_store_uri": "s3://test-platform/objects",
        "github_oauth_client_id": "test-client", "private_business_cidrs": [],
        "blocked_ipv4_cidrs": ["10.80.0.0/16", "169.254.0.0/16", "203.0.113.10/32"],
        "controlplane_image": "ghcr.io/helmrdotdev/control-plane@sha256:" + "a" * 64,
        "postgres_image": "postgres:18-bookworm@sha256:5c4783a8cc3f0074912164f94dfa1be4e043438f0f707cf968d7af9a0d31a2c8", "clickhouse_image": "clickhouse/clickhouse-server:26.7.5.10@sha256:800e82865530eb2f1c4bc1b960e43b435fd9b2d83b4bd04a2564a5cfd88fdb6e",
        "caddy_image": "caddy:2@sha256:5e5286cb37d44086d651a5363f05f43a692ce4d649d88fb1bd844579f45fa9e8", "controlplane_environment": {}, "dispatcher_environment": {},
    }
    credentials = {key: key + "-database-test-password" for key in (
        "postgres", "helmr_app", "helmr_migration", "helmr_audit", "helmr_admin", "helmr_reader", "helmr_ingester")}
    key = base64.b64encode(b"q" * 32).decode()
    platform = {name: key for name in ("auth_key", "encryption_key", "computer_fencing_key", "computer_wrapping_key", "worker_host_credential_signing_key")}
    platform.update({"github_oauth_client_secret": "literal-$UNSET-'quote'",
                     "capacity_token": base64.urlsafe_b64encode(b"c" * 32).decode().rstrip("="),
                     "worker_enrollment_token": "hlmr_wgt_" + base64.urlsafe_b64encode(b"w" * 32).decode().rstrip("=")})
    return config, credentials, platform


class HostConfigTests(unittest.TestCase):
    def test_serving_processes_do_not_receive_bootstrap_or_migration_authority(self):
        services = module.compose_config(*fixture())["services"]
        for name in ("controlplane", "dispatcher"):
            env = services[name]["environment"]
            self.assertIn("helmr_app:", env["DATABASE_URL"])
            self.assertFalse({"DATABASE_ADMIN_PASSWORD", "DATABASE_MIGRATION_URL", "DATABASE_AUDIT_MAINTENANCE_URL", "CLICKHOUSE_BOOTSTRAP_PASSWORD"} & env.keys())
            self.assertNotIn("SLACK_BOT_TOKEN", env)
        self.assertNotIn("CAPACITY_TOKEN", services["dispatcher"]["environment"])
        self.assertEqual(services["controlplane"]["environment"]["CLICKHOUSE_USER"], "helmr_reader")
        self.assertEqual(services["dispatcher"]["environment"]["CLICKHOUSE_USER"], "helmr_ingester")
        self.assertIn("DATABASE_AUDIT_MAINTENANCE_URL", services["audit-purge"]["environment"])

    def test_stateful_services_and_control_bind_only_to_loopback(self):
        services = module.compose_config(*fixture())["services"]
        self.assertIn("listen_addresses=127.0.0.1", services["postgres"]["command"])
        self.assertIn("ssl=on", services["postgres"]["command"])
        self.assertEqual(services["controlplane"]["environment"]["CONTROL_PLANE_ADDR"], "127.0.0.1:8080")
        self.assertTrue(all(service["network_mode"] == "host" for service in services.values()))
        self.assertTrue(all("ports" not in service for service in services.values()))
        self.assertTrue(any(volume.endswith(":/etc/clickhouse-server/config.d/docker_related_config.xml:ro")
                            for volume in services["clickhouse"]["volumes"]))
        self.assertTrue(any(volume.endswith(":/var/lib/postgresql") for volume in services["postgres"]["volumes"]))
        self.assertTrue(all(services[name]["profiles"] == ["setup"] for name in ("database-bootstrap", "clickhouse-bootstrap", "migrate", "audit-purge")))

    def test_nonsecret_overrides_cannot_replace_credential_or_network_wiring(self):
        config, credentials, platform = fixture()
        config["controlplane_environment"] = {"DEPLOYMENT_UPLOAD_PROCESS_CONCURRENCY": "2"}
        self.assertEqual(module.compose_config(config, credentials, platform)["services"]["controlplane"]["environment"]["DEPLOYMENT_UPLOAD_PROCESS_CONCURRENCY"], "2")
        for key in ("DATABASE_URL", "CONTROL_PLANE_ADDR", "CONTROL_PLANE_NETWORK_BLOCKED_IPV4_CIDRS", "CAPACITY_TOKEN"):
            config["controlplane_environment"] = {key: "override"}
            with self.assertRaises(ValueError):
                module.compose_config(config, credentials, platform)

    def test_literal_dollars_survive_compose_serialization(self):
        config = module.escape_compose(module.compose_config(*fixture()))
        self.assertEqual(config["services"]["controlplane"]["environment"]["GITHUB_OAUTH_CLIENT_SECRET"], "literal-$$UNSET-'quote'")


if __name__ == "__main__":
    unittest.main()
