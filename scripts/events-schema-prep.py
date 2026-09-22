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
import sys


def strip_existing_java_type(node):
    """Removes every `existingJavaType` key, at any depth.

    It is a jsonschema2pojo extension naming the Java class that tool emits
    for a property. go-jsonschema does not read it, and Cloud's generator
    needs it, so it stays in the published tree and is dropped from this
    scratch copy.

    Structural rather than textual: it used to be a regex that took the comma
    in front of the key, which held only while `"type": "any"` was always
    there to put one there.
    """
    if isinstance(node, dict):
        node.pop('existingJavaType', None)
        for value in node.values():
            strip_existing_java_type(value)
    elif isinstance(node, list):
        for value in node:
            strip_existing_java_type(value)


def main(clean):
    # Step 1: Load BaseEvent properties and required fields.
    base_event_path = os.path.join(clean, 'common', 'BaseEvent.json')
    with open(base_event_path) as fh:
        base_event = json.load(fh)
    strip_existing_java_type(base_event)
    base_props = base_event.get('properties', {})
    base_required = base_event.get('required', [])

    # Step 2: Process all schemas.
    for f in glob.glob(os.path.join(clean, '**', '*.json'), recursive=True):
        with open(f) as fh:
            schema = json.load(fh)

        strip_existing_java_type(schema)

        # Step 3: Inline BaseEvent's fields into the schemas that compose with
        # it. go-jsonschema resolves the allOf itself, but into a different
        # shape: BaseEvent declared twice, and the per-schema error object
        # collapsed into one type named after whichever schema was read first —
        # a rename of public wire types. Inline here instead, and keep the
        # generated output stable.
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

        with open(f, 'w') as fh:
            json.dump(schema, fh, indent=2)


if __name__ == '__main__':
    if len(sys.argv) != 2:
        sys.exit('usage: events-schema-prep.py <dir>')
    main(sys.argv[1])
