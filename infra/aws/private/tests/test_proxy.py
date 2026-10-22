#!/usr/bin/env python3
"""Qualify the exact maintained CONNECT proxy in an isolated Docker network."""
import json
from pathlib import Path
import socket
import ssl
import subprocess
import tempfile
import time
import uuid

ROOT = Path(__file__).resolve().parents[1]
PIN = json.loads((ROOT / "proxy-image.json").read_text())
IMAGE = PIN["image"]


def docker(*args):
    return subprocess.check_output(["docker", *args], text=True, stderr=subprocess.STDOUT).strip()


def connect(port, authority, method="CONNECT"):
    stream = socket.create_connection(("127.0.0.1", port), timeout=4)
    stream.sendall(f"{method} {authority} HTTP/1.1\r\nHost: {authority}\r\n\r\n".encode())
    header = b""
    while b"\r\n\r\n" not in header:
        part = stream.recv(1)
        if not part:
            raise AssertionError(f"missing proxy response: {header!r}")
        header += part
    return stream, int(header.split()[1])


def main():
    name = "helmr-proxy-test-" + uuid.uuid4().hex[:10]
    proxy, upstream = name + "-proxy", name + "-upstream"
    docker("info", "--format", "{{.OSType}}")
    # Only test-owned containers are attached. Every approved test name maps to a fixture; no provider request is made.
    docker("network", "create", "--subnet", "203.0.113.0/24", name)
    try:
        with tempfile.TemporaryDirectory(prefix="helmr-proxy-") as tmp:
            config = (ROOT / "templates/squid.conf.tftpl").read_text().replace(
                '${join(" ", domains)}', "api.github.com slack.com wss-primary.slack.com"
            )
            path = Path(tmp) / "squid.conf"
            path.write_text(config)
            path.chmod(0o644)
            server = (
                "openssl req -x509 -newkey rsa:2048 -nodes -keyout /tmp/server.key "
                "-out /tmp/server.crt -days 1 -subj /CN=api.github.com >/dev/null 2>&1; "
                "exec openssl s_server -accept 443 -cert /tmp/server.crt "
                "-key /tmp/server.key -www -quiet"
            )
            docker("run", "-d", "--name", upstream, "--network", name, "--ip", "203.0.113.20",
                   "--entrypoint", "/bin/sh", IMAGE, "-ec", server)
            docker("run", "-d", "--name", proxy, "--network", name, "--ip", "203.0.113.10",
                   "--user", "13:13", "--read-only", "--cap-drop", "ALL", "--tmpfs", "/tmp:mode=1777",
                   "--add-host", "api.github.com:203.0.113.20", "--add-host", "wss-primary.slack.com:203.0.113.20",
                   "--add-host", "slack.com:127.0.0.1", "-p", "127.0.0.1::3128",
                   "-v", f"{path}:/etc/helmr-squid.conf:ro", "--entrypoint", "/usr/sbin/squid",
                   IMAGE, "-N", "-f", "/etc/helmr-squid.conf")
            port = int(json.loads(docker("inspect", proxy))[0]["NetworkSettings"]["Ports"]["3128/tcp"][0]["HostPort"])
            deadline = time.monotonic() + 15
            while True:
                try:
                    stream, status = connect(port, "api.github.com:443")
                    if status != 200:
                        stream.close()
                        raise AssertionError(f"allowed CONNECT returned {status}")
                    break
                except (OSError, AssertionError):
                    if time.monotonic() >= deadline:
                        raise
                    time.sleep(0.2)
            # The client retains TLS: the proxy cannot read or rewrite app bytes.
            context = ssl._create_unverified_context()  # Test-owned certificate.
            with context.wrap_socket(stream, server_hostname="api.github.com") as tls:
                tls.sendall(b"GET / HTTP/1.0\r\n\r\n")
                assert b"200" in tls.recv(4096), "TLS tunnel did not reach fixture"
            stream, status = connect(port, "wss-primary.slack.com:443")
            assert status == 200, "Socket Mode host was blocked"
            with context.wrap_socket(stream, server_hostname="wss-primary.slack.com") as tls:
                tls.sendall(b"GET / HTTP/1.0\r\n\r\n")
                assert b"200" in tls.recv(4096)
            for authority, method in [
                ("unapproved.example:443", "CONNECT"),
                ("api.github.com:80", "CONNECT"),
                ("203.0.113.20:443", "CONNECT"),
                ("127.0.0.1:443", "CONNECT"),
                ("169.254.169.254:443", "CONNECT"),
                ("slack.com:443", "CONNECT"),  # Approved name with private answer.
                ("http://api.github.com/private-sentinel", "GET"),
            ]:
                stream, status = connect(port, authority, method)
                stream.close()
                assert status == 403, (authority, method, status)
            logs = docker("logs", proxy)
            assert "private-sentinel" not in logs, "request path leaked to proxy logs"
            version = docker("exec", proxy, "/usr/sbin/squid", "-v")
            assert f"Version {PIN['squid_version']}" in version
            print("PASS: pinned Squid CONNECT/TLS, approved Socket host, destination/port/private-address denials, redacted logs")
    except Exception:
        try:
            print(docker("logs", proxy))
        except subprocess.CalledProcessError:
            pass
        raise
    finally:
        for container in (proxy, upstream):
            subprocess.run(["docker", "rm", "-f", container], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        subprocess.run(["docker", "network", "rm", name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True)


if __name__ == "__main__":
    main()
