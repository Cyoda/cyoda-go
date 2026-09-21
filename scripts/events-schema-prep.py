#!/usr/bin/env python3
"""Pre-processes a copy of the event schema tree for Go code generation.

Usage: events-schema-prep.py <dir>

<dir> is a scratch copy of docs/cyoda/schema; the files are rewritten in
place. Called by scripts/generate-events.sh, and exercised directly by
scripts/generate-events_test.sh.
"""

import glob
import json
import os
import re
import sys


def main(clean):
    # Step 1: Load BaseEvent properties and required fields.
    base_event_path = os.path.join(clean, 'common', 'BaseEvent.json')
    with open(base_event_path) as fh:
        base_event = json.load(fh)
    base_props = base_event.get('properties', {})
    base_required = base_event.get('required', [])

    # Step 2: Process all schemas.
    for f in glob.glob(os.path.join(clean, '**', '*.json'), recursive=True):
        with open(f) as fh:
            content = fh.read()

        # Fix Java-specific 'type': 'any' (not valid JSON Schema).
        content = re.sub(r',?\s*"existingJavaType":\s*"[^"]*"', '', content)
        content = content.replace('"type": "any"', '"description": "arbitrary JSON"')
        content = re.sub(r',(\s*[}\]])', r'\1', content)

        # Step 3: Inline BaseEvent's fields into the schemas that compose with
        # it. go-jsonschema resolves the allOf itself, but into a different
        # shape: BaseEvent declared twice, and the per-schema error object
        # collapsed into one type named after whichever schema was read first —
        # a rename of public wire types. Inline here instead, and keep the
        # generated output stable.
        try:
            schema = json.loads(content)
        except json.JSONDecodeError:
            with open(f, 'w') as fh:
                fh.write(content)
            continue

        composed = schema.get('allOf', [])
        base_members = [
            m for m in composed
            if isinstance(m, dict) and 'BaseEvent.json' in m.get('$ref', '')
        ]
        if base_members:
            # Merge BaseEvent properties into this schema.
            props = schema.get('properties', {})
            for k, v in base_props.items():
                if k not in props:
                    props[k] = v
            schema['properties'] = props

            # Merge required fields.
            req = schema.get('required', [])
            for r_field in base_required:
                if r_field not in req:
                    req.append(r_field)
            schema['required'] = req

            # Drop only the members just inlined. Anything else the schema
            # composes with stays in the list for the generator to resolve —
            # removing the whole list would silently lose it from the types.
            rest = [m for m in composed if m not in base_members]
            if rest:
                schema['allOf'] = rest
            else:
                del schema['allOf']

            content = json.dumps(schema, indent=2)

        with open(f, 'w') as fh:
            fh.write(content)


if __name__ == '__main__':
    if len(sys.argv) != 2:
        sys.exit('usage: events-schema-prep.py <dir>')
    main(sys.argv[1])
