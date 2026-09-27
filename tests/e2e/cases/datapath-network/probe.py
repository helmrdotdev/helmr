#!/usr/bin/env python3
"""Bounded, one-shot network probes for isolated datapath validation."""

from __future__ import annotations

import errno
import ipaddress
import json
import os
import socket
import struct
import sys
import time
from typing import Any


MODES = {"tcp", "udp", "icmp", "dns", "raw-ip", "raw-mac", "ipv6", "hold"}
MAX_ATTEMPTS = 32
FIXED_PAYLOAD = b"helmr-datapath-v0"
ICMP_IDENTIFIER = 0x484D


class ProbeError(Exception):
    pass


def checksum(data: bytes) -> int:
    if len(data) % 2:
        data += b"\x00"
    total = sum(struct.unpack(f"!{len(data) // 2}H", data))
    total = (total >> 16) + (total & 0xFFFF)
    total += total >> 16
    return (~total) & 0xFFFF


def require_root(mode: str) -> None:
    if os.geteuid() != 0:
        raise ProbeError(f"{mode} requires root with CAP_NET_RAW")


def parse_mac(value: str) -> bytes:
    parts = value.split(":")
    if len(parts) != 6:
        raise ProbeError("invalid MAC address")
    try:
        raw = bytes(int(part, 16) for part in parts)
    except ValueError as exc:
        raise ProbeError("invalid MAC address") from exc
    if len(raw) != 6:
        raise ProbeError("invalid MAC address")
    return raw


def require_string(config: dict[str, Any], key: str) -> str:
    value = config.get(key)
    if not isinstance(value, str) or not value:
        raise ProbeError(f"{key} is required")
    return value


def require_int(
    config: dict[str, Any], key: str, minimum: int, maximum: int, default: int
) -> int:
    value = config.get(key, default)
    if isinstance(value, bool) or not isinstance(value, int):
        raise ProbeError(f"{key} must be an integer")
    if value < minimum or value > maximum:
        raise ProbeError(f"{key} must be between {minimum} and {maximum}")
    return value


def receive_exact(sock: socket.socket, expected: bytes) -> bool:
    reply = sock.recv(512)
    return reply == expected


def tcp_probe(config: dict[str, Any], timeout: float) -> dict[str, Any]:
    target = require_string(config, "target")
    port = require_int(config, "port", 1, 65535, 0)
    addresses = socket.getaddrinfo(
        target,
        port,
        family=socket.AF_INET,
        type=socket.SOCK_STREAM,
        proto=socket.IPPROTO_TCP,
    )
    if not addresses:
        raise ProbeError("target has no IPv4 TCP address")
    family, socktype, proto, _, address = addresses[0]
    with socket.socket(family, socktype, proto) as sock:
        sock.settimeout(timeout)
        sock.connect(address)
        source = sock.getsockname()
        destination = sock.getpeername()
        return {
            "protocol": "tcp",
            "sourceAddress": source[0],
            "sourcePort": source[1],
            "destinationAddress": destination[0],
            "destinationPort": destination[1],
        }


def udp_probe(config: dict[str, Any], timeout: float) -> bool:
    target = require_string(config, "target")
    port = require_int(config, "port", 1, 65535, 0)
    expect_reply = config.get("expectReply", False)
    if not isinstance(expect_reply, bool):
        raise ProbeError("expectReply must be a boolean")
    family, socktype, proto, _, address = socket.getaddrinfo(
        target, port, type=socket.SOCK_DGRAM
    )[0]
    with socket.socket(family, socktype, proto) as sock:
        sock.settimeout(timeout)
        sock.sendto(FIXED_PAYLOAD, address)
        return receive_exact(sock, FIXED_PAYLOAD) if expect_reply else True


def icmp_packet(sequence: int) -> bytes:
    header = struct.pack("!BBHHH", 8, 0, 0, ICMP_IDENTIFIER, sequence)
    return struct.pack(
        "!BBHHH", 8, 0, checksum(header + FIXED_PAYLOAD), ICMP_IDENTIFIER, sequence
    ) + FIXED_PAYLOAD


def icmp_probe(config: dict[str, Any], timeout: float, sequence: int) -> bool:
    require_root("icmp")
    target = require_string(config, "target")
    destination = socket.gethostbyname(target)
    with socket.socket(socket.AF_INET, socket.SOCK_RAW, socket.IPPROTO_ICMP) as sock:
        sock.settimeout(timeout)
        sock.sendto(icmp_packet(sequence), (destination, 0))
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            packet, _ = sock.recvfrom(256)
            if len(packet) < 28:
                continue
            ihl = (packet[0] & 0x0F) * 4
            if len(packet) < ihl + 8:
                continue
            kind, _, _, identifier, reply_sequence = struct.unpack(
                "!BBHHH", packet[ihl : ihl + 8]
            )
            if kind == 0 and identifier == ICMP_IDENTIFIER and reply_sequence == sequence:
                return True
        return False


