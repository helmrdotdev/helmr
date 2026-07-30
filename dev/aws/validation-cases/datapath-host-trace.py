#!/usr/bin/env python3
"""Capture bounded packet headers from exact Linux interfaces."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import selectors
import signal
import socket
import struct
import time
from pathlib import Path
from typing import Any


PACKET_TYPES = {
    0: "host",
    1: "broadcast",
    2: "multicast",
    3: "other-host",
    4: "outgoing",
}
MAX_CAPTURED_HEADER = 128
stopping = False


def stop_handler(_signum: int, _frame: object) -> None:
    global stopping
    stopping = True


def exclusive_file(path: Path) -> int:
    return os.open(
        path,
        os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_CLOEXEC | os.O_NOFOLLOW,
        0o600,
    )


def write_exclusive_json(path: Path, value: dict[str, Any]) -> None:
    raw = json.dumps(value, separators=(",", ":"), sort_keys=True).encode() + b"\n"
    fd = exclusive_file(path)
    try:
        os.write(fd, raw)
        os.fsync(fd)
    finally:
        os.close(fd)


def mac(raw: bytes) -> str:
    return ":".join(f"{part:02x}" for part in raw)


def parse_transport(event: dict[str, Any], frame: bytes, offset: int, protocol: int) -> None:
    if protocol in (6, 17) and len(frame) >= offset + 4:
        source, destination = struct.unpack("!HH", frame[offset : offset + 4])
        event["source_port"] = source
        event["destination_port"] = destination
    elif protocol in (1, 58) and len(frame) >= offset + 2:
        event["icmp_type"] = frame[offset]
        event["icmp_code"] = frame[offset + 1]


def parse_frame(
    interface: str,
    ifindex: int,
    packet_type: int,
    wire_length: int,
    frame: bytes,
) -> dict[str, Any]:
    event: dict[str, Any] = {
        "timestamp_ns": time.time_ns(),
        "interface": interface,
        "ifindex": ifindex,
        "packet_type": PACKET_TYPES.get(packet_type, f"unknown-{packet_type}"),
        "wire_length": wire_length,
        "captured_header_bytes": len(frame),
        "observation_point": "af-packet",
    }
    if len(frame) < 14:
        event["malformed"] = "short-ethernet"
        return event
    event["destination_mac"] = mac(frame[0:6])
    event["source_mac"] = mac(frame[6:12])
    ethertype = struct.unpack("!H", frame[12:14])[0]
    offset = 14
    if ethertype in (0x8100, 0x88A8) and len(frame) >= 18:
        event["vlan_tci"] = struct.unpack("!H", frame[14:16])[0]
        ethertype = struct.unpack("!H", frame[16:18])[0]
        offset = 18
    event["ethertype"] = f"0x{ethertype:04x}"
    if ethertype == 0x0800 and len(frame) >= offset + 20:
        ihl = (frame[offset] & 0x0F) * 4
        if ihl < 20 or len(frame) < offset + ihl:
            event["malformed"] = "invalid-ipv4-header"
            return event
        protocol = frame[offset + 9]
        event["ip_version"] = 4
        event["source_ip"] = socket.inet_ntop(socket.AF_INET, frame[offset + 12 : offset + 16])
        event["destination_ip"] = socket.inet_ntop(
            socket.AF_INET, frame[offset + 16 : offset + 20]
        )
        event["ip_protocol"] = protocol
        event["ip_identifier"] = struct.unpack("!H", frame[offset + 4 : offset + 6])[0]
        parse_transport(event, frame, offset + ihl, protocol)
    elif ethertype == 0x86DD and len(frame) >= offset + 40:
        protocol = frame[offset + 6]
        event["ip_version"] = 6
        event["source_ip"] = socket.inet_ntop(socket.AF_INET6, frame[offset + 8 : offset + 24])
        event["destination_ip"] = socket.inet_ntop(
            socket.AF_INET6, frame[offset + 24 : offset + 40]
        )
        event["ip_protocol"] = protocol
        parse_transport(event, frame, offset + 40, protocol)
    elif ethertype == 0x0806 and len(frame) >= offset + 8:
        event["arp_operation"] = struct.unpack("!H", frame[offset + 6 : offset + 8])[0]
    return event


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--summary", required=True, type=Path)
    parser.add_argument("--ready", required=True, type=Path)
    parser.add_argument("--stop", required=True, type=Path)
    parser.add_argument("--interface", action="append", required=True)
    parser.add_argument("--duration", required=True, type=int)
    parser.add_argument("--max-events", type=int, default=256)
    parser.add_argument("--max-bytes", type=int, default=256 * 1024)
    args = parser.parse_args()
    if not 1 <= args.duration <= 900:
        parser.error("duration must be between 1 and 900 seconds")
    if not 1 <= args.max_events <= 256:
        parser.error("max-events must be between 1 and 256")
    if not 1024 <= args.max_bytes <= 256 * 1024:
        parser.error("max-bytes must be between 1024 and 262144")
    if len(set(args.interface)) != len(args.interface):
        parser.error("interfaces must be unique")
    return args


def main() -> int:
    args = parse_args()
    signal.signal(signal.SIGINT, stop_handler)
    signal.signal(signal.SIGTERM, stop_handler)
    selector = selectors.DefaultSelector()
    sockets: list[socket.socket] = []
    trace_fd = -1
    events = 0
    trace_bytes = 0
    truncated = False
    trace_hash = hashlib.sha256()
    started_ns = time.time_ns()
    status = "completed"
    error_class: str | None = None
    try:
        trace_fd = exclusive_file(args.output)
        for interface in args.interface:
            ifindex = socket.if_nametoindex(interface)
            packet_socket = socket.socket(
                socket.AF_PACKET, socket.SOCK_RAW, socket.htons(0x0003)
            )
            packet_socket.setblocking(False)
            packet_socket.bind((interface, 0))
            selector.register(packet_socket, selectors.EVENT_READ, (interface, ifindex))
            sockets.append(packet_socket)
        write_exclusive_json(
            args.ready,
            {
                "schema": "helmrdotdev.datapath-trace-ready.v0",
                "interfaces": sorted(args.interface),
                "pid": os.getpid(),
            },
        )
        deadline = time.monotonic() + args.duration
        buffer = bytearray(MAX_CAPTURED_HEADER)
        while not stopping and not args.stop.exists() and time.monotonic() < deadline:
            for key, _ in selector.select(timeout=0.2):
                packet_socket = key.fileobj
                interface, ifindex = key.data
                wire_length, _, _, address = packet_socket.recvmsg_into(
                    [buffer], 0, socket.MSG_TRUNC
                )
                captured = bytes(buffer[: min(wire_length, MAX_CAPTURED_HEADER)])
                packet_type = address[2] if len(address) > 2 else -1
                event = parse_frame(
                    interface, ifindex, packet_type, wire_length, captured
                )
                line = (
                    json.dumps(event, separators=(",", ":"), sort_keys=True).encode()
                    + b"\n"
                )
                if events >= args.max_events or trace_bytes + len(line) > args.max_bytes:
                    truncated = True
                    status = "truncated"
                    break
                os.write(trace_fd, line)
                trace_hash.update(line)
                events += 1
                trace_bytes += len(line)
            if truncated:
                break
        os.fsync(trace_fd)
    except Exception as exc:  # exact class only; never serialize message or packet data
        status = "error"
        error_class = type(exc).__name__
    finally:
        if trace_fd >= 0:
            os.close(trace_fd)
        for packet_socket in sockets:
            try:
                selector.unregister(packet_socket)
            except Exception:
                pass
            packet_socket.close()
        selector.close()
    summary = {
        "schema": "helmrdotdev.datapath-trace-summary.v0",
        "status": status,
        "truncated": truncated,
        "event_count": events,
        "trace_bytes": trace_bytes,
        "trace_sha256": trace_hash.hexdigest(),
        "started_at_ns": started_ns,
        "finished_at_ns": time.time_ns(),
        "error_class": error_class,
    }
    try:
        write_exclusive_json(args.summary, summary)
    except FileExistsError:
        return 1
    if status == "truncated":
        return 3
    return 0 if status == "completed" else 1


if __name__ == "__main__":
    raise SystemExit(main())
