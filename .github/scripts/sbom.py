#!/usr/bin/env python3
"""Writes a CycloneDX list of what went into a Go binary, from what the binary itself records.

    sbom.py BINARY VERSION > moorage-sbom.cdx.json
"""
import json
import subprocess
import sys


def main():
    binary, version = sys.argv[1:]
    said = subprocess.run(["go", "version", "-m", binary], check=True, capture_output=True, text=True).stdout
    components = {}
    go = None
    for line in said.splitlines():
        fields = line.split()
        if not line.startswith("\t"):
            go = fields[1] if len(fields) > 1 else None
        elif fields[0] == "dep" and len(fields) >= 3:
            components[fields[1]] = fields[2]
        elif fields[0] == "=>" and len(fields) >= 3:
            # A module replaced by another: it is the other that went in. The last dep is the one replaced.
            del components[list(components)[-1]]
            components[fields[1]] = fields[2]
    if not components or not go:
        raise SystemExit(f"{binary} says nothing of what went into it")
    purl = "pkg:golang/github.com/robert-mcdermott/moorage@v" + version
    bom = {
        "bomFormat": "CycloneDX",
        "specVersion": "1.5",
        "version": 1,
        "metadata": {
            "component": {"type": "application", "name": "moorage", "version": version, "bom-ref": purl, "purl": purl,
                          "licenses": [{"license": {"id": "Apache-2.0"}}]},
            "properties": [{"name": "go", "value": go}],
        },
        "components": [
            {"type": "library", "name": name, "version": v, "bom-ref": f"pkg:golang/{name}@{v}", "purl": f"pkg:golang/{name}@{v}"}
            for name, v in sorted(components.items())
        ],
    }
    json.dump(bom, sys.stdout, indent=2)
    print()


main()
