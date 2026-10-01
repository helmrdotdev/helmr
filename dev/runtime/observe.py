#!/usr/bin/env python3
"""Render fixed, read-only verification observations from typed JSON inputs.

The caller executes the returned psql input against this exact Product revision.
No connection credentials, SQL text, file names or query fragments are inputs.
"""
import argparse
import json
from pathlib import Path
import re

UUID = re.compile(r'[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}')


def identifier(value):
    return isinstance(value, str) and UUID.fullmatch(value) is not None


def text(value):
    return isinstance(value, str) and 0 < len(value) <= 512 and all(ord(c) >= 32 for c in value)


def identifiers(value):
    return isinstance(value, list) and 0 < len(value) <= 100 and all(identifier(v) for v in value) and len(set(value)) == len(value)


def resources(value):
    return isinstance(value, list) and 0 < len(value) <= 100 and all(text(v) for v in value) and len(set(value)) == len(value)


def age(value):
    return type(value) is int and 1 <= value <= 3600


INPUTS = {
    'worker-fleet': {'region': text},
    'capacity-state': {'region': text, 'max_age_seconds': age},
    'clock': {},
    'run-placements': {'run_ids': identifiers},
    'worker-state': {'resource_ids': resources},
    'computer-state': {'computer_id': identifier},
    'computer-path': {'computer_id': identifier},
    'run-state': {'run_id': identifier},
    'run-path': {'run_id': identifier},
    'deployment-state': {'project': text, 'environment': text},
    'api-key-scope': {'key_prefix': text, 'project': text, 'environment': text},
}


def render(name, values):
    if name not in INPUTS or not isinstance(values, dict) or values.keys() != INPUTS[name].keys():
        raise ValueError('unknown observation or unexpected input fields')
    if any(not validate(values[key]) for key, validate in INPUTS[name].items()):
        raise ValueError('invalid observation input')
    # Quote a single JSON value, never SQL fragments or identifiers.
    literal = json.dumps(values, ensure_ascii=True).replace("\\", "\\\\").replace("'", "''")
    query = (Path(__file__).with_name('observations') / (name + '.sql')).read_text()
    return ("BEGIN READ ONLY;\nSET LOCAL statement_timeout='5s';\n"
            "SET LOCAL lock_timeout='1s';\nSET LOCAL idle_in_transaction_session_timeout='10s';\n"
            "WITH input AS (SELECT E'" + literal + "'::jsonb AS args)\n" + query + "\nCOMMIT;\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('observation', choices=INPUTS)
    parser.add_argument('inputs', help='JSON object with exactly the named observation inputs')
    args = parser.parse_args()
    try:
        print(render(args.observation, json.loads(args.inputs)), end='')
    except (ValueError, TypeError) as exc:
        parser.error(str(exc))


if __name__ == '__main__':
    main()