def dns_name(value: str) -> bytes:
    labels = value.rstrip(".").split(".")
    if not labels or any(not label or len(label.encode("ascii")) > 63 for label in labels):
        raise ProbeError("invalid DNS query name")
    encoded = b"".join(bytes([len(label)]) + label.encode("ascii") for label in labels)
    if len(encoded) > 253:
        raise ProbeError("DNS query name is too long")
    return encoded + b"\x00"


def dns_probe(config: dict[str, Any], timeout: float, sequence: int) -> bool:
    target = require_string(config, "target")
    port = require_int(config, "port", 1, 65535, 53)
    query_name = config.get("queryName", "datapath-validation.invalid")
    if not isinstance(query_name, str):
        raise ProbeError("queryName must be a string")
    transport = config.get("transport", "udp")
    if transport not in {"udp", "tcp"}:
        raise ProbeError("transport must be udp or tcp")
    query_id = (ICMP_IDENTIFIER + sequence) & 0xFFFF
    query = (
        struct.pack("!HHHHHH", query_id, 0x0100, 1, 0, 0, 0)
        + dns_name(query_name)
        + struct.pack("!HH", 1, 1)
    )
    socktype = socket.SOCK_DGRAM if transport == "udp" else socket.SOCK_STREAM
    with socket.socket(socket.AF_INET, socktype) as sock:
        sock.settimeout(timeout)
        if transport == "udp":
            sock.sendto(query, (target, port))
            reply = sock.recv(512)
        else:
            sock.connect((target, port))
            sock.sendall(struct.pack("!H", len(query)) + query)
            length = sock.recv(2)
            if len(length) != 2:
                return False
            expected = struct.unpack("!H", length)[0]
            reply = b""
            while len(reply) < expected:
                chunk = sock.recv(min(512, expected - len(reply)))
                if not chunk:
                    return False
                reply += chunk
        return (
            len(reply) >= 12
            and struct.unpack("!H", reply[:2])[0] == query_id
            and (struct.unpack("!H", reply[2:4])[0] & 0x8000) != 0
        )


def ipv4_header(source: str, destination: str, body: bytes, sequence: int) -> bytes:
    source_bytes = ipaddress.IPv4Address(source).packed
    destination_bytes = ipaddress.IPv4Address(destination).packed
    header = struct.pack(
        "!BBHHHBBH4s4s",
        0x45,
        0,
        20 + len(body),
        sequence & 0xFFFF,
        0,
        64,
        socket.IPPROTO_ICMP,
        0,
        source_bytes,
        destination_bytes,
    )
    return (
        struct.pack(
            "!BBHHHBBH4s4s",
            0x45,
            0,
            20 + len(body),
            sequence & 0xFFFF,
            0,
            64,
            socket.IPPROTO_ICMP,
            checksum(header),
            source_bytes,
            destination_bytes,
        )
        + body
    )


def raw_ip_probe(config: dict[str, Any], sequence: int) -> bool:
    require_root("raw-ip")
    source = str(ipaddress.IPv4Address(require_string(config, "sourceAddress")))
    destination = str(ipaddress.IPv4Address(require_string(config, "target")))
    packet = ipv4_header(source, destination, icmp_packet(sequence), sequence)
    with socket.socket(socket.AF_INET, socket.SOCK_RAW, socket.IPPROTO_RAW) as sock:
        sock.setsockopt(socket.IPPROTO_IP, socket.IP_HDRINCL, 1)
        return sock.sendto(packet, (destination, 0)) == len(packet)


def raw_mac_probe(config: dict[str, Any], sequence: int) -> bool:
    require_root("raw-mac")
    interface = require_string(config, "interface")
    source_mac = parse_mac(require_string(config, "sourceMac"))
    destination_mac = parse_mac(require_string(config, "destinationMac"))
    source = str(ipaddress.IPv4Address(require_string(config, "sourceAddress")))
    destination = str(ipaddress.IPv4Address(require_string(config, "target")))
    body = ipv4_header(source, destination, icmp_packet(sequence), sequence)
    frame = destination_mac + source_mac + struct.pack("!H", 0x0800) + body
    with socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(0x0003)) as sock:
        sock.bind((interface, 0))
        return sock.send(frame) == len(frame)


def ipv6_probe(config: dict[str, Any], sequence: int) -> bool:
    require_root("ipv6")
    interface = require_string(config, "interface")
    source_mac = parse_mac(require_string(config, "sourceMac"))
    destination_mac = parse_mac(require_string(config, "destinationMac"))
    source = ipaddress.IPv6Address(require_string(config, "sourceAddress"))
    destination = ipaddress.IPv6Address(require_string(config, "target"))
    icmp_body = struct.pack("!BBHHH", 128, 0, 0, ICMP_IDENTIFIER, sequence) + FIXED_PAYLOAD
    pseudo = (
        source.packed
        + destination.packed
        + struct.pack("!I3xB", len(icmp_body), socket.IPPROTO_ICMPV6)
    )
    icmp_body = (
        struct.pack(
            "!BBHHH",
            128,
            0,
            checksum(pseudo + icmp_body),
            ICMP_IDENTIFIER,
            sequence,
        )
        + FIXED_PAYLOAD
    )
    ipv6_header = struct.pack(
        "!IHBB16s16s",
        6 << 28,
        len(icmp_body),
        socket.IPPROTO_ICMPV6,
        64,
        source.packed,
        destination.packed,
    )
    frame = destination_mac + source_mac + struct.pack("!H", 0x86DD) + ipv6_header + icmp_body
    with socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(0x0003)) as sock:
        sock.bind((interface, 0))
        return sock.send(frame) == len(frame)


def run(config: dict[str, Any]) -> dict[str, Any]:
    unknown = set(config) - {
        "mode",
        "target",
        "port",
        "interface",
        "sourceAddress",
        "sourceMac",
        "destinationMac",
        "attempts",
        "intervalMs",
        "timeoutMs",
        "holdMs",
        "expectReply",
        "queryName",
        "transport",
    }
    if unknown:
        raise ProbeError(f"unknown configuration member: {sorted(unknown)[0]}")
    mode = config.get("mode")
    if mode not in MODES:
        raise ProbeError("unsupported mode")
    attempts = require_int(config, "attempts", 1, MAX_ATTEMPTS, 1)
    interval_ms = require_int(config, "intervalMs", 0, 1000, 50)
    timeout_ms = require_int(config, "timeoutMs", 50, 10000, 1000)
    if mode == "hold":
        hold_ms = require_int(config, "holdMs", 1, 300000, 1000)
        time.sleep(hold_ms / 1000)
        return {"schema": "helmrdotdev.datapath-probe-result.v0", "mode": mode, "attempts": []}

    results: list[dict[str, Any]] = []
    for index in range(attempts):
        started = time.monotonic()
        flow: dict[str, Any] | None = None
        try:
            if mode == "tcp":
                flow = tcp_probe(config, timeout_ms / 1000)
                observed = True
            elif mode == "udp":
                observed = udp_probe(config, timeout_ms / 1000)
            elif mode == "icmp":
                observed = icmp_probe(config, timeout_ms / 1000, index + 1)
            elif mode == "dns":
                observed = dns_probe(config, timeout_ms / 1000, index + 1)
            elif mode == "raw-ip":
                observed = raw_ip_probe(config, index + 1)
            elif mode == "raw-mac":
                observed = raw_mac_probe(config, index + 1)
            else:
                observed = ipv6_probe(config, index + 1)
            outcome = "observed" if observed else "no-response"
            error_name = None
        except (TimeoutError, socket.timeout):
            outcome = "timeout"
            error_name = "ETIMEDOUT"
        except OSError as exc:
            outcome = "os-error"
            error_name = errno.errorcode.get(exc.errno or 0, "UNKNOWN")
        attempt = {
            "sequence": index + 1,
            "outcome": outcome,
            "errno": error_name,
            "elapsedMs": min(60000, int((time.monotonic() - started) * 1000)),
        }
        if flow is not None:
            attempt["flow"] = flow
        results.append(attempt)
        if index + 1 < attempts and interval_ms:
            time.sleep(interval_ms / 1000)
    return {
        "schema": "helmrdotdev.datapath-probe-result.v0",
        "mode": mode,
        "attempts": results,
    }


def main() -> int:
    if len(sys.argv) != 2:
        print("usage: datapath-network-probe.py '<json>'", file=sys.stderr)
        return 2
    try:
        config = json.loads(sys.argv[1])
        if not isinstance(config, dict):
            raise ProbeError("configuration must be a JSON object")
        result = run(config)
    except (ProbeError, ValueError, UnicodeError) as exc:
        print(f"datapath probe rejected: {exc}", file=sys.stderr)
        return 2
    print(json.dumps(result, separators=(",", ":"), sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
